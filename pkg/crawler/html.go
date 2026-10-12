package crawler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
)

// HTMLExtractor downloads the HTML content from a URL and saves it to
// index.html. It returns whether a headless-browser fallback was needed to
// obtain the content, and the final HTML that was written to disk (used by
// the caller to feed link discovery without re-requesting a blocked URL over
// plain HTTP).
func HTMLExtractor(ctx context.Context, link string, projectPath string, userAgent string) (usedHeadless bool, finalHTML string, err error) {
	fmt.Println("Extracting HTML from --> ", link)

	// get the html body (shared client carries a per-request timeout and
	// skips TLS verification, matching the asset extractor)
	resp, err := httpClient.Get(link)
	if err != nil {
		return false, "", fmt.Errorf("failed to GET HTML: %v", err)
	}
	defer resp.Body.Close()

	// Read the HTML content
	htmlData, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, "", fmt.Errorf("failed to read HTML body: %v", err)
	}

	if challengeErr := detectBotChallenge(resp, htmlData); challengeErr != nil {
		// Only sites that actually trip the anti-bot detection pay the cost
		// of a real browser launch; every other site never reaches this branch.
		fmt.Printf("warning: %s: %v - retrying with headless browser\n", link, challengeErr)

		rendered, headlessErr := renderWithHeadlessBrowser(ctx, link, userAgent, projectPath)
		if headlessErr != nil {
			return false, "", fmt.Errorf("%s: %w (headless fallback also failed: %v)", link, challengeErr, headlessErr)
		}
		if stillBlockedErr := detectBotChallenge(&http.Response{Header: http.Header{}, StatusCode: http.StatusOK}, []byte(rendered)); stillBlockedErr != nil {
			return false, "", fmt.Errorf("%s: still blocked after headless render: %w", link, stillBlockedErr)
		}

		htmlData = []byte(rendered)
		usedHeadless = true
	}

	// get the project name and path we use the path to
	f, err := os.OpenFile(projectPath+"/"+"index.html", os.O_RDWR|os.O_CREATE, 0777)
	if err != nil {
		return false, "", fmt.Errorf("failed to open index.html: %v", err)
	}
	defer f.Close()

	if _, err := f.Write(htmlData); err != nil {
		return false, "", fmt.Errorf("failed to write HTML content: %v", err)
	}

	return usedHeadless, string(htmlData), nil
}
