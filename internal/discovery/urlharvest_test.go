package discovery

import "testing"

func TestParseATSBoardURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		wantOK   bool
		platform string
		slug     string
		workday  string // "sub/board" when platform is workday
	}{
		{"greenhouse job-boards", "https://job-boards.greenhouse.io/affirm/jobs/7708927003", true, "greenhouse", "affirm", ""},
		{"greenhouse boards+query", "https://boards.greenhouse.io/andurilindustries/jobs/4965592007?gh_jid=4965592007", true, "greenhouse", "andurilindustries", ""},
		{"greenhouse embed for=", "https://boards.greenhouse.io/embed/job_app?for=stripe&token=123", true, "greenhouse", "stripe", ""},
		{"lever", "https://jobs.lever.co/palantir/74748e0e-a0bc-4c53-a636-ef14e36d9b5e", true, "lever", "palantir", ""},
		{"ashby", "https://jobs.ashbyhq.com/openai/1234-5678", true, "ashby", "openai", ""},
		{"workday", "https://capitalone.wd12.myworkdayjobs.com/wday/cxs/capitalone/Capital_One/job/123", true, "workday", "", "capitalone/Capital_One"},
		// SmartRecruiters identifiers are case-sensitive and must be preserved verbatim.
		{"smartrecruiters jobs", "https://jobs.smartrecruiters.com/BoschGroup/744000-engineer", true, "smartrecruiters", "BoschGroup", ""},
		{"smartrecruiters careers", "https://careers.smartrecruiters.com/Freshworks/12345", true, "smartrecruiters", "Freshworks", ""},
		{"smartrecruiters app route skipped", "https://jobs.smartrecruiters.com/oneclick-ui/foo", false, "", "", ""},
		{"smartrecruiters robots skipped", "https://jobs.smartrecruiters.com/robots.txt", false, "", "", ""},

		{"not an ATS host", "https://www.linkedin.com/jobs/view/123", false, "", "", ""},
		{"greenhouse no slug", "https://job-boards.greenhouse.io/", false, "", "", ""},
		{"numeric segment rejected", "https://jobs.lever.co/12345/abc", false, "", "", ""},
		{"blank", "", false, "", "", ""},
		{"greenhouse embed no for", "https://boards.greenhouse.io/embed/job_app", false, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseATSBoardURL(tt.url)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (board=%+v)", ok, tt.wantOK, got)
			}
			if !tt.wantOK {
				return
			}
			if got.Platform != tt.platform {
				t.Errorf("platform = %q, want %q", got.Platform, tt.platform)
			}
			if tt.platform == "workday" {
				if got.Workday == nil {
					t.Fatalf("workday entry nil")
				}
				if key := WorkdayKey(*got.Workday); key != tt.workday {
					t.Errorf("workday key = %q, want %q", key, tt.workday)
				}
				return
			}
			if got.Slug != tt.slug {
				t.Errorf("slug = %q, want %q", got.Slug, tt.slug)
			}
		})
	}
}

// SmartRecruiters slugs are case-sensitive, so the dedup key must keep case
// variants distinct (otherwise a wrong-case twin could evict the real board and
// then fail verification). API platforms stay case-folded.
func TestBoardKeyCaseSensitivity(t *testing.T) {
	a := BoardKey(VerifiedBoard{Platform: "smartrecruiters", Slug: "1Huddle"})
	b := BoardKey(VerifiedBoard{Platform: "smartrecruiters", Slug: "1HUDDLE"})
	if a == b {
		t.Fatalf("SmartRecruiters case variants collapsed: %q == %q", a, b)
	}
	gh1 := BoardKey(VerifiedBoard{Platform: "greenhouse", Slug: "Stripe"})
	gh2 := BoardKey(VerifiedBoard{Platform: "greenhouse", Slug: "stripe"})
	if gh1 != gh2 {
		t.Fatalf("greenhouse keys should case-fold: %q != %q", gh1, gh2)
	}
}
