package scraper

import (
	"io"
	"net/url"

	"strings"

	"github.com/PuerkitoBio/goquery"
	"go.uber.org/zap"
)

func (s *Scraper) fixFileReferences(u *url.URL, buf io.Reader) (string, error) {
	g, err := goquery.NewDocumentFromReader(buf)
	if err != nil {
		return "", err
	}

	relativeToRoot := s.urlRelativeToRoot(u)

	g.Find("a").Each(func(_ int, selection *goquery.Selection) {
		s.fixQuerySelection(u, "href", selection, true, relativeToRoot)
	})

	g.Find("link").Each(func(_ int, selection *goquery.Selection) {
		s.fixQuerySelection(u, "href", selection, false, relativeToRoot)
	})

	g.Find("img").Each(func(_ int, selection *goquery.Selection) {
		s.fixQuerySelection(u, "src", selection, false, relativeToRoot)
		s.fixSrcSetSelection(u, selection, relativeToRoot)
	})

	g.Find("script").Each(func(_ int, selection *goquery.Selection) {
		s.fixQuerySelection(u, "src", selection, false, relativeToRoot)
	})

	g.Find("style").Each(func(_ int, selection *goquery.Selection) {
		styleIn := selection.Text()
		styleOut, _ := ProcessStyle(&styleIn, func(urlIn string) *url.URL {
			u, err := u.Parse(urlIn)
			if err != nil {
				u = nil
				return nil
			}
			return s.browser.ResolveUrl(u)
		})
		selection.SetText(*styleOut)
	})

	return g.Html()
}

func (s *Scraper) fixQuerySelection(url *url.URL, attribute string, selection *goquery.Selection,
	linkIsAPage bool, relativeToRoot string) {
	src, ok := selection.Attr(attribute)
	if !ok {
		return
	}

	if strings.HasPrefix(src, "data:") {
		return
	}
	if strings.HasPrefix(src, "mailto:") {
		return
	}

	resolved := s.resolveURL(url, src, linkIsAPage, relativeToRoot)
	if src == resolved { // nothing changed
		return
	}

	if linkIsAPage && s.config.SkipIndexRewrites {
		s.log.Debug("TODO: Better message here")
		if strings.HasSuffix(resolved, "index.html") {
			resolved = strings.ReplaceAll(resolved, "index.html", "")
		}
		selection.SetAttr(attribute, resolved)
	}

	s.log.Debug("HTML Element relinked", zap.String("URL", src), zap.String("Fixed", resolved))
	selection.SetAttr(attribute, resolved)

}

func (s *Scraper) fixSrcSetSelection(
	url *url.URL,
	selection *goquery.Selection,
	relativeToRoot string) {

	srcset, exists := selection.Attr("srcset")

	if !exists {
		return
	}

	lines := strings.Split(srcset,",")
	for i, line := range lines {
		splits := strings.Split(strings.TrimSpace(line), " ")
		splits[0] = s.resolveURL(url, splits[0], false, relativeToRoot)
		lines[i] = strings.Join(splits, " ")
	}
	srcset = strings.Join(lines, ",\n")

	selection.SetAttr("srcset", srcset)
}
