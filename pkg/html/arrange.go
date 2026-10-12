package html

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/goclone-dev/goclone/pkg/parser"
)

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
)

// uniqueFilename must match the name the crawler saved the asset under, so it
// delegates to the same function the crawler uses.
func uniqueFilename(rawURL string) string {
	return parser.UniqueFilename(rawURL)
}

// stripIntegrity removes Subresource Integrity and CORS attributes from an
// element whose URL has been rewritten to a local file. The saved copy is no
// longer byte-identical to what the hash was computed over (CSS has its
// url(...) references rewritten), so browsers refuse to apply the resource at
// all - the page renders with no stylesheet. crossorigin="anonymous" is also
// rejected for file:// loads in Chrome, and pointless for same-origin files.
func stripIntegrity(s *goquery.Selection) {
	s.RemoveAttr("integrity")
	s.RemoveAttr("crossorigin")
}

// TODO: figure out what was done here at 4am
func arrange(projectDir string) error {
	indexfile := projectDir + "/index.html"
	input, err := os.ReadFile(indexfile)
	if err != nil {
		return err
	}

	// Create a single document from the entire HTML
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(input))
	if err != nil {
		return err
	}

	// Replace JS links in HTML (using unique filenames to match what extractor saved)
	doc.Find("script[src]").Each(func(i int, s *goquery.Selection) {
		data, exists := s.Attr("src")
		if exists {
			file := uniqueFilename(data)
			newPath := "js/" + file
			s.SetAttr("src", newPath)
			stripIntegrity(s)
		}
	})

	// Replace CSS links in HTML
	doc.Find("link[rel='stylesheet']").Each(func(i int, s *goquery.Selection) {
		data, exists := s.Attr("href")
		if exists {
			file := uniqueFilename(data)
			newPath := "css/" + file
			s.SetAttr("href", newPath)
			stripIntegrity(s)
		}
	})

	// Replace IMG links in HTML and strip remote srcset
	doc.Find("img[src]").Each(func(i int, s *goquery.Selection) {
		data, exists := s.Attr("src")
		if exists {
			file := uniqueFilename(data)
			newPath := "imgs/" + file
			s.SetAttr("src", newPath)
		}
		// Remove srcset pointing to remote to force local src usage
		srcset, hasSrcset := s.Attr("srcset")
		if hasSrcset && (strings.HasPrefix(srcset, "http://") || strings.HasPrefix(srcset, "https://")) {
			s.RemoveAttr("srcset")
		}
		s.RemoveAttr("sizes")
	})

	// Remove picture > source elements that load remote WebP variants
	// The fallback <img> will handle local loading
	doc.Find("picture source").Each(func(i int, s *goquery.Selection) {
		srcset, _ := s.Attr("srcset")
		if strings.HasPrefix(srcset, "http://") || strings.HasPrefix(srcset, "https://") {
			s.Remove()
		}
	})

	// Replace preload image links with local paths
	doc.Find("link[rel='preload'][as='image']").Each(func(i int, s *goquery.Selection) {
		data, exists := s.Attr("href")
		if exists && (strings.HasPrefix(data, "http://") || strings.HasPrefix(data, "https://")) {
			file := uniqueFilename(data)
			ext := strings.ToLower(parser.URLExtension(data))
			dir := extensionDir[ext]
			if dir != "" {
				s.SetAttr("href", dir+"/"+file)
			} else {
				s.Remove()
			}
		}
	})

	// Replace inline style links in HTML
	doc.Find("[style]").Each(func(i int, s *goquery.Selection) {
		styleAttr, exists := s.Attr("style")
		if exists {
			for _, match := range parser.CSSURLs(styleAttr) {
				assetUrl := strings.TrimSpace(match.URL)
				if assetUrl == "" || strings.HasPrefix(assetUrl, "data:") || strings.HasPrefix(assetUrl, "#") {
					continue
				}
				assetExt := strings.ToLower(parser.URLExtension(assetUrl))
				assetDir := extensionDir[assetExt]
				if assetDir != "" {
					assetBase := uniqueFilename(assetUrl)
					newPath := assetDir + "/" + assetBase
					styleAttr = strings.ReplaceAll(styleAttr, match.Raw, fmt.Sprintf("url('%s')", newPath))
				}
			}
			s.SetAttr("style", styleAttr)
		}
	})

	// Remove preconnect/dns-prefetch links pointing to remote resources
	doc.Find("link[rel='preconnect'], link[rel='dns-prefetch']").Each(func(i int, s *goquery.Selection) {
		href, _ := s.Attr("href")
		if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
			s.Remove()
		}
	})

	// Get the modified HTML
	html, err := doc.Html()
	if err != nil {
		return err
	}

	return os.WriteFile(indexfile, []byte(html), 0777)
}
