package logprune

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("log\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestPruneDeletesOnlyExpiredDatedLogs(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "refresh", "20260101.log")
	fresh := filepath.Join(root, "refresh", "20260601.log")
	persistent := filepath.Join(root, "refresh", "refresh.log")
	other := filepath.Join(root, "refresh", "notes.txt")
	writeFile(t, old)
	writeFile(t, fresh)
	writeFile(t, persistent)
	writeFile(t, other)

	res, err := Prune(Options{
		Root:          root,
		RetentionDays: 30,
		Now:           time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res.Scanned != 2 || res.Deleted != 1 {
		t.Fatalf("result = %+v, want scanned 2 deleted 1", res)
	}
	if exists(old) {
		t.Fatalf("old dated log still exists")
	}
	for _, path := range []string{fresh, persistent, other} {
		if !exists(path) {
			t.Fatalf("%s should remain", path)
		}
	}
}

func TestPruneDryRunDoesNotDelete(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "refresh", "20260101.log")
	writeFile(t, old)

	res, err := Prune(Options{
		Root:          root,
		RetentionDays: 30,
		DryRun:        true,
		Now:           time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res.Scanned != 1 || res.Deleted != 0 || !res.DryRun {
		t.Fatalf("result = %+v", res)
	}
	if !exists(old) {
		t.Fatal("dry run deleted old log")
	}
}

func TestPruneMissingRootIsNoop(t *testing.T) {
	res, err := Prune(Options{
		Root:          filepath.Join(t.TempDir(), "missing"),
		RetentionDays: 30,
		Now:           time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if res.Scanned != 0 || res.Deleted != 0 {
		t.Fatalf("result = %+v, want empty", res)
	}
}

func TestPruneRejectsInvalidRetention(t *testing.T) {
	if _, err := Prune(Options{Root: t.TempDir(), RetentionDays: 0}); err == nil {
		t.Fatal("expected invalid retention error")
	}
}
