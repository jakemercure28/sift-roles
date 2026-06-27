package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRenderReportProblemPageParity(t *testing.T) {
	assertGolden(t, "report-problem.html.golden", RenderReportProblemPage())
}

func TestReportProblemRouteNative(t *testing.T) {
	ts, _, _ := fragmentServer(t) // proxy must NOT be hit
	resp := get(t, ts.URL+"/report-problem", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{"<!DOCTYPE html>", "<title>" + BrandName + " - Report Problem</title>", `id="rp-form"`, `id="theme-vars"`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("report-problem page missing %q", want)
		}
	}
}

// newTriggerServer builds a front door whose scrape trigger points at triggerURL.
func newTriggerServer(t *testing.T, triggerURL string) *httptest.Server {
	t.Helper()
	repo := newRepo(t)
	srv, err := New(t.TempDir(), repo, nil, time.Second, 500, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	srv.SetScrapeTriggerURL(triggerURL)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestScrapeNow(t *testing.T) {
	// Not configured -> 503.
	ts := newTriggerServer(t, "")
	r := postJSON(t, ts.URL+"/api/scrape-now", "")
	if r.StatusCode != http.StatusServiceUnavailable {
		r.Body.Close()
		t.Fatalf("no-trigger status = %d, want 503", r.StatusCode)
	}
	r.Body.Close()

	cases := []struct {
		name       string
		upstream   int
		wantStatus int
		wantBody   string
	}{
		{"ok", 200, 200, `{"ok":true}`},
		{"busy", http.StatusConflict, http.StatusConflict, `{"ok":false,"busy":true}`},
		{"upstream-500", 500, http.StatusBadGateway, `{"ok":false,"error":"Scraper returned 500"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotUserID string
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					UserID string `json:"userId"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				gotUserID = body.UserID
				w.WriteHeader(c.upstream)
			}))
			defer backend.Close()
			ts := newTriggerServer(t, backend.URL)

			resp := postJSON(t, ts.URL+"/api/scrape-now", "")
			defer resp.Body.Close()
			if resp.StatusCode != c.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, c.wantStatus)
			}
			body, _ := io.ReadAll(resp.Body)
			if strings.TrimSpace(string(body)) != c.wantBody {
				t.Fatalf("body = %q, want %q", strings.TrimSpace(string(body)), c.wantBody)
			}
			if gotUserID != "local" {
				t.Fatalf("trigger userId = %q, want local", gotUserID)
			}
		})
	}

	// Unreachable upstream -> 502.
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	target := closed.URL
	closed.Close()
	tsDown := newTriggerServer(t, target)
	resp := postJSON(t, tsDown.URL+"/api/scrape-now", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("unreachable status = %d, want 502", resp.StatusCode)
	}
}
