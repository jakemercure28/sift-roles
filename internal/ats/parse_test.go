package ats

import (
	"reflect"
	"slices"
	"testing"
)

func TestNormalizePlatform(t *testing.T) {
	cases := map[string]string{
		"Ashby":                    "ashby",
		"GREENHOUSE":               "greenhouse",
		"job-boards.greenhouse.io": "greenhouse",
		"Lever":                    "lever",
		"myWorkdayJobs":            "workday",
		"Workday":                  "workday",
		"RemoteOK":                 "remoteok",
		"":                         "",
		"  Built In ":              "built in",
	}
	for in, want := range cases {
		if got := normalizePlatform(in); got != want {
			t.Errorf("normalizePlatform(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDisplayPlatform(t *testing.T) {
	cases := map[string]string{
		"ashby": "Ashby", "greenhouse": "Greenhouse", "lever": "Lever",
		"workday": "Workday", "RemoteOK": "RemoteOK", "": "",
	}
	for in, want := range cases {
		if got := displayPlatform(in); got != want {
			t.Errorf("displayPlatform(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsPrimaryPlatform(t *testing.T) {
	for _, p := range []string{"ashby", "greenhouse", "Lever", "WORKDAY", "myworkdayjobs"} {
		if !isPrimaryPlatform(p) {
			t.Errorf("isPrimaryPlatform(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"remoteok", "builtin", "", "linkedin"} {
		if isPrimaryPlatform(p) {
			t.Errorf("isPrimaryPlatform(%q) = true, want false", p)
		}
	}
}

func TestDetectAts(t *testing.T) {
	cases := []struct {
		url      string
		platform string
		company  string
	}{
		{"https://jobs.ashbyhq.com/acme/123", "Ashby", "acme"},
		{"https://boards.greenhouse.io/acme/jobs/1", "Greenhouse", "acme"},
		{"https://job-boards.greenhouse.io/acme/jobs/1", "Greenhouse", "acme"},
		{"https://jobs.lever.co/acme/abc", "Lever", "acme"},
		{"https://apply.workable.com/acme/j/1", "Workable", "acme"},
		{"https://acme.wd5.myworkdayjobs.com/jobs", "Workday", "acme"},
		{"https://acme.myworkdayjobs.com/jobs", "Workday", "acme"},
		{"https://ats.rippling.com/acme/jobs/1", "Rippling", "acme"},
		{"https://example.com/jobs?gh_jid=123", "Greenhouse", ""},
	}
	for _, c := range cases {
		got := detectAts(c.url)
		if got == nil {
			t.Errorf("detectAts(%q) = nil, want %s", c.url, c.platform)
			continue
		}
		if got.Platform != c.platform || got.Company != c.company {
			t.Errorf("detectAts(%q) = %+v, want %s/%s", c.url, got, c.platform, c.company)
		}
	}
	if detectAts("https://example.com/careers") != nil {
		t.Error("detectAts of a plain careers page should be nil")
	}
	if detectAts("") != nil {
		t.Error("detectAts('') should be nil")
	}
}

func TestDetectJobPlatform(t *testing.T) {
	cases := []struct {
		job  Job
		want string
	}{
		{Job{URL: "https://boards.greenhouse.io/acme/jobs/1"}, "greenhouse"},
		{Job{ID: "lever-abc", Platform: "Inbound"}, "lever"},
		{Job{ID: "ashby-xyz"}, "ashby"},
		{Job{ID: "workday-1"}, "workday"},
		{Job{ID: "rippling-1"}, "rippling"},
		{Job{Platform: "RemoteOK"}, "remoteok"},
		{Job{}, ""},
	}
	for _, c := range cases {
		if got := detectJobPlatform(c.job); got != c.want {
			t.Errorf("detectJobPlatform(%+v) = %q, want %q", c.job, got, c.want)
		}
	}
}

func TestParseGreenhouseURL(t *testing.T) {
	if ref := parseGreenhouseURL("https://job-boards.greenhouse.io/acme/jobs/123", ""); ref == nil ||
		ref.BoardToken != "acme" || ref.JobID != "123" {
		t.Errorf("standard parse = %+v", ref)
	}
	// gh_jid falls back to slugifying the company.
	if ref := parseGreenhouseURL("https://careers.example.com/job?gh_jid=456", "Acme Inc"); ref == nil ||
		ref.BoardToken != "acme" || ref.JobID != "456" {
		t.Errorf("gh_jid parse = %+v", ref)
	}
	if ref := parseGreenhouseURL("https://careers.example.com/job?gh_jid=456", ""); ref != nil {
		t.Errorf("gh_jid with no company should be nil, got %+v", ref)
	}
	if ref := parseGreenhouseURL("https://example.com/careers", "Acme"); ref != nil {
		t.Errorf("non-greenhouse url should be nil, got %+v", ref)
	}
}

func TestParseGreenhouseJob(t *testing.T) {
	if ref := parseGreenhouseJob(Job{ID: "greenhouse-789", Company: "Acme Inc"}); ref == nil ||
		ref.BoardToken != "acme" || ref.JobID != "789" {
		t.Errorf("id-based parse = %+v", ref)
	}
}

func TestParseAshbyURL(t *testing.T) {
	u := "https://jobs.ashbyhq.com/acme/12345678-1234-1234-1234-1234567890ab"
	if ref := parseAshbyURL(u); ref == nil || ref.BoardToken != "acme" ||
		ref.JobID != "12345678-1234-1234-1234-1234567890ab" {
		t.Errorf("ashby parse = %+v", ref)
	}
	if parseAshbyURL("https://jobs.ashbyhq.com/acme/not-a-uuid") != nil {
		t.Error("non-uuid ashby should be nil")
	}
}

func TestParseLeverURL(t *testing.T) {
	u := "https://jobs.lever.co/acme/abcdef12-3456-7890-abcd-ef1234567890"
	if ref := parseLeverURL(u); ref == nil || ref.Company != "acme" ||
		ref.JobID != "abcdef12-3456-7890-abcd-ef1234567890" {
		t.Errorf("lever parse = %+v", ref)
	}
}

// TestParseWorkdayURL mirrors the JS unit expectation in test/ats-resolver.test.js.
func TestParseWorkdayURL(t *testing.T) {
	got := parseWorkdayURL("https://ffive.wd5.myworkdayjobs.com/f5jobs/job/Seattle/SRE-III_RP1037204")
	want := &WorkdayRef{
		Subdomain:    "ffive",
		Host:         "ffive.wd5.myworkdayjobs.com",
		Board:        "f5jobs",
		ExternalPath: "/job/Seattle/SRE-III_RP1037204",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseWorkdayURL = %+v, want %+v", got, want)
	}
	// Locale segments are skipped to find the board.
	got2 := parseWorkdayURL("https://acme.myworkdayjobs.com/en-US/External/job/Remote/Engineer_R1")
	if got2 == nil || got2.Board != "External" || got2.ExternalPath != "/job/Remote/Engineer_R1" {
		t.Errorf("locale-skipping parse = %+v", got2)
	}
	if parseWorkdayURL("https://example.com/jobs") != nil {
		t.Error("non-workday host should be nil")
	}
}

// TestClassifyUnsupportedURL mirrors the JS unit expectation in test/ats-resolver.test.js.
func TestClassifyUnsupportedURL(t *testing.T) {
	cases := map[string]string{
		"https://ats.rippling.com/overland-ai/jobs/123":      "Rippling",
		"https://careers-americas.icims.com/jobs/24810/job":  "iCIMS",
		"https://apply.workable.com/acme/j/123":              "Workable",
		"https://search-careers.gm.com/en/jobs/jr-1/example": "Company Careers",
		"https://careers.draftkings.com/jobs/jr1/example":    "Company Careers",
		"https://www.linkedin.com/jobs/view/123":             "LinkedIn",
		"https://remoteok.com/remote-jobs/123":               "RemoteOK",
		"https://www.builtin.com/job/1":                      "Built In",
		"recruiter@example.com":                              "Email",
		"https://boards.greenhouse.io/acme/jobs/1":           "", // primary, not unsupported
		"": "",
	}
	for in, want := range cases {
		if got := classifyUnsupportedURL(in); got != want {
			t.Errorf("classifyUnsupportedURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeSlugPart(t *testing.T) {
	cases := map[string]string{
		"Acme Inc":          "acme",
		"Foo & Bar":         "fooandbar",
		"Example (Remote)":  "example",
		"UJET Technologies": "ujet",
		"Big Co Corp":       "big",
	}
	for in, want := range cases {
		if got := normalizeSlugPart(in); got != want {
			t.Errorf("normalizeSlugPart(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSlugCandidates(t *testing.T) {
	got := slugCandidates("Acme Foo Inc")
	// normalized "acmefoo", hyphenated "acme-foo", concatenated "acmefoo".
	wantHas := []string{"acmefoo", "acme-foo"}
	for _, w := range wantHas {
		if !slices.Contains(got, w) {
			t.Errorf("slugCandidates(Acme Foo Inc) = %v, missing %q", got, w)
		}
	}
	if u := slugCandidates("UJET"); !slices.Contains(u, "ujet") {
		t.Errorf("slugCandidates(UJET) = %v, want ujet", u)
	}
}

func TestGreenhouseBoardCandidates(t *testing.T) {
	if got := greenhouseBoardCandidates("Acme Inc"); !slices.Contains(got, "acme") {
		t.Errorf("greenhouseBoardCandidates(Acme Inc) = %v, want acme", got)
	}
	if got := greenhouseBoardCandidates("UJET Technologies"); !slices.Contains(got, "ujet") {
		t.Errorf("greenhouseBoardCandidates(UJET) = %v, want ujet", got)
	}
}

func TestTitleMatches(t *testing.T) {
	if !titleMatches("Senior Site Reliability Engineer", "Senior Site Reliability Engineer") {
		t.Error("exact match should be true")
	}
	if !titleMatches("Site Reliability Engineer - Platform", "Senior Site Reliability Engineer") {
		t.Error("token overlap (site, reliability) should match")
	}
	if titleMatches("Product Designer", "Senior Site Reliability Engineer") {
		t.Error("unrelated titles should not match")
	}
	if titleMatches("", "anything") || titleMatches("anything", "") {
		t.Error("blank titles should not match")
	}
}

func TestTitleTokens(t *testing.T) {
	got := titleTokens("Senior Staff Site Reliability Engineer (Remote)")
	// senior/staff/engineer/remote are stop tokens; <3-char tokens dropped.
	want := []string{"site", "reliability"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("titleTokens = %v, want %v", got, want)
	}
}
