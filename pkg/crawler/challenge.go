package crawler

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
)

// challengeTitleMarkers are strings found in the HTML body of common
// anti-bot / WAF challenge pages (SiteGround SG-Captcha, Cloudflare,
// generic "checking your browser" style interstitials). These pages
// return a 200/202 status but never contain the real site content, so
// goclone must not save them as if the clone succeeded.
var challengeTitleMarkers = []string{
	"robot challenge screen",
	"just a moment",
	"attention required",
	"checking your browser",
	"ddos protection by",
	"access denied",
}

// detectBotChallenge inspects a response for signs that the request was
// blocked or redirected to an anti-bot/WAF challenge instead of the real
// content. It returns a descriptive error when a challenge is detected,
// or nil if the response looks like genuine page content.
func detectBotChallenge(resp *http.Response, body []byte) error {
	header := resp.Header

	if v := header.Get("SG-Captcha"); v != "" {
		return fmt.Errorf("blocked by anti-bot challenge (SG-Captcha: %s) - this site requires a JS-capable browser to pass", v)
	}
	if v := header.Get("cf-mitigated"); v != "" {
		return fmt.Errorf("blocked by Cloudflare bot mitigation (cf-mitigated: %s)", v)
	}

	// Challenge pages are HTML. Scanning JS/CSS/binary assets for these
	// phrases gives false positives (a minified bundle containing the string
	// "access denied" was rejected as a challenge page and never saved).
	if looksLikeHTML(header.Get("Content-Type"), body) {
		lower := strings.ToLower(string(body))
		for _, marker := range challengeTitleMarkers {
			if strings.Contains(lower, marker) {
				return fmt.Errorf("blocked by anti-bot/WAF challenge page (detected %q in response) - cannot clone without a JS-capable browser", marker)
			}
		}
	}

	switch resp.StatusCode {
	case http.StatusForbidden, http.StatusTooManyRequests:
		return fmt.Errorf("server returned %d %s - site may be blocking automated requests", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	return nil
}

// looksLikeHTML reports whether a response is an HTML document, judged by
// Content-Type when present and otherwise by whether the body starts with a tag.
func looksLikeHTML(contentType string, body []byte) bool {
	if strings.Contains(strings.ToLower(contentType), "html") {
		return true
	}
	return bytes.HasPrefix(bytes.TrimSpace(body), []byte("<"))
}
