package dashboard

import (
	"reflect"
	"testing"

	"job-search-automation/internal/db"
)

// TestFilteredJobsLightSearchParity pins the SQL-pushed text search to the prior
// in-memory behavior: fetchFilteredJobsLightSearch must return exactly the rows
// (and order) that fetchFilteredJobs + applyDashboardSearch produced, so moving the
// substring match into Postgres to cut egress does not change what the user sees.
// Covers description/reasoning-only matches (the light rows carry no such text, so
// the match must run in SQL), LIKE-wildcard escaping, case-insensitivity, and the
// min-score floor (NULL scores pass).
func TestFilteredJobsLightSearchParity(t *testing.T) {
	repo, conn := dataLayerRepo(t)

	score := func(n int) *int { return &n }
	jobs := []db.ListedJob{
		{ID: "j1", Title: "Google SRE", Description: "kubernetes terraform", Score: score(8), Status: "pending"},
		{ID: "j2", Title: "Acme Dev", Description: "c++ and python", Score: score(5), Status: "pending"},
		{ID: "j3", Title: "Data Co", Description: "100% remote role", Score: score(9), Status: "pending"},
		{ID: "j4", Title: "Foo", Company: "Bar", Description: "the snake_case style", Score: score(3), Status: "pending"},
		{ID: "j5", Title: "Quiet", Reasoning: "strong match for google", Status: "pending"}, // NULL score, match only in reasoning
		{ID: "j6", Title: "Zeta", Description: "GOOGLE cloud platform", Score: score(2), Status: "pending"},
		{ID: "j7", Title: "Pat A", Description: "the a%e token", Score: score(6), Status: "pending"}, // literal percent
		{ID: "j8", Title: "Pat B", Description: "axxe value", Score: score(6), Status: "pending"},    // would over-match if % unescaped
		{ID: "j9", Title: "Und B", Description: "snakeXcase", Score: score(6), Status: "pending"},    // would over-match if _ unescaped
	}
	for _, j := range jobs {
		seedFullJob(t, conn, j)
	}

	reference := func(q string, minScore int) []string {
		full, err := fetchFilteredJobs(repo, "all", "score")
		if err != nil {
			t.Fatalf("fetchFilteredJobs: %v", err)
		}
		return ids(applyDashboardSearch(full, SearchOptions{Q: q, MinScore: minScore}))
	}
	pushed := func(q string, minScore int) []string {
		rows, err := fetchFilteredJobsLightSearch(repo, "all", "score", SearchOptions{Q: q, MinScore: minScore})
		if err != nil {
			t.Fatalf("fetchFilteredJobsLightSearch: %v", err)
		}
		return ids(rows)
	}

	cases := []struct {
		name     string
		q        string
		minScore int
	}{
		{"title and reasoning and desc", "google", 1},
		{"case insensitive", "GOOGLE", 1},
		{"score floor drops low non-null, keeps null", "google", 5},
		{"plus is literal in like", "c++", 1},
		{"percent escaped to literal", "a%e", 1},
		{"underscore escaped to literal", "snake_case", 1},
		{"multi word within a field", "remote role", 1},
		{"no match", "zzzznomatch", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := reference(c.q, c.minScore)
			got := pushed(c.q, c.minScore)
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("q=%q minScore=%d: pushed=%v, in-memory reference=%v", c.q, c.minScore, got, want)
			}
		})
	}

	// Guard the escaping cases actually exercise the wildcard: a%e must select j7
	// only (not j8), and snake_case must select j4 only (not j9). If escaping
	// regressed these would over-match and the parity check above would still pass
	// only because the reference also uses literal matching, so assert explicitly.
	if got := pushed("a%e", 1); !reflect.DeepEqual(got, []string{"j7"}) {
		t.Fatalf(`pushed("a%%e") = %v, want [j7]`, got)
	}
	if got := pushed("snake_case", 1); !reflect.DeepEqual(got, []string{"j4"}) {
		t.Fatalf(`pushed("snake_case") = %v, want [j4]`, got)
	}
}
