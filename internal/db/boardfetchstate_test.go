package db

import (
	"path/filepath"
	"testing"

	"job-search-automation/internal/discovery"
)

func TestBoardFetchStateRoundTrip(t *testing.T) {
	repo, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	if _, err := repo.UpsertRegistryBoards([]discovery.VerifiedBoard{
		{Platform: "greenhouse", Slug: "acme"},
	}, "test"); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	key := BoardKey{Platform: "greenhouse", RegistryKey: discovery.SlugKey("acme")}

	// No state recorded yet: absent from the load.
	states, err := repo.LoadBoardFetchStates([]BoardKey{key})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := states[key]; ok {
		t.Fatal("state present before any fetch was recorded")
	}

	if err := repo.SaveBoardFetchResult(key, "ok", `W/"v1"`, "hash1"); err != nil {
		t.Fatalf("save: %v", err)
	}
	states, err = repo.LoadBoardFetchStates([]BoardKey{key})
	if err != nil {
		t.Fatalf("load after save: %v", err)
	}
	st, ok := states[key]
	if !ok || st.Etag != `W/"v1"` || st.ContentHash != "hash1" {
		t.Fatalf("state = %+v (ok=%v), want etag+hash back", st, ok)
	}

	// A failed fetch clears the state so the next cycle fully re-fetches.
	if err := repo.SaveBoardFetchResult(key, "failed", "", ""); err != nil {
		t.Fatalf("save failed status: %v", err)
	}
	states, err = repo.LoadBoardFetchStates([]BoardKey{key})
	if err != nil {
		t.Fatalf("load after failure: %v", err)
	}
	if _, ok := states[key]; ok {
		t.Fatal("state survived a failed fetch; next cycle would wrongly skip")
	}

	// A board with no registry row: save is a silent no-op, load stays empty.
	ghost := BoardKey{Platform: "lever", RegistryKey: "nowhere"}
	if err := repo.SaveBoardFetchResult(ghost, "ok", "e", "h"); err != nil {
		t.Fatalf("save ghost: %v", err)
	}
	states, err = repo.LoadBoardFetchStates([]BoardKey{ghost})
	if err != nil {
		t.Fatalf("load ghost: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("ghost board grew state: %+v", states)
	}
}
