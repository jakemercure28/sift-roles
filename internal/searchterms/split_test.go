package searchterms

import (
	"reflect"
	"testing"

	"job-search-automation/internal/jobfilter"
)

func TestExpandDerivations(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "strips generic suffix to the domain core",
			in:   []string{"DevOps Engineer"},
			want: []string{"devops engineer", "devops"},
		},
		{
			name: "multi-word core keeps a phrase and adds the head noun",
			in:   []string{"Site Reliability Engineer"},
			want: []string{"site reliability engineer", "site reliability", "reliability"},
		},
		{
			name: "seniority stripped",
			in:   []string{"Senior Platform Engineer"},
			want: []string{"senior platform engineer", "platform"},
		},
		{
			name: "no generic words: only the phrase (nothing invented)",
			in:   []string{"Registered Nurse"},
			want: []string{"registered nurse"},
		},
		{
			name: "non-tech role: generic role suffix stripped",
			in:   []string{"Financial Analyst", "Marketing Director"},
			want: []string{"financial analyst", "financial", "marketing director", "marketing"},
		},
		{
			name: "dedupes across terms",
			in:   []string{"DevOps Engineer", "devops engineer"},
			want: []string{"devops engineer", "devops"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Expand(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Expand(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestExpandNeverRegresses: every raw term still matches whatever it matched
// before (the full phrase is always retained), and the widening is additive.
func TestExpandNeverRegresses(t *testing.T) {
	raw := []string{"devops engineer", "platform engineer"}
	widened := Expand(raw)
	rawSet := map[string]bool{}
	for _, r := range Expand(raw) {
		rawSet[r] = true
	}
	for _, r := range raw {
		if !rawSet[r] {
			t.Errorf("widened set dropped raw term %q: %v", r, widened)
		}
	}
}

// TestExpandFixesTheRealMiss: the Nominal titles that raw phrase-terms missed
// now match through the derived core, while a genuinely unrelated title stays
// unmatched.
func TestExpandFixesTheRealMiss(t *testing.T) {
	raw := []string{"devops engineer", "platform engineer"}
	widened := Expand(raw)

	nowMatches := "Devops - Internal Platform & Tools"
	if jobfilter.MatchesSearchTerms(nowMatches, raw) {
		t.Fatalf("precondition: raw terms should not match %q", nowMatches)
	}
	if !jobfilter.MatchesSearchTerms(nowMatches, widened) {
		t.Errorf("widened terms still miss %q: %v", nowMatches, widened)
	}

	// Widening the words the user typed must not start matching unrelated work.
	unrelated := "Enterprise Account Executive"
	if jobfilter.MatchesSearchTerms(unrelated, widened) {
		t.Errorf("widened terms wrongly match unrelated role %q: %v", unrelated, widened)
	}
}

func TestExpandCapsOutput(t *testing.T) {
	in := make([]string, MaxTerms+50)
	for i := range in {
		in[i] = "role" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + " specialist"
	}
	if got := Expand(in); len(got) > MaxTerms {
		t.Errorf("Expand produced %d terms, want <= %d", len(got), MaxTerms)
	}
}
