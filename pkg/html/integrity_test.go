package html

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Rewritten local assets no longer match their original SRI hash, and
// crossorigin breaks file:// loads, so both attributes must be dropped.
func TestArrangeStripsIntegrityAndCrossorigin(t *testing.T) {
	dir := t.TempDir()
	in := `<html><head>` +
		`<link href="https://cdn.x.com/a/site.css" rel="stylesheet" integrity="sha384-abc" crossorigin="anonymous"/>` +
		`</head><body><script src="https://cdn.x.com/tolt.js" integrity="sha384-def" crossorigin="anonymous"></script></body></html>`
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(in), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := LinkRestructure(dir); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(filepath.Join(dir, "index.html"))
	s := string(out)
	for _, bad := range []string{"integrity", "crossorigin"} {
		if strings.Contains(s, bad) {
			t.Errorf("output still contains %q:\n%s", bad, s)
		}
	}
	// script saved by the crawler as tolt.js, not <host>_tolt.js
	if !strings.Contains(s, `src="js/tolt.js"`) || !strings.Contains(s, `href="css/a_site.css"`) {
		t.Errorf("unexpected rewritten paths:\n%s", s)
	}
}
