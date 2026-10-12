package parser

import (
	"reflect"
	"testing"
)

func TestUniqueFilename(t *testing.T) {
	tests := map[string]string{
		// single path segment: the host must not be treated as the parent dir
		"https://cdn.tolt.io/tolt.js":                              "tolt.js",
		"//cdn.prod.website-files.com/6a4f_clarity_script-6.0.5.js": "6a4f_clarity_script-6.0.5.js",
		"https://x.com/wp-content/blocks/acf-cases/view.js":        "acf-cases_view.js",
		"/wp-content/blocks/acf-cases/view.js":                     "acf-cases_view.js",
		"https://www.googletagmanager.com/gtag/js?id=G-1":          "gtag_js",
		"https://x.com/css/css.css":                                "css.css",
	}
	for in, want := range tests {
		if got := UniqueFilename(in); got != want {
			t.Errorf("UniqueFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCSSURLs(t *testing.T) {
	css := `a{background:url("a b.png")}b{background:url('c.avif')}` +
		`c{background:url(https://x.com/d%20\(1\).avif)}d{background:url( ../e.woff2 )}`
	got := CSSURLs(css)
	var urls []string
	for _, g := range got {
		urls = append(urls, g.URL)
	}
	want := []string{"a b.png", "c.avif", "https://x.com/d%20(1).avif", "../e.woff2"}
	if !reflect.DeepEqual(urls, want) {
		t.Errorf("CSSURLs = %q, want %q", urls, want)
	}
	if got[2].Raw != `url(https://x.com/d%20\(1\).avif)` {
		t.Errorf("Raw not preserved: %q", got[2].Raw)
	}
	if UniqueFilename(got[2].URL) != "d (1).avif" {
		t.Errorf("unexpected filename %q", UniqueFilename(got[2].URL))
	}
}
