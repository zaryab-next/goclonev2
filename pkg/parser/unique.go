package parser

import (
	"net/url"
	"regexp"
	"strings"
)

// UniqueFilename generates a local filename for an asset URL that stays
// unique even when several URLs share the same basename (e.g. different
// view.js files), by prefixing the parent directory name.
//
// The crawler (which saves the file) and the HTML rewriter (which points the
// page at it) must agree on this name byte for byte, so both call this one
// function. It always works on the parsed URL path, never on the raw string:
// splitting the raw string made the host count as a path segment for URLs
// with a single path component (https://cdn.tolt.io/tolt.js), producing a
// name the crawler never wrote.
func UniqueFilename(link string) string {
	u, err := url.Parse(link)
	if err != nil {
		return URLFilename(link)
	}
	base := URLFilename(link)
	ext := URLExtension(link)
	// e.g. /wp-content/themes/sumatosoft/blocks/acf-cases-block/view.js
	//   -> acf-cases-block_view.js
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) >= 2 {
		parentDir := parts[len(parts)-2]
		nameWithoutExt := strings.TrimSuffix(base, ext)
		if parentDir != nameWithoutExt && parentDir != "" {
			return parentDir + "_" + base
		}
	}
	return base
}

// CSSURL is one url(...) reference found in CSS text.
type CSSURL struct {
	// Raw is the full matched text, e.g. url("a.png"), for replacing in place.
	Raw string
	// URL is the unquoted, unescaped reference, e.g. a.png.
	URL string
}

// cssURLRegex matches url(...) in its three forms: double-quoted,
// single-quoted and bare. Bare URLs may contain backslash escapes such as
// the \( and \) Webflow emits for files named "image (1).avif"; a naive
// non-greedy `url\((.*?)\)` stops at the first escaped paren and yields a
// truncated URL with the wrong extension.
var cssURLRegex = regexp.MustCompile(`url\(\s*(?:"((?:[^"\\]|\\.)*)"|'((?:[^'\\]|\\.)*)'|((?:[^()\s"'\\]|\\.)*))\s*\)`)

var cssEscapeRegex = regexp.MustCompile(`\\(.)`)

// CSSURLs returns every url(...) reference in css, in order of appearance.
func CSSURLs(css string) []CSSURL {
	matches := cssURLRegex.FindAllStringSubmatch(css, -1)
	out := make([]CSSURL, 0, len(matches))
	for _, m := range matches {
		ref := m[1]
		if ref == "" {
			ref = m[2]
		}
		if ref == "" {
			ref = m[3]
		}
		out = append(out, CSSURL{Raw: m[0], URL: cssEscapeRegex.ReplaceAllString(ref, "$1")})
	}
	return out
}
