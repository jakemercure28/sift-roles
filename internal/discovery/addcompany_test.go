package discovery

import "testing"

func TestSlugFromATSURL(t *testing.T) {
	cases := []struct {
		url      string
		platform string
		slug     string
		nilOK    bool
	}{
		{"https://boards.greenhouse.io/glossier", "greenhouse", "glossier", false},
		{"https://job-boards.greenhouse.io/acme/jobs/123", "greenhouse", "acme", false},
		{"https://jobs.lever.co/farfetch", "lever", "farfetch", false},
		{"https://api.lever.co/v0/postings/farfetch?mode=json", "lever", "farfetch", false},
		{"https://jobs.ashbyhq.com/notion", "ashby", "notion", false},
		{"https://notion.ashbyhq.com", "ashby", "notion", false},
		// Workday is handled separately; not an API board.
		{"https://jcrew.wd1.myworkdayjobs.com/jcrew", "", "", true},
		{"not a url at all", "", "", true},
		{"https://example.com/careers", "", "", true},
	}
	for _, c := range cases {
		got := slugFromATSURL(c.url)
		if c.nilOK {
			if got != nil {
				t.Errorf("slugFromATSURL(%q) = %+v, want nil", c.url, got)
			}
			continue
		}
		if got == nil {
			t.Errorf("slugFromATSURL(%q) = nil, want %s/%s", c.url, c.platform, c.slug)
			continue
		}
		if got.Platform != c.platform || got.Slug != c.slug {
			t.Errorf("slugFromATSURL(%q) = %s/%s, want %s/%s", c.url, got.Platform, got.Slug, c.platform, c.slug)
		}
	}
}

func TestAddAPIBoardDedup(t *testing.T) {
	s := emptySuggested()
	if added, already := addAPIBoard(&s, "greenhouse", "Acme"); !added || already {
		t.Fatalf("first add: added=%v already=%v, want true/false", added, already)
	}
	// Slug-variant aware: "acme" keys the same as "Acme".
	if added, already := addAPIBoard(&s, "greenhouse", "acme"); added || !already {
		t.Fatalf("dup add: added=%v already=%v, want false/true", added, already)
	}
	if len(s.Greenhouse) != 1 {
		t.Fatalf("greenhouse list = %v, want one entry", s.Greenhouse)
	}
	if apiList(&s, "workday") != nil {
		t.Errorf("apiList(workday) should be nil (not an API platform)")
	}
}

func TestAddWorkdayBoardDedup(t *testing.T) {
	s := emptySuggested()
	e := WorkdayEntry{Sub: "jcrew", WD: 1, Board: "jcrew", Label: "J.Crew"}
	if added, already := addWorkdayBoardEntry(&s, e); !added || already {
		t.Fatalf("first add: added=%v already=%v, want true/false", added, already)
	}
	if added, already := addWorkdayBoardEntry(&s, e); added || !already {
		t.Fatalf("dup add: added=%v already=%v, want false/true", added, already)
	}
	// A second brand board on the same tenant coexists.
	e2 := WorkdayEntry{Sub: "jcrew", WD: 1, Board: "MadewellCareers", Label: "Madewell"}
	if added, _ := addWorkdayBoardEntry(&s, e2); !added {
		t.Fatalf("second brand board should be added")
	}
	if len(s.Workday) != 2 {
		t.Fatalf("workday list = %v, want two entries", s.Workday)
	}
}
