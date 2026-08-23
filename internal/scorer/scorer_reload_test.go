package scorer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScorerReloadPicksUpNewResume(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "resume.md"), []byte(content), 0o644); err != nil {
			t.Fatalf("write resume: %v", err)
		}
	}
	write("resume v1")

	s := New(nil, dir)
	h1, err := s.ResumeHash()
	if err != nil {
		t.Fatalf("hash v1: %v", err)
	}

	// Without Reload the cache keeps serving v1.
	write("resume v2")
	if h, _ := s.ResumeHash(); h != h1 {
		t.Fatal("hash changed without Reload; cache should still serve v1")
	}

	s.Reload()
	h2, err := s.ResumeHash()
	if err != nil {
		t.Fatalf("hash v2: %v", err)
	}
	if h2 == h1 {
		t.Fatal("hash unchanged after Reload; new resume not picked up")
	}
	if want := HashResume("resume v2"); h2 != want {
		t.Fatalf("hash after Reload = %q, want hash of v2", h2)
	}
}
