package ats

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// These fuzz targets harden the parsers that consume untrusted job-board data
// (recruiter-pasted URLs, scraped HTML, aggregator redirects). Go fails a fuzz
// target automatically on any panic, so the baseline guarantee — "never crash
// on garbage input" — comes for free. On top of that, each target asserts the
// output invariants the rest of the package relies on, turning the fuzzer into
// a property test that also catches silent corruption from future edits.
//
// They run their seed corpus during a normal `go test` (and so in CI). To
// actually generate inputs, run e.g.:
//
//	go test -run '^$' -fuzz=FuzzParseWorkdayURL -fuzztime=30s ./internal/ats/

// commonURLSeeds are adversarial strings fed to every URL parser, on top of the
// platform-specific valid seeds each target adds.
var commonURLSeeds = []string{
	"",
	"https://",
	"http://",
	"://",
	"https:///",
	"https://example.com",
	"https://example.com/",
	"//no-scheme.example.com/jobs/1",
	"not a url at all",
	"%%%",
	"https://example.com/%zz",
	"https://例え.テスト/仕事/123",
	strings.Repeat("/a", 5000),
	strings.Repeat("x", 20000),
}

// allDigits reports whether s is non-empty and every rune is an ASCII digit.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func FuzzParseGreenhouseURL(f *testing.F) {
	seeds := append([]string{
		"https://job-boards.greenhouse.io/acme/jobs/123",
		"https://boards.greenhouse.io/acme/jobs/456",
		"https://careers.example.com/job?gh_jid=789",
		"https://careers.example.com/job?gh_jid=",
		"greenhouse.io//jobs/123",
	}, commonURLSeeds...)
	for _, s := range seeds {
		f.Add(s, "Acme Inc")
		f.Add(s, "")
	}
	f.Fuzz(func(t *testing.T, rawURL, fallbackCompany string) {
		ref := parseGreenhouseURL(rawURL, fallbackCompany)
		if ref == nil {
			return
		}
		if ref.BoardToken == "" {
			t.Errorf("non-nil ref with empty BoardToken: url=%q company=%q", rawURL, fallbackCompany)
		}
		if !allDigits(ref.JobID) {
			t.Errorf("JobID is not all-digits: %q (url=%q)", ref.JobID, rawURL)
		}
	})
}

func FuzzParseWorkdayURL(f *testing.F) {
	seeds := append([]string{
		"https://ffive.wd5.myworkdayjobs.com/f5jobs/job/Seattle/SRE-III_RP1037204",
		"https://acme.myworkdayjobs.com/en-US/External/job/Remote/Engineer_R1",
		"https://acme.myworkdayjobs.com/en-US/",
		"https://acme.myworkdayjobs.com/en-US/jobs",
		"https://acme.wd1.myworkdayjobs.com",
		"https://x.myworkdayjobs.com/en/en-US/jobs/job",
	}, commonURLSeeds...)
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, rawURL string) {
		ref := parseWorkdayURL(rawURL)
		if ref == nil {
			return
		}
		if ref.Subdomain == "" || ref.Host == "" {
			t.Errorf("non-nil ref missing host parts: %+v (url=%q)", ref, rawURL)
		}
		if ref.Board == "" {
			t.Errorf("non-nil ref with empty Board: %+v (url=%q)", ref, rawURL)
		}
		// fetch.go builds the CXS path from ExternalPath; it must be a real
		// rooted sub-path, never "" or a bare "/".
		if !strings.HasPrefix(ref.ExternalPath, "/") || ref.ExternalPath == "/" {
			t.Errorf("invalid ExternalPath %q (url=%q)", ref.ExternalPath, rawURL)
		}
	})
}

func FuzzParseAshbyURL(f *testing.F) {
	seeds := append([]string{
		"https://jobs.ashbyhq.com/acme/12345678-1234-1234-1234-123456789abc",
		"https://jobs.ashbyhq.com/acme/not-a-uuid",
		"https://jobs.ashbyhq.com/acme",
	}, commonURLSeeds...)
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, rawURL string) {
		ref := parseAshbyURL(rawURL)
		if ref == nil {
			return
		}
		if ref.BoardToken == "" {
			t.Errorf("non-nil ref with empty BoardToken (url=%q)", rawURL)
		}
		if len(ref.JobID) != 36 {
			t.Errorf("JobID not 36 chars: %q len=%d (url=%q)", ref.JobID, len(ref.JobID), rawURL)
		}
	})
}

func FuzzParseLeverURL(f *testing.F) {
	seeds := append([]string{
		"https://jobs.lever.co/acme/12345678-1234-1234-1234-123456789abc",
		"https://jobs.lever.co/acme/not-a-uuid",
		"https://jobs.lever.co/acme",
	}, commonURLSeeds...)
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, rawURL string) {
		ref := parseLeverURL(rawURL)
		if ref == nil {
			return
		}
		if ref.Company == "" {
			t.Errorf("non-nil ref with empty Company (url=%q)", rawURL)
		}
		if len(ref.JobID) != 36 {
			t.Errorf("JobID not 36 chars: %q len=%d (url=%q)", ref.JobID, len(ref.JobID), rawURL)
		}
	})
}

func FuzzClassifyUnsupportedURL(f *testing.F) {
	seeds := append([]string{
		"https://ats.rippling.com/acme/jobs",
		"https://apply.workable.com/acme/",
		"https://boards.icims.com/jobs",
		"recruiter@example.com",
		"https://www.linkedin.com/jobs/view/123",
	}, commonURLSeeds...)
	for _, s := range seeds {
		f.Add(s)
	}
	// No structured output to assert; the value is proving it never panics on
	// the url.Parse / lowercasing of arbitrary input.
	f.Fuzz(func(t *testing.T, rawURL string) {
		_ = classifyUnsupportedURL(rawURL)
	})
}

func FuzzNormalizeSlugPart(f *testing.F) {
	for _, s := range []string{
		"", "Acme Inc", "AT&T", "Foo (Bar) LLC", "Über Technologies",
		"123 Numbers", "---", "a.b.c", strings.Repeat("x ", 5000),
		"\x00\x01\x02", "日本語 Co",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, value string) {
		out := normalizeSlugPart(value)
		// reNonAlnum strips everything outside [a-z0-9], so the result must too.
		for _, r := range out {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
				t.Errorf("slug %q contains non-alnum rune %q (input=%q)", out, r, value)
			}
		}
	})
}

func FuzzDecodeEntities(f *testing.F) {
	for _, s := range []string{
		"", "&amp;", "&amp;amp;", "&#169;", "&#x1F600;", "\\u00e9",
		"&notareal; &lt;tag&gt;", "&#;", "&#xZZ;", "\\uZZZZ",
		strings.Repeat("&amp;", 1000), "&#x110000;", "&#1114112;",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := decodeEntities(s)
		// Garbage in, garbage out is fine, but valid UTF-8 must stay valid: a
		// surrogate/out-of-range entity (e.g. &#xD800;) must collapse to U+FFFD,
		// never emit raw invalid bytes.
		if utf8.ValidString(s) && !utf8.ValidString(out) {
			t.Errorf("decodeEntities turned valid UTF-8 %q into invalid output", s)
		}
	})
}

func FuzzStripHTML(f *testing.F) {
	for _, s := range []string{
		"", "<p>hello</p>", "<b>&amp;</b> world", "no tags here",
		"<<<>>>", "&#x1F600; <span>", strings.Repeat("<a>x</a>", 5000),
		strings.Repeat("é", maxDescriptionLength+10),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		out := stripHTML(text)
		if utf8.ValidString(text) && !utf8.ValidString(out) {
			t.Errorf("stripHTML turned valid UTF-8 %q into invalid output", text)
		}
		// Truncation is rune-based and must respect the cap.
		if n := utf8.RuneCountInString(out); n > maxDescriptionLength {
			t.Errorf("stripHTML output is %d runes, exceeds cap %d", n, maxDescriptionLength)
		}
	})
}
