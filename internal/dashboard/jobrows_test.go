package dashboard

// The committed .golden fixtures here are the source of truth for the Go
// renderer's output; this test asserts the renderer byte-for-byte against them.

import (
	"strings"
	"testing"
)

func ptr(n int) *int { return &n }

func goldenJobs() []Job {
	return []Job{
		{
			ID: "gh-1", Title: "Senior Platform Engineer", Company: "Acme & Co",
			URL: "https://job-boards.greenhouse.io/acme/jobs/123", Platform: "greenhouse",
			Location: "Remote - San Francisco, CA", PostedAt: "2026-06-01", CreatedAt: "2026-06-01",
			Description: "Comp: $180k - $220k. Build platform things.",
			Score:       ptr(9), Reasoning: "Strong match but missing Go experience.",
			Status: "pending", Stage: "",
		},
		{
			ID: "wd-2", Title: "DevOps Engineer <Cloud>", Company: "Globex",
			URL: "https://globex.wd1.myworkdayjobs.com/job/456", Platform: "workday",
			Location: "California - Pleasanton", PostedAt: "2026-05-20", CreatedAt: "2026-05-20",
			Description: "No salary listed.", Score: ptr(6), Reasoning: "",
			Status: "applied", Stage: "phone_screen", AppliedAt: "2026-06-03",
		},
		{
			ID: "lv-3", Title: "SRE", Company: "O'Brien Labs", URL: "", Platform: "lever",
			Location: "United States, Austin", PostedAt: "2026-05-15", CreatedAt: "2026-05-15",
			Description: "Up to $200,000 a year.", Score: nil, Reasoning: "",
			Status: "rejected", Stage: "rejected", RejectedFromStage: "interview",
			RejectedAt: "2026-06-05T10:00:00Z",
		},
	}
}

func TestRenderJobTableParity(t *testing.T) {
	jobs := goldenJobs()
	appliedByCompany := map[string]int{"globex": 2}
	companyTags := map[string][]string{
		"acme & co": {"remote", "yc"},
		"globex":    {"agency"},
	}
	pagination := &Pagination{Page: 1, PageSize: 25, TotalItems: 3, TotalPages: 1, StartItem: 1, EndItem: 3}

	got := RenderJobTable(jobs, appliedByCompany, companyTags, "all", "score", pagination, SearchOptions{})
	assertGolden(t, "job-table.html.golden", got)
}

func TestRenderJobTableEmptyParity(t *testing.T) {
	got := RenderJobTable(nil, map[string]int{}, map[string][]string{}, "applied", "score", nil, SearchOptions{})
	assertGolden(t, "job-table-empty.html.golden", got)
}

func TestRenderJobTableOmitsAppliedBadges(t *testing.T) {
	jobs := []Job{
		{
			ID: "applied-1", Title: "Applied Role", Company: "Acme",
			Status: "pending", Stage: "", AppliedAt: "2026-06-03",
		},
	}
	got := RenderJobTable(jobs, map[string]int{"acme": 2}, map[string][]string{}, "all", "score", nil, SearchOptions{})
	if strings.Contains(got, "complexity-badge applied-date") {
		t.Fatal("rendered job card still includes the applied-date badge")
	}
	if strings.Contains(got, "complexity-badge applied-co") {
		t.Fatal("rendered job card still includes the applied-company badge")
	}
}

func TestNormalizeLocationCases(t *testing.T) {
	cases := map[string]string{
		"Remote - San Francisco, CA": "Remote, San Francisco, CA",
		"California - Pleasanton":    "Pleasanton, CA",
		"United States, Austin":      "Austin",
		"Remote":                     "Remote",
		"Hybrid - NYC":               "Hybrid",
		"San Francisco Bay Area":     "Bay Area",
		"United States":              "US",
		"WA - Seattle":               "Seattle, WA",
	}
	for in, want := range cases {
		if got := normalizeLocation(in); got != want {
			t.Errorf("normalizeLocation(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractSalaryCases(t *testing.T) {
	cases := map[string]string{
		"Comp: $180k - $220k.":  "$180k–$220k",
		"Up to $200,000 a year": "≤$200k",
		"From $150,000":         "$150k+",
		"$75/hr contract":       "$75/hr",
		"No salary listed.":     "",

		// bare numbers in salary prose (Nabis et al.)
		"Competitive salary Base Salary starting at 145,000- 165,000 Medical/Dental/Vision offered to all full-time employees 401(k) plan with a match.": "$145k–$165k",
		"The salary range for this full-time position is 141,600": "$142k",
		"Base salary starting at 152,000 for this level":          "$152k+",

		// USD written out instead of $
		"Salary USD 120,500 - 228,500":                                                "$121k–$229k",
		"The salary range is 152,000 USD - 218,500 USD for Level 3":                   "$152k–$219k",
		"Compensation is market competitive, with a range of USD 220,000-USD 250,000": "$220k–$250k",

		// non-USD currencies stay unextracted
		"Salary Range: CAD 145,000-220,000":                            "",
		"base pay range per year: 358,000 zł - 458,000 zł":             "",
		"The typical pay range for this role is: €65,000-€80,000":      "",
		"Total compensation: 600,000 - 1,000,000 DKK":                  "",
		"estimated base salary range is between 31,000 - 36,000 PLN":   "",
		"Target Base Salary Range: 38,000 - 63,000 CAD":                "",
		"the Base compensation range for this role is EU 65K - EU 81K": "",

		// bare numbers without salary context stay out
		"serving 100,000 to 200,000 users":       "",
		"up to 5 years of experience":            "",
		"Compensation includes 401k matching":    "",
		"a base pay process for 1,000 employees": "",
	}
	for in, want := range cases {
		if got := extractSalary(in); got != want {
			t.Errorf("extractSalary(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderJobTitleRejectsNonHTTPScheme(t *testing.T) {
	// Untrusted scraped/imported URLs must never reach an href unless http(s);
	// a "javascript:" scheme would otherwise render a clickable XSS link.
	for _, u := range []string{
		"javascript:alert(document.cookie)",
		"JavaScript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"vbscript:msgbox(1)",
		" javascript:alert(1)", // leading space, trimmed before the scheme check
	} {
		j := Job{ID: "x1", Title: "Pwn", Company: "Evil", URL: u}
		got := renderJobTitle(j, escapeHTML(j.ID), escapeHTML(j.Title))
		if strings.Contains(got, "<a href=") {
			t.Errorf("renderJobTitle(%q) emitted an href: %q", u, got)
		}
		if !strings.Contains(got, "job-title-missing-url") {
			t.Errorf("renderJobTitle(%q) should fall back to the no-URL button: %q", u, got)
		}
	}

	// Genuine http(s) URLs still render as links.
	for _, u := range []string{"https://example.com/jobs/1", "http://example.com/jobs/2"} {
		j := Job{ID: "x2", Title: "Real", Company: "Co", URL: u}
		got := renderJobTitle(j, escapeHTML(j.ID), escapeHTML(j.Title))
		if !strings.Contains(got, `<a href="`+u+`"`) {
			t.Errorf("renderJobTitle(%q) should render an href: %q", u, got)
		}
	}
}
