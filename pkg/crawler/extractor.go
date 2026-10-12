package crawler

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goclone-dev/goclone/pkg/parser"
)

// httpClient is shared by all asset/HTML downloads. It carries a per-request
// timeout so a single slow or hung response can never stall the whole crawl
// (the previous http.Get used http.DefaultClient, which has no timeout).
var httpClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	},
}

// downloadSem bounds how many asset downloads run at once. Asset extraction
// is now concurrent (see processCSS), so this caps parallelism to avoid
// opening hundreds of sockets or tripping a site's rate limiting, while still
// being far faster than the old fully-serial downloads.
var downloadSem = make(chan struct{}, 12)

// seenURLs deduplicates downloads: the same font/image is frequently
// referenced from many CSS files, and re-fetching each one serially was a
// large part of why font-heavy pages took minutes to clone. It is reset at
// the start of each crawl (resetDownloadState) so re-cloning re-downloads.
var (
	seenURLs   = map[string]bool{}
	seenURLsMu sync.Mutex
)

// resetDownloadState clears per-crawl dedup bookkeeping so a fresh Collector
// run (including re-cloning into a cleaned directory) downloads assets again
// instead of treating them as already handled from a previous run.
func resetDownloadState() {
	seenURLsMu.Lock()
	seenURLs = map[string]bool{}
	seenURLsMu.Unlock()

	urlToFilenameMu.Lock()
	urlToFilename = map[string]string{}
	urlToFilenameMu.Unlock()
}

// markSeen records link as being handled for a given project and reports
// whether it was already seen, so callers can skip a redundant download. The
// key includes projectPath so cloning multiple sites in one run that share an
// asset URL still writes the asset into each project's directory.
func markSeen(projectPath, link string) (already bool) {
	key := projectPath + "\x00" + link
	seenURLsMu.Lock()
	defer seenURLsMu.Unlock()
	if seenURLs[key] {
		return true
	}
	seenURLs[key] = true
	return false
}

// file extension map for directing files to their proper directory in O(1) time
var (
	extensionDir = map[string]string{
		".css":   "css",
		".js":    "js",
		".jpg":   "imgs",
		".jpeg":  "imgs",
		".gif":   "imgs",
		".png":   "imgs",
		".svg":   "imgs",
		".webp":  "imgs",
		".avif":  "imgs",
		".ico":   "imgs",
		".woff":  "fonts",
		".woff2": "fonts",
		".ttf":   "fonts",
		".eot":   "fonts",
		".otf":   "fonts",
		".mp4":   "media",
		".webm":  "media",
	}

	// Track URL -> local filename mappings to avoid duplicate downloads.
	// Written concurrently from colly's async asset callbacks and, for
	// challenge-protected sites, from headless-browser capture goroutines
	// (see headless.go) - guarded by urlToFilenameMu.
	urlToFilename   = map[string]string{}
	urlToFilenameMu sync.Mutex
)

func recordURLFilename(link, relPath string) {
	urlToFilenameMu.Lock()
	urlToFilename[link] = relPath
	urlToFilenameMu.Unlock()
}

// uniqueFilename is the local filename used for link; shared with the HTML
// rewriter via the parser package so both sides always agree.
func uniqueFilename(link string) string {
	return parser.UniqueFilename(link)
}

// Extractor visits a link determines if its a page or sublink
// downloads the contents to a correct directory in project folder
func Extractor(link string, projectPath string) error {
	return extractTo(link, projectPath, "")
}

// extractTo is Extractor with an optional directory hint. The HTML rewriter
// points every <script src>, <link rel=stylesheet> and <img src> at js/, css/
// or imgs/ regardless of the URL's extension, so callers that know which tag
// referenced the asset pass that directory here; otherwise URLs with no
// recognised extension (e.g. googletagmanager.com/gtag/js?id=...) would never
// be downloaded and the rewritten page would reference a missing file.
func extractTo(link string, projectPath string, dirHint string) error {
	// Skip anything already downloaded (or in flight) for this project; the
	// same asset is commonly referenced from many CSS files.
	if markSeen(projectPath, link) {
		return nil
	}

	// Only bother with extensions we know how to place on disk.
	ext := strings.ToLower(parser.URLExtension(link))
	dirPath := dirHint
	if dirPath == "" {
		dirPath = extensionDir[ext]
	}
	if dirPath == "" {
		return nil
	}
	base := uniqueFilename(link)

	// The network fetch and disk write are bounded by downloadSem. The
	// semaphore slot is released before recursing into processCSS so that a
	// stylesheet being processed doesn't hold a slot while waiting on its own
	// (semaphore-bounded) nested font/image downloads, which would deadlock.
	if err := func() error {
		downloadSem <- struct{}{}
		defer func() { <-downloadSem }()

		fmt.Println("Extracting --> ", link)
		resp, err := httpClient.Get(link)
		if err != nil {
			return fmt.Errorf("failed to GET %s: %w", link, err)
		}
		defer resp.Body.Close()

		recordURLFilename(link, dirPath+"/"+base)
		return writeFileToPath(projectPath, base, dirPath, resp)
	}(); err != nil {
		return err
	}

	if dirPath == "css" {
		processCSS(projectPath, link, base)
	}
	return nil
}

func writeFileToPath(projectPath, filename, fileDir string, resp *http.Response) error {
	// Read the body first so a blocked/challenge response never touches disk
	htmlData, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}
	if err := detectBotChallenge(resp, htmlData); err != nil {
		return fmt.Errorf("%s: %w", resp.Request.URL, err)
	}

	// Create the full path
	fullPath := filepath.Join(projectPath, fileDir, filename)

	// Create the directory if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(fullPath), 0777); err != nil {
		return fmt.Errorf("failed to create directories for %s: %w", fullPath, err)
	}

	// Open the file for writing
	f, err := os.OpenFile(fullPath, os.O_RDWR|os.O_CREATE, 0777)
	if err != nil {
		return fmt.Errorf("failed to open file %s: %w", fullPath, err)
	}
	defer f.Close()

	if _, err := f.Write(htmlData); err != nil {
		return fmt.Errorf("failed to write file %s: %w", fullPath, err)
	}
	return nil
}

func processCSS(projectPath, link, base string) {
	fullPath := filepath.Join(projectPath, "css", base)
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return
	}

	baseLinkUrl, err := url.Parse(link)
	if err != nil {
		return
	}

	cssStr := string(data)
	matches := parser.CSSURLs(cssStr)

	// The referenced assets are downloaded concurrently (a single stylesheet
	// can reference dozens of font files; downloading them one-by-one was the
	// main cause of multi-minute clones). The CSS-string rewriting is done
	// synchronously here in the loop and is independent of whether a given
	// download succeeds, so it stays correct regardless of download ordering.
	var wg sync.WaitGroup
	for _, match := range matches {
		assetUrl := strings.TrimSpace(match.URL)
		if assetUrl == "" || strings.HasPrefix(assetUrl, "data:") || strings.HasPrefix(assetUrl, "#") {
			continue
		}

		// Resolve absolute URL
		assetParsedUrl, err := url.Parse(assetUrl)
		if err != nil {
			continue
		}
		absoluteUrl := baseLinkUrl.ResolveReference(assetParsedUrl).String()

		// Replace the URL in CSS with the local path (deterministic).
		assetExt := strings.ToLower(parser.URLExtension(absoluteUrl))
		assetDir := extensionDir[assetExt]
		if assetDir != "" {
			assetBase := uniqueFilename(absoluteUrl)
			newPath := "../" + assetDir + "/" + assetBase
			cssStr = strings.ReplaceAll(cssStr, match.Raw, fmt.Sprintf("url('%s')", newPath))
		}

		// Download the asset concurrently.
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			if err := Extractor(u, projectPath); err != nil {
				fmt.Printf("warning: failed to extract %s from CSS: %v\n", u, err)
			}
		}(absoluteUrl)
	}
	wg.Wait()

	os.WriteFile(fullPath, []byte(cssStr), 0777)
}
