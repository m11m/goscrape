package scraper

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/headzoo/surf"
	"github.com/headzoo/surf/agent"
	"github.com/headzoo/surf/browser"
	"go.uber.org/zap"
)

// Config contains the scraper configuration.
type Config struct {
	URL      string
	Includes []string
	Excludes []string

	ImageQuality uint // image quality from 0 to 100%, 0 to disable reencoding
	MaxDepth     uint // download depth, 0 for unlimited
	Timeout      uint // time limit in seconds to process each http request

	OutputDirectory string
	Username        string
	Password        string
}

// Scraper contains all scraping data.
type Scraper struct {
	config Config
	log    *zap.Logger

	// Configuration
	URL *url.URL

	browser  *browser.Browser
	includes []*regexp.Regexp
	excludes []*regexp.Regexp

	assets         map[string]bool
	imagesQueue    []*browser.DownloadableAsset
	assetsExternal map[string]bool
	pages          map[string]bool
}

// assetProcessor is a processor of a downloaded asset.
type assetProcessor func(URL *url.URL, buf *bytes.Buffer) *bytes.Buffer

// New creates a new Scraper instance.
func New(logger *zap.Logger, cfg Config) (*Scraper, error) {
	var errs *multierror.Error
	u, err := url.Parse(cfg.URL)
	if err != nil {
		errs = multierror.Append(errs, err)
	}

	includes, err := compileRegexps(cfg.Includes)
	if err != nil {
		errs = multierror.Append(errs, err)
	}

	excludes, err := compileRegexps(cfg.Excludes)
	if err != nil {
		errs = multierror.Append(errs, err)
	}

	if errs != nil {
		return nil, errs.ErrorOrNil()
	}

	if u.Scheme == "" {
		u.Scheme = "http" // if no URL scheme was given default to http
	}

	b := surf.NewBrowser()
	b.SetUserAgent(agent.GoogleBot())
	b.SetTimeout(time.Duration(cfg.Timeout) * time.Second)

	s := &Scraper{
		config: cfg,

		browser:        b,
		log:            logger,
		assets:         make(map[string]bool),
		assetsExternal: make(map[string]bool),
		pages:          make(map[string]bool),
		URL:            u,
		includes:       includes,
		excludes:       excludes,
	}
	return s, nil
}

// compileRegexps compiles the strings to regular expressions.
func compileRegexps(sl []string) ([]*regexp.Regexp, error) {
	var errs error
	var l []*regexp.Regexp
	for _, e := range sl {
		re, err := regexp.Compile(e)
		if err == nil {
			l = append(l, re)
		} else {
			errs = multierror.Append(errs, err)
		}
	}
	return l, errs
}

// Start starts the scraping
func (s *Scraper) Start() error {
	if s.config.OutputDirectory != "" {
		if err := os.MkdirAll(s.config.OutputDirectory, os.ModePerm); err != nil {
			return err
		}
	}

	p := s.URL.Path
	if p == "" {
		p = "/"
	}
	s.pages[p] = false

	if s.config.Username != "" {
		auth := base64.StdEncoding.EncodeToString([]byte(s.config.Username + ":" + s.config.Password))
		s.browser.AddRequestHeader("Authorization", "Basic "+auth)
	}

	s.scrapeURL(s.URL, 0)
	return nil
}

func (s *Scraper) scrapeURL(u *url.URL, currentDepth uint) {
	s.log.Info("Downloading", zap.Stringer("URL", u))
	if err := s.browser.Open(u.String()); err != nil {
		s.log.Error("Request failed",
			zap.Stringer("URL", u),
			zap.Error(err))
		return
	}
	if c := s.browser.StatusCode(); c != http.StatusOK {
		s.log.Error("Request failed",
			zap.Stringer("URL", u),
			zap.Int("http_status_code", c))
		return
	}

	buf := &bytes.Buffer{}
	if _, err := s.browser.Download(buf); err != nil {
		s.log.Error("Downloading content failed",
			zap.Stringer("URL", u),
			zap.Error(err))
		return
	}

	if currentDepth == 0 {
		u = s.browser.Url()
		// use the URL that the website returned as new base url for the
		// scrape, in case of a redirect it changed
		s.URL = u
	}

	html, err := s.fixFileReferences(u, buf)
	if err != nil {
		s.log.Error("Fixing file references failed",
			zap.Stringer("URL", u),
			zap.Error(err))
	} else {
		buf = bytes.NewBufferString(html)
		filePath := s.GetFilePath(u, true)
		// always update html files, content might have changed
		if err = s.writeFile(filePath, buf); err != nil {
			s.log.Error("Writing HTML to file failed",
				zap.Stringer("URL", u),
				zap.String("file", filePath),
				zap.Error(err))
		}
	}

	s.downloadReferences()

	var toScrape []*url.URL
	// check first and download afterwards to not hit max depth limit for
	// start page links because of recursive linking
	for _, link := range s.browser.Links() {
		if s.checkPageURL(link.URL, currentDepth) {
			toScrape = append(toScrape, link.URL)
		}
	}

	for _, URL := range toScrape {
		s.scrapeURL(URL, currentDepth+1)
	}
}

func (s *Scraper) downloadReferences() {
	for _, image := range s.browser.Images() {
		s.imagesQueue = append(s.imagesQueue, &image.DownloadableAsset)
	}
	for _, stylesheet := range s.browser.Stylesheets() {
		s.downloadAssetURL(&stylesheet.DownloadableAsset, s.checkCSSForUrls)
	}
	for _, script := range s.browser.Scripts() {
		s.downloadAssetURL(&script.DownloadableAsset, nil)
	}
	for _, image := range s.imagesQueue {
		s.downloadAssetURL(image, s.checkImageForRecode)
	}
	s.imagesQueue = nil
}

// checkPageURL checks if a page should be downloaded
func (s *Scraper) checkPageURL(url *url.URL, currentDepth uint) bool {
	if url.Scheme != "http" && url.Scheme != "https" {
		return false
	}
	if url.Host != s.URL.Host {
		s.log.Debug("Skipping external host page", zap.Stringer("URL", url))
		return false
	}

	p := url.Path
	if p == "" {
		p = "/"
	}

	if _, ok := s.pages[p]; ok { // was already downloaded or checked
		if url.Fragment != "" {
			return false
		}
		s.log.Debug("Skipping already checked page", zap.Stringer("URL", url))
		return false
	}

	s.pages[p] = false
	if s.config.MaxDepth != 0 && currentDepth == s.config.MaxDepth {
		s.log.Debug("Skipping too deep level page", zap.Stringer("URL", url))
		return false
	}

	if s.includes != nil && !s.isURLIncluded(url) {
		return false
	}
	if s.excludes != nil && s.isURLExcluded(url) {
		return false
	}

	s.log.Debug("New page to queue", zap.Stringer("URL", url))
	return true
}

// downloadAssetURL downloads an asset if it does not exist on disk yet.
func (s *Scraper) downloadAssetURL(asset *browser.DownloadableAsset, processor assetProcessor) {
	URL := asset.URL

	if URL.Host == s.URL.Host {
		if _, ok := s.assets[URL.Path]; ok { // was already downloaded or checked
			return
		}

		s.assets[URL.Path] = false
	} else if s.isExternalFileChecked(URL) {
		return
	}

	if s.includes != nil && !s.isURLIncluded(URL) {
		return
	}
	if s.excludes != nil && s.isURLExcluded(URL) {
		return
	}

	filePath := s.GetFilePath(URL, false)
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		return
	}

	s.log.Info("Downloading", zap.Stringer("URL", URL))

	buf := &bytes.Buffer{}
	_, err := asset.Download(buf)
	if err != nil {
		s.log.Error("Downloading asset failed",
			zap.Stringer("URL", URL),
			zap.Error(err))
		return
	}

	if processor != nil {
		buf = processor(URL, buf)
	}

	if err = s.writeFile(filePath, buf); err != nil {
		s.log.Error("Writing asset file failed",
			zap.Stringer("URL", URL),
			zap.String("file", filePath),
			zap.Error(err))
	}
}

func (s *Scraper) isURLIncluded(url *url.URL) bool {
	if url.Scheme == "data" {
		return true
	}

	for _, re := range s.includes {
		if re.MatchString(url.Path) {
			s.log.Info("Including URL",
				zap.Stringer("URL", url),
				zap.Stringer("Included", re))
			return true
		}
	}
	return false
}

func (s *Scraper) isURLExcluded(url *url.URL) bool {
	if url.Scheme == "data" {
		return true
	}

	for _, re := range s.excludes {
		if re.MatchString(url.Path) {
			s.log.Info("Skipping URL",
				zap.Stringer("URL", url),
				zap.Stringer("Excluded", re))
			return true
		}
	}
	return false
}

func (s *Scraper) isExternalFileChecked(url *url.URL) bool {
	if url.Host == s.URL.Host {
		return false
	}

	fullURL := url.String()
	if _, ok := s.assetsExternal[fullURL]; ok { // was already downloaded or checked
		return true
	}

	s.assetsExternal[fullURL] = true
	s.log.Info("External URL", zap.Stringer("URL", url))

	return false
}
