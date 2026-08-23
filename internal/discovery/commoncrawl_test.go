package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDecodeCrawlIDs(t *testing.T) {
	got, err := decodeCrawlIDs([]byte(`[{"id":"CC-MAIN-2026-25","name":"June 2026"},{"id":"CC-MAIN-2026-21"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "CC-MAIN-2026-25" || got[1] != "CC-MAIN-2026-21" {
		t.Fatalf("ids = %v, want [CC-MAIN-2026-25 CC-MAIN-2026-21]", got)
	}
	if _, err := decodeCrawlIDs([]byte(`[]`)); err == nil {
		t.Fatal("empty collection list should error")
	}
}

func TestCCFetchURLs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("url") == "empty.example/*" {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("No Captures found"))
			return
		}
		// One malformed line and two good ones; the parser must skip the junk.
		w.Write([]byte(
			`{"url":"https://job-boards.greenhouse.io/affirm/jobs/1"}` + "\n" +
				`not json` + "\n" +
				`{"url":"https://jobs.lever.co/palantir/abc"}` + "\n",
		))
	}))
	defer srv.Close()

	urls, err := ccFetchURLsFrom(context.Background(), srv.Client(), srv.URL, ccQuery{pattern: "greenhouse.io/*"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 2 {
		t.Fatalf("got %d urls, want 2: %v", len(urls), urls)
	}

	// 404 (no captures) is an empty result, not an error.
	empty, err := ccFetchURLsFrom(context.Background(), srv.Client(), srv.URL, ccQuery{pattern: "empty.example/*"}, 100)
	if err != nil {
		t.Fatalf("404 should be empty, got error: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("404 should yield no urls, got %v", empty)
	}
}
