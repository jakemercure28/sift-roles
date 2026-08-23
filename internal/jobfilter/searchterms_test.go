package jobfilter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The Go matcher must agree with the worker's matchesSearchTerms on the
// worker's own committed golden (scraper-service/test/fixtures/search): board
// mode moves term filtering from the worker to Go, so any divergence between
// the two implementations silently changes which jobs a tenant receives.
func TestMatchesSearchTermsAgreesWithWorkerGolden(t *testing.T) {
	// Same inputs as scraper-service/test/search.test.ts.
	terms := []string{"devops", "platform engineer", "sre", "vp", "tax", "investment"}

	raw, err := os.ReadFile(filepath.Join("..", "..", "scraper-service", "test", "fixtures", "search", "results.json"))
	if err != nil {
		t.Fatalf("read worker golden: %v", err)
	}
	var cases []struct {
		Text    string `json:"text"`
		Matched bool   `json:"matched"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("parse worker golden: %v", err)
	}
	if len(cases) < 5 {
		t.Fatalf("golden looks truncated: %d cases", len(cases))
	}
	for _, c := range cases {
		if got := MatchesSearchTerms(c.Text, terms); got != c.Matched {
			t.Errorf("MatchesSearchTerms(%q) = %v, worker golden says %v", c.Text, got, c.Matched)
		}
	}
}

func TestMatchesSearchTermsEdgeCases(t *testing.T) {
	if MatchesSearchTerms("anything", nil) {
		t.Error("no terms must match nothing")
	}
	if MatchesSearchTerms("", []string{"devops"}) {
		t.Error("empty text must not match")
	}
	if MatchesSearchTerms("syntax wizard", []string{"tax"}) {
		t.Error("short term must not match inside another word")
	}
	if !MatchesSearchTerms("Tax, Senior", []string{"tax"}) {
		t.Error("short term must match on a word boundary")
	}
}
