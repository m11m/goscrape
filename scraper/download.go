package scraper

import (
	"bytes"
	"github.com/PuerkitoBio/goquery"
	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/css"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/headzoo/surf/browser"
	"go.uber.org/zap"
)

// assetProcessor is a processor of a downloaded asset that can transform
// a downloaded file content before it will be stored on disk.
type assetProcessor func(URL *url.URL, buf *bytes.Buffer) *bytes.Buffer

func (s *Scraper) downloadReferences() {
	for _, image := range s.browser.Images() {
		s.imagesQueue = append(s.imagesQueue, &image.DownloadableAsset)
	}

	s.imagesQueue = append(s.imagesQueue, s.downloadableAssetsFromSrcSets()...)

	s.imagesQueue = append(s.imagesQueue, s.downloadableAssetsFromBackgroundImagesInStyle()...)

	for _, stylesheet := range s.browser.Stylesheets() {
		s.downloadAsset(&stylesheet.DownloadableAsset, s.checkCSSForUrls)
	}
	for _, script := range s.browser.Scripts() {
		s.downloadAsset(&script.DownloadableAsset, nil)
	}
	for _, image := range s.imagesQueue {
		s.downloadAsset(image, s.checkImageForRecode)
	}
	s.imagesQueue = nil
}

// downloadAsset downloads an asset if it does not exist on disk yet.
func (s *Scraper) downloadAsset(asset *browser.DownloadableAsset, processor assetProcessor) {
	URL := asset.URL
	u := URL.String()
	if _, ok := s.processed[u]; ok {
		return // was already processed
	}
	s.processed[u] = struct{}{}

	if s.includes != nil && !s.isURLIncluded(URL) {
		return
	}
	if s.excludes != nil && s.isURLExcluded(URL) {
		return
	}

	filePath := s.GetFilePath(URL, false)
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		return // exists already on disk
	}

	s.log.Info("Downloading", zap.String("URL", u))

	buf := &bytes.Buffer{}
	_, err := asset.Download(buf)
	if err != nil {
		s.log.Error("Downloading asset failed",
			zap.String("URL", u),
			zap.Error(err))
		return
	}

	if processor != nil {
		buf = processor(URL, buf)
	}

	if err = s.writeFile(filePath, buf); err != nil {
		s.log.Error("Writing asset file failed",
			zap.String("URL", u),
			zap.String("file", filePath),
			zap.Error(err))
	}
}

func (s *Scraper) downloadableAssetsFromSrcSets() []*browser.DownloadableAsset {
	assets := []*browser.DownloadableAsset{}

	s.browser.Find("img[srcset]").Each(func(_ int, selection *goquery.Selection) {
		srcset, ok := selection.Attr("srcset")
		if !ok {
			return
		}

		lines := strings.Split(srcset, ",")

		for _, l := range lines {
			split := strings.Split(strings.TrimSpace(l), " ")

			u, err := url.Parse(split[0])
			if err != nil {
				println(err.Error(), split[0])
				continue
			}

			a := browser.DownloadableAsset{}
			a.URL =	s.browser.ResolveUrl(u)
			a.Type = browser.ImageAsset

			assets = append(assets, &a)
		}
	})

	return assets
}

func (s *Scraper) downloadableAssetsFromBackgroundImagesInStyle() []*browser.DownloadableAsset {
	assets := []*browser.DownloadableAsset{}

	s.browser.Find("style").Each(func(_ int, style *goquery.Selection) {
		styleIn := style.Text()

		println(styleIn)

		_, newAssets := ProcessStyle(&styleIn, func(urlIn string) (u *url.URL) {
			u, err := url.Parse(urlIn)
			if err != nil {
				u = nil
				return
			}
			return s.browser.ResolveUrl(u)
		})
		assets = append(assets, newAssets...)
	})

	return assets
}

func ProcessStyle(styleIn *string, resolveUrl func(urlIn string) *url.URL) (styleOut *string, assets []*browser.DownloadableAsset) {
	assets = []*browser.DownloadableAsset{}

	output := ""
	p := css.NewParser(parse.NewInputString(*styleIn), false)

	for {
		grammar, _, data := p.Next()
		data = parse.Copy(data)
		if grammar == css.ErrorGrammar {
			if err := p.Err(); err != io.EOF {
				data = []byte("ERROR(")
				for _, val := range p.Values() {
					data = append(data, val.Data...)
				}
				data = append(data, ")"...)
			} else {
				break
			}
		} else if grammar == css.AtRuleGrammar || grammar == css.BeginAtRuleGrammar || grammar == css.QualifiedRuleGrammar || grammar == css.BeginRulesetGrammar || grammar == css.DeclarationGrammar || grammar == css.CustomPropertyGrammar {
			if grammar == css.DeclarationGrammar || grammar == css.CustomPropertyGrammar {
				data = append(data, ":"...)
			}
			for _, val := range p.Values() {
				if val.TokenType == css.URLToken {
					a := browser.DownloadableAsset{}
					a.Type = browser.ImageAsset
					a.URL = resolveUrl(string(val.Data[4:len(val.Data)-1]))
					assets = append(assets, &a)
				}
				data = append(data, val.Data...)
			}
			if grammar == css.BeginAtRuleGrammar || grammar == css.BeginRulesetGrammar {
				data = append(data, " {"...)
			} else if grammar == css.AtRuleGrammar || grammar == css.DeclarationGrammar || grammar == css.CustomPropertyGrammar {
				data = append(data, ";"...)
			} else if grammar == css.QualifiedRuleGrammar {
				data = append(data, ","...)
			}
		}
		output += string(append(data, "\n"...))
	}

	return &output, assets
}
