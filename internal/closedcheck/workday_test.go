package closedcheck

import (
	"context"
	"encoding/json"
	"testing"
)

// searchPost fakes the CXS search endpoint: it records each searchText and
// answers from the term->body table (missing terms get an empty result).
func searchPost(t *testing.T, wantURL string, bodies map[string]string, calls *[]string) PostFetcher {
	t.Helper()
	return func(_ context.Context, rawURL string, body []byte) (*HTTPResult, error) {
		if wantURL != "" && rawURL != wantURL {
			t.Errorf("search URL = %q, want %q", rawURL, wantURL)
		}
		var req struct {
			SearchText string `json:"searchText"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("request body: %v", err)
		}
		*calls = append(*calls, req.SearchText)
		res, ok := bodies[req.SearchText]
		if !ok {
			res = `{"total":0,"jobPostings":[]}`
		}
		return &HTTPResult{StatusCode: 200, Body: []byte(res)}, nil
	}
}

func TestCheckWorkdayPostingOpen(t *testing.T) {
	var calls []string
	post := searchPost(t,
		"https://intel.wd1.myworkdayjobs.com/wday/cxs/intel/External/jobs",
		map[string]string{
			// The board answers with a case-drifted externalPath; the match
			// must be case-insensitive, like the diff's URL survival check.
			"JR0284118": `{"total":1,"jobPostings":[{"externalPath":"/JOB/US-Arizona-Phoenix/Yield-Engineer_JR0284118"}]}`,
		}, &calls)

	got, err := CheckWorkdayPosting(context.Background(), post,
		"https://intel.wd1.myworkdayjobs.com/en-US/External/job/US-Arizona-Phoenix/Yield-Engineer_JR0284118")
	if err != nil || got != "open" {
		t.Errorf("verdict = %q, %v; want open", got, err)
	}
	if len(calls) != 1 || calls[0] != "JR0284118" {
		t.Errorf("search terms = %v, want [JR0284118]", calls)
	}
}

func TestCheckWorkdayPostingClosed(t *testing.T) {
	var calls []string
	post := searchPost(t, "", map[string]string{
		// The search matches an unrelated posting; only an exact path match
		// counts as this posting being live.
		"JR100": `{"total":1,"jobPostings":[{"externalPath":"/job/Remote/Other-Role_JR1000"}]}`,
	}, &calls)

	got, err := CheckWorkdayPosting(context.Background(), post,
		"https://acme.wd5.myworkdayjobs.com/en-US/careers/job/Remote/Gone-Role_JR100")
	if err != nil || got != "closed" {
		t.Errorf("verdict = %q, %v; want closed", got, err)
	}
	if len(calls) != 1 {
		t.Errorf("calls = %v, want one (no variant suffix to retry)", calls)
	}
}

// TestCheckWorkdayPostingVariantSuffixRetry: Workday's search indexes the base
// req id, so a posting-variant URL (_R0134153-1) can miss on the full term and
// hit on the stripped core; the core hit still proves THIS posting live via
// the externalPath match.
func TestCheckWorkdayPostingVariantSuffixRetry(t *testing.T) {
	var calls []string
	post := searchPost(t, "", map[string]string{
		"R0134153": `{"total":1,"jobPostings":[{"externalPath":"/job/San-Jose/Staff-Engineer_R0134153-1"}]}`,
	}, &calls)

	got, err := CheckWorkdayPosting(context.Background(), post,
		"https://paypal.wd1.myworkdayjobs.com/en-US/Jobs/job/San-Jose/Staff-Engineer_R0134153-1")
	if err != nil || got != "open" {
		t.Errorf("verdict = %q, %v; want open", got, err)
	}
	if len(calls) != 2 || calls[0] != "R0134153-1" || calls[1] != "R0134153" {
		t.Errorf("search terms = %v, want [R0134153-1 R0134153]", calls)
	}
}

// TestCheckWorkdayPostingLongDigitTailNotStripped: nike-style req ids ARE a
// letter plus a long digit run (R-86901); stripping that tail would search a
// bare "R" and match the whole board.
func TestCheckWorkdayPostingLongDigitTailNotStripped(t *testing.T) {
	var calls []string
	post := searchPost(t, "", nil, &calls)

	got, err := CheckWorkdayPosting(context.Background(), post,
		"https://nike.wd1.myworkdayjobs.com/en-US/nke/job/Beaverton-Oregon/Senior-Planner_R-86901")
	if err != nil || got != "closed" {
		t.Errorf("verdict = %q, %v; want closed", got, err)
	}
	if len(calls) != 1 || calls[0] != "R-86901" {
		t.Errorf("search terms = %v, want [R-86901]", calls)
	}
}

func TestCheckWorkdayPostingInconclusive(t *testing.T) {
	cases := []struct {
		name string
		post PostFetcher
	}{
		{"blocked", func(context.Context, string, []byte) (*HTTPResult, error) {
			return &HTTPResult{StatusCode: 403, Body: []byte("denied")}, nil
		}},
		{"transport failure", func(context.Context, string, []byte) (*HTTPResult, error) {
			return nil, nil
		}},
		{"garbage body", func(context.Context, string, []byte) (*HTTPResult, error) {
			return &HTTPResult{StatusCode: 200, Body: []byte("<html>waf</html>")}, nil
		}},
		{"broad match overflows page", func(context.Context, string, []byte) (*HTTPResult, error) {
			return &HTTPResult{StatusCode: 200, Body: []byte(`{"total":900,"jobPostings":[{"externalPath":"/job/X/Y_Z1"}]}`)}, nil
		}},
	}
	for _, tc := range cases {
		got, err := CheckWorkdayPosting(context.Background(), tc.post,
			"https://acme.wd5.myworkdayjobs.com/en-US/External/job/Remote/Role_JR9")
		if err != nil || got != "" {
			t.Errorf("%s: verdict = %q, %v; want inconclusive", tc.name, got, err)
		}
	}
}

func TestCheckWorkdayPostingIneligibleURLs(t *testing.T) {
	post := func(context.Context, string, []byte) (*HTTPResult, error) {
		t.Error("ineligible URL must not be probed")
		return nil, nil
	}
	for _, u := range []string{
		"https://boards.greenhouse.io/acme/jobs/123",
		"https://acme.wd5.myworkdayjobs.com/en-US/External",
		"https://acme.wd5.myworkdayjobs.com/en-US/External/job/Remote/NoReqIdHere",
		"http://acme.wd5.myworkdayjobs.com/en-US/External/job/Remote/Role_JR9",
		"",
	} {
		if got, err := CheckWorkdayPosting(context.Background(), post, u); err != nil || got != "" {
			t.Errorf("%q: verdict = %q, %v; want inconclusive", u, got, err)
		}
	}
}
