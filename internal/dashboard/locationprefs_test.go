package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestEmbeddedMetrosMatchesRepo guards against drift between the embedded copy
// and config/metros.json (the Node source of truth during the transition).
func TestEmbeddedMetrosMatchesRepo(t *testing.T) {
	repoCopy, err := os.ReadFile(filepath.Join("..", "..", "config", "metros.json"))
	if err != nil {
		t.Skipf("config/metros.json not found: %v", err)
	}
	if string(repoCopy) != string(metrosJSON) {
		t.Fatal("internal/dashboard/metros.json has drifted from config/metros.json; re-copy it")
	}
}

func TestNormalizePrefs(t *testing.T) {
	// Unknown metros are dropped; includeUnknown defaults true; remoteOnly false.
	def := normalizePrefs(rawPrefs{})
	if len(def.Metros) != 0 || !def.IncludeUnknown || def.RemoteOnly {
		t.Fatalf("defaults = %+v", def)
	}
	// Metros must marshal as [] not null.
	if b, _ := json.Marshal(def); string(b) != `{"metros":[],"includeUnknown":true,"remoteOnly":false}` {
		t.Fatalf("default JSON = %s", b)
	}

	f := false
	tr := true
	got := normalizePrefs(rawPrefs{Metros: []string{"seattle", "not-a-metro"}, IncludeUnknown: &f, RemoteOnly: &tr})
	if len(got.Metros) != 1 || got.Metros[0] != "seattle" {
		t.Fatalf("metros filtering = %v", got.Metros)
	}
	if got.IncludeUnknown {
		t.Fatal("explicit includeUnknown=false should stick")
	}
	if !got.RemoteOnly {
		t.Fatal("explicit remoteOnly=true should stick")
	}
}

func TestLocationPrefsRouteRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	repo := newRepo(t)
	srv, err := New(t.TempDir(), repo, nil, time.Second, 500, dataDir, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// GET defaults before anything is saved.
	resp := get(t, ts.URL+"/api/location-prefs", nil)
	var got struct {
		Prefs  LocationPrefs   `json:"prefs"`
		Metros json.RawMessage `json:"metros"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if !got.Prefs.IncludeUnknown || got.Prefs.RemoteOnly || len(got.Prefs.Metros) != 0 {
		t.Fatalf("default prefs = %+v", got.Prefs)
	}
	if len(got.Metros) == 0 || string(got.Metros) == "null" {
		t.Fatal("metros should be returned")
	}

	// POST saves; an unknown metro is dropped, remoteOnly sticks.
	post := postJSON(t, ts.URL+"/api/location-prefs", `{"metros":["seattle","bogus"],"remoteOnly":true}`)
	var saved struct {
		OK    bool          `json:"ok"`
		Prefs LocationPrefs `json:"prefs"`
	}
	_ = json.NewDecoder(post.Body).Decode(&saved)
	post.Body.Close()
	if !saved.OK || len(saved.Prefs.Metros) != 1 || saved.Prefs.Metros[0] != "seattle" || !saved.Prefs.RemoteOnly {
		t.Fatalf("saved = %+v", saved)
	}

	// The file persisted, and a fresh GET reflects it.
	if _, err := os.Stat(filepath.Join(dataDir, "location.json")); err != nil {
		t.Fatalf("location.json not written: %v", err)
	}
	resp2 := get(t, ts.URL+"/api/location-prefs", nil)
	var got2 struct {
		Prefs LocationPrefs `json:"prefs"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&got2)
	resp2.Body.Close()
	if len(got2.Prefs.Metros) != 1 || got2.Prefs.Metros[0] != "seattle" || !got2.Prefs.RemoteOnly {
		t.Fatalf("reloaded prefs = %+v", got2.Prefs)
	}
}
