package crawler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"

	"github.com/gocolly/colly/v2"
	"github.com/goclone-dev/goclone/pkg/parser"
)

// Collector searches for css, js, and images within a given link
// TODO improve for better performance
func Collector(ctx context.Context, url string, projectPath string, cookieJar *cookiejar.Jar, proxyString string, userAgent string) error {
	// Fresh dedup state for this crawl so re-cloning re-downloads assets.
	resetDownloadState()

	// First, download the main HTML file
	fmt.Printf("Downloading main HTML from: %s\n", url)
	usedHeadless, mainHTML, err := HTMLExtractor(ctx, url, projectPath, userAgent)
	if err != nil {
		return fmt.Errorf("failed to download main HTML: %v", err)
	}

	// create a new collector
	c := colly.NewCollector(colly.Async(true))
	setUpCollector(c, ctx, cookieJar, proxyString, userAgent)

	if usedHeadless {
		// The plain HTTP path was blocked by an anti-bot challenge and a
		// headless browser already solved it to obtain mainHTML. Reuse that
		// content for link discovery instead of letting colly re-request the
		// same URL over plain HTTP, which would just hit the same challenge
		// again and find zero links. Sites that never needed the headless
		// fallback are unaffected and keep using colly's normal network path.
		fmt.Println("Reusing headless-rendered HTML for link discovery")
		c.WithTransport(cachedResponseTransport{body: []byte(mainHTML)})
	}

	// extractAsync runs an asset download in its own goroutine so that
	// colly's callbacks (which fire sequentially in one goroutine) don't
	// serialize the downloads. Actual concurrency is bounded by downloadSem
	// inside Extractor; assetWG lets the crawl wait for every download to
	// finish before link restructuring runs.
	var assetWG sync.WaitGroup
	extractAsync := func(link, context, dirHint string) {
		assetWG.Add(1)
		go func() {
			defer assetWG.Done()
			if err := extractTo(link, projectPath, dirHint); err != nil {
				fmt.Printf("warning: failed to extract %s%s: %v\n", link, context, err)
			}
		}()
	}

	// search for all link tags that have a rel attribute that is equal to stylesheet - CSS
	c.OnHTML("link[rel='stylesheet']", func(e *colly.HTMLElement) {
		link := e.Attr("href")
		fmt.Println("Css found", "-->", link)
		extractAsync(e.Request.AbsoluteURL(link), "", "css")
	})

	// search for all script tags with src attribute -- JS
	c.OnHTML("script[src]", func(e *colly.HTMLElement) {
		link := e.Attr("src")
		fmt.Println("Js found", "-->", link)
		extractAsync(e.Request.AbsoluteURL(link), "", "js")
	})

	// serach for all img tags with src attribute -- Images
	c.OnHTML("img[src]", func(e *colly.HTMLElement) {
		link := e.Attr("src")
		if strings.HasPrefix(link, "data:image") || strings.HasPrefix(link, "blob:") {
			return
		}
		fmt.Println("Img found", "-->", link)
		extractAsync(e.Request.AbsoluteURL(link), "", "imgs")
	})

	// search for all elements with a style attribute to find url(...)
	c.OnHTML("[style]", func(e *colly.HTMLElement) {
		styleAttr := e.Attr("style")
		for _, match := range parser.CSSURLs(styleAttr) {
			assetUrl := strings.TrimSpace(match.URL)
			if assetUrl == "" || strings.HasPrefix(assetUrl, "data:") || strings.HasPrefix(assetUrl, "#") {
				continue
			}
			fmt.Println("Inline style url found", "-->", assetUrl)
			extractAsync(e.Request.AbsoluteURL(assetUrl), " from inline style", "")
		}
	})

	// search for preload image links (used for hero/above-fold images)
	c.OnHTML("link[rel='preload'][as='image']", func(e *colly.HTMLElement) {
		link := e.Attr("href")
		if link == "" {
			return
		}
		fmt.Println("Preload image found", "-->", link)
		extractAsync(e.Request.AbsoluteURL(link), " (preload image)", "")
	})

	// Visit each url and wait for stuff to load :)
	if err := c.Visit(url); err != nil {
		return err
	}
	c.Wait()
	// Wait for every asset download spawned by the callbacks above (and any
	// nested CSS asset downloads) to complete before returning, so link
	// restructuring sees a fully-populated project directory.
	assetWG.Wait()
	return nil
}

// cachedResponseTransport serves a single pre-fetched HTML document for any
// request instead of hitting the network. It's only installed on the
// collector when the main page required a headless-browser fallback, so that
// colly's link-discovery visit doesn't re-trigger the anti-bot challenge.
type cachedResponseTransport struct {
	body []byte
}

func (t cachedResponseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(t.body)),
		Request:    req,
	}, nil
}

type cancelableTransport struct {
	ctx       context.Context
	transport http.RoundTripper
}

func (t cancelableTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.ctx.Err(); err != nil {
		return nil, err
	}
	return t.transport.RoundTrip(req.WithContext(t.ctx))
}

func setUpCollector(c *colly.Collector, ctx context.Context, cookieJar *cookiejar.Jar, proxyString, userAgent string) {
	if cookieJar != nil {
		c.SetCookieJar(cookieJar)
	}
	if proxyString != "" {
		c.SetProxy(proxyString)
	} else {
		c.WithTransport(cancelableTransport{ctx: ctx, transport: http.DefaultTransport})
	}
	if userAgent != "" {
		c.UserAgent = userAgent
	}
}
