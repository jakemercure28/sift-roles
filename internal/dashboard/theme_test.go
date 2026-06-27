package dashboard

import (
	"os"
	"path/filepath"
	"testing"
)

func readGolden(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return string(data)
}

// assertGolden compares got to the golden file and, on mismatch, reports the
// first differing line so divergences are easy to locate. Set UPDATE_GOLDENS=1
// to rewrite the fixtures from current renderer output instead of comparing.
func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	if os.Getenv("UPDATE_GOLDENS") == "1" {
		if err := os.WriteFile(filepath.Join("testdata", name), []byte(got), 0o644); err != nil {
			t.Fatalf("update golden %s: %v", name, err)
		}
		return
	}
	want := readGolden(t, name)
	if got == want {
		return
	}
	gl, wl := splitLines(got), splitLines(want)
	for i := 0; i < len(gl) || i < len(wl); i++ {
		var g, w string
		if i < len(gl) {
			g = gl[i]
		}
		if i < len(wl) {
			w = wl[i]
		}
		if g != w {
			t.Fatalf("%s mismatch at line %d:\n  got:  %q\n  want: %q\n(got %d lines, want %d)", name, i+1, g, w, len(gl), len(wl))
		}
	}
	t.Fatalf("%s mismatch (len got=%d want=%d)", name, len(got), len(want))
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func TestRenderThemeCSSParity(t *testing.T) {
	assertGolden(t, "theme.css.golden", RenderThemeCSS())
}

func TestClientThemeJSONParity(t *testing.T) {
	assertGolden(t, "client-theme.json.golden", ClientThemeJSON())
}
