package ats

import (
	"reflect"
	"slices"
	"testing"
)

func TestDecodeEntities(t *testing.T) {
	cases := map[string]string{
		"&amp;":      "&",
		"&amp;amp;":  "&",   // collapses across passes
		"a&#x2B;b":   "a+b", // hex numeric
		"x&#38;y":    "x&y", // decimal numeric
		`&`:          "&",   // unicode escape
		"&mdash;":    "—",
		"plain text": "plain text",
		"&unknown;":  "&unknown;", // unrecognized entity left intact
	}
	for in, want := range cases {
		if got := decodeEntities(in); got != want {
			t.Errorf("decodeEntities(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripHTML(t *testing.T) {
	got := stripHTML("<p>Hello&nbsp;&amp; <b>world</b></p>")
	want := "Hello & world"
	if got != want {
		t.Errorf("stripHTML = %q, want %q", got, want)
	}
}

func TestAbsoluteURL(t *testing.T) {
	cases := []struct {
		href, base, want string
	}{
		{"/l/123", "https://remoteok.com/remote-jobs/x", "https://remoteok.com/l/123"},
		{"https://abs.com/x", "", "https://abs.com/x"},
		{"relative/path", "", "relative/path"},
		{"mailto:foo@bar.com", "", "foo@bar.com"},
		{"", "https://x.com", ""},
		{"https://abs.com/y", "https://base.com", "https://abs.com/y"},
	}
	for _, c := range cases {
		if got := absoluteURL(c.href, c.base); got != c.want {
			t.Errorf("absoluteURL(%q,%q) = %q, want %q", c.href, c.base, got, c.want)
		}
	}
}

// TestExtractBuiltInApplyURL mirrors the JS unit expectation in test/ats-resolver.test.js:
// the & escape is decoded to & before JSON parsing.
func TestExtractBuiltInApplyURL(t *testing.T) {
	html := `
      <script type="module">
        Builtin.jobPostInit({"job":{"id":8633203,"howToApply":"https://careers.example.com/jobs/1?iisn=BuiltIn&iis=Job"}});
      </script>
    `
	got := extractBuiltInApplyURL(html)
	want := "https://careers.example.com/jobs/1?iisn=BuiltIn&iis=Job"
	if got != want {
		t.Errorf("extractBuiltInApplyURL = %q, want %q", got, want)
	}

	if extractBuiltInApplyURL("<html>no payload here</html>") != "" {
		t.Error("expected empty string when no payload present")
	}
}

func TestExtractJSONLd(t *testing.T) {
	html := `<script type="application/ld+json">{"@graph":[{"@type":"JobPosting","url":"https://x.com/1"},{"@type":"Org"}]}</script>`
	blocks := extractJSONLd(html)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 graph blocks, got %d", len(blocks))
	}
	// A single (non-@graph) object is wrapped in a one-element slice.
	single := extractJSONLd(`<script type="application/ld+json">{"@type":"JobPosting","url":"https://y.com/2"}</script>`)
	if len(single) != 1 {
		t.Fatalf("expected 1 block for single object, got %d", len(single))
	}
}

func TestFindValuesDeep(t *testing.T) {
	data := map[string]any{
		"url": "https://a.com",
		"nested": map[string]any{
			"sameAs":  "https://b.com",
			"ignored": 42,
			"deeper":  []any{map[string]any{"applyUrl": "https://c.com"}},
		},
	}
	var found []string
	findValuesDeep(data, map[string]bool{"url": true, "sameAs": true, "applyUrl": true}, &found)
	want := map[string]bool{"https://a.com": true, "https://b.com": true, "https://c.com": true}
	if len(found) != 3 {
		t.Fatalf("findValuesDeep found %v, want 3 values", found)
	}
	for _, f := range found {
		if !want[f] {
			t.Errorf("unexpected value %q", f)
		}
	}
}

func TestExtractCandidateURLs(t *testing.T) {
	html := `
		<html>
		  <script type="application/ld+json">{"@type":"JobPosting","applyUrl":"https://boards.greenhouse.io/acme/jobs/1"}</script>
		  <a href="/careers">Careers</a>
		  <a href="https://boards.greenhouse.io/acme/jobs/1">Apply</a>
		</html>`
	got := extractCandidateURLs(html, "https://www.acme.com/jobs/x")
	// Relative href resolves against the source; the greenhouse URL appears once
	// despite being present twice (JSON-LD + anchor).
	if !slices.Contains(got, "https://www.acme.com/careers") {
		t.Errorf("expected resolved relative URL, got %v", got)
	}
	count := 0
	for _, u := range got {
		if u == "https://boards.greenhouse.io/acme/jobs/1" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("greenhouse URL should be de-duplicated, appeared %d times in %v", count, got)
	}
}

func TestExtractRemoteOkApplyURLs(t *testing.T) {
	src := "https://remoteok.com/remote-jobs/remote-sre-ujet-1131206"
	got := extractRemoteOkApplyURLs(`<a href="/l/999">apply</a>`, src)
	want := []string{
		"https://remoteok.com/l/1131206", // id from the source URL
		"https://remoteok.com/l/999",     // id from the href
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("extractRemoteOkApplyURLs = %v, want %v", got, want)
	}
}
