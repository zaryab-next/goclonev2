package crawler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/goclone-dev/goclone/pkg/parser"
)

// headlessRenderTimeout bounds the entire headless render attempt (browser
// launch + navigation + challenge wait + asset capture). It only runs when a
// plain HTTP request has already been detected as blocked by an anti-bot
// challenge, so this cost is never paid by sites that don't need it.
const headlessRenderTimeout = 30 * time.Second

// challengeSettleWait gives client-side JS challenges (proof-of-work
// redirects like SiteGround's SG-Captcha, Cloudflare's "Just a moment...",
// etc.) time to finish executing and redirect to the real page before the
// rendered HTML is captured.
const challengeSettleWait = 6 * time.Second

// assetFlushWait is how long to give in-flight network-response captures a
// chance to finish writing to disk after the page itself has settled.
const assetFlushWait = 3 * time.Second

// renderWithHeadlessBrowser loads link in a real headless Chrome instance so
// that JS-based anti-bot challenges resolve the same way they would for an
// actual browser visitor, then returns the final rendered page's HTML.
//
// Anti-bot WAFs that gate the main page (e.g. SiteGround's SG-Captcha) also
// gate same-origin CSS/JS/image requests made afterward as separate plain
// HTTP calls - the browser's own session/fingerprint passes, but a fresh
// net/http request for the same asset does not (verified: replaying the
// browser's session cookie via curl still gets challenged). So instead of
// only grabbing the rendered HTML and leaving those assets to fail later,
// this also listens to the browser's own network traffic while it renders
// the page and saves every CSS/JS/image/font response it receives straight
// to projectPath - those bytes came from inside the browser session that
// already passed the challenge, so they're the real content.
//
// This is a fallback path only: it is deliberately not used for every site,
// only invoked after a plain HTTP request has already been flagged by
// detectBotChallenge. Sites without this kind of protection never launch a
// browser and keep using the fast plain-HTTP path.
func renderWithHeadlessBrowser(parentCtx context.Context, link string, userAgent string, projectPath string) (string, error) {
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	if userAgent != "" {
		opts = append(opts, chromedp.UserAgent(userAgent))
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(parentCtx, opts...)
	defer cancelAlloc()

	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	timeoutCtx, cancelTimeout := context.WithTimeout(browserCtx, headlessRenderTimeout)
	defer cancelTimeout()

	var (
		mu      sync.Mutex
		pending = map[network.RequestID]string{} // requestID -> resource URL, awaiting body
		wg      sync.WaitGroup
	)

	chromedp.ListenTarget(timeoutCtx, func(ev interface{}) {
		switch e := ev.(type) {
		case *network.EventResponseReceived:
			if e.Response == nil || extensionDir[strings.ToLower(parser.URLExtension(e.Response.URL))] == "" {
				return
			}
			mu.Lock()
			pending[e.RequestID] = e.Response.URL
			mu.Unlock()

		case *network.EventLoadingFinished:
			mu.Lock()
			resourceURL, ok := pending[e.RequestID]
			delete(pending, e.RequestID)
			mu.Unlock()
			if !ok {
				return
			}

			wg.Add(1)
			go func(reqID network.RequestID, resourceURL string) {
				defer wg.Done()
				// GetResponseBody requires a context bound to the tab's CDP
				// executor; the plain context from ListenTarget isn't
				// enough outside of chromedp.Run and fails with "invalid
				// context" without this.
				execCtx := cdp.WithExecutor(timeoutCtx, chromedp.FromContext(timeoutCtx).Target)
				body, err := network.GetResponseBody(reqID).Do(execCtx)
				if err != nil {
					return
				}
				saveCapturedAsset(projectPath, resourceURL, body)
			}(e.RequestID, resourceURL)
		}
	})

	var html string
	err := chromedp.Run(timeoutCtx,
		network.Enable(),
		chromedp.Navigate(link),
		chromedp.Sleep(challengeSettleWait),
		chromedp.OuterHTML("html", &html, chromedp.ByQuery),
	)
	if err != nil {
		return "", fmt.Errorf("headless render failed: %w", err)
	}

	// Give in-flight GetResponseBody calls a chance to finish before the
	// browser context (and with it, the ability to fetch response bodies)
	// is torn down by the deferred cancels.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(assetFlushWait):
	}

	return html, nil
}

// saveCapturedAsset writes a resource captured from the browser's own
// network traffic to disk using the same directory/naming scheme as
// Extractor, so link rewriting later treats it identically to an asset
// fetched over plain HTTP.
func saveCapturedAsset(projectPath, resourceURL string, body []byte) {
	ext := strings.ToLower(parser.URLExtension(resourceURL))
	dir := extensionDir[ext]
	if dir == "" {
		return
	}

	base := uniqueFilename(resourceURL)
	fullPath := filepath.Join(projectPath, dir, base)

	if err := os.MkdirAll(filepath.Dir(fullPath), 0777); err != nil {
		return
	}
	if err := os.WriteFile(fullPath, body, 0777); err != nil {
		return
	}

	recordURLFilename(resourceURL, dir+"/"+base)
	fmt.Println("Captured via headless browser -->", resourceURL)

	if ext == ".css" {
		processCSS(projectPath, resourceURL, base)
	}
}
