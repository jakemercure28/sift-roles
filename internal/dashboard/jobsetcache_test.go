package dashboard

import (
	"errors"
	"testing"
	"time"

	"job-search-automation/internal/db"
)

// buildCounter returns a build func that records how many times it ran and yields
// a fresh one-row slice carrying id so callers can tell rebuilds apart.
func buildCounter(id string, calls *int) func() ([]db.ListedJob, error) {
	return func() ([]db.ListedJob, error) {
		*calls++
		return []db.ListedJob{{ID: id}}, nil
	}
}

func TestJobSetCacheHitSkipsBuild(t *testing.T) {
	c := newMemJobSetCache(8, time.Minute)
	calls := 0

	rows, hit, err := c.Get("k", "sig1", buildCounter("a", &calls))
	if err != nil || len(rows) != 1 || rows[0].ID != "a" {
		t.Fatalf("first Get: rows=%v err=%v", rows, err)
	}
	if hit {
		t.Fatal("first Get should be a cache miss")
	}
	if calls != 1 {
		t.Fatalf("first Get should build once, got %d", calls)
	}

	rows, hit, err = c.Get("k", "sig1", buildCounter("b", &calls))
	if err != nil {
		t.Fatalf("second Get err: %v", err)
	}
	if !hit {
		t.Fatal("matching sig should report a cache hit")
	}
	if calls != 1 {
		t.Fatalf("matching sig should serve cache without rebuilding, builds=%d", calls)
	}
	if rows[0].ID != "a" {
		t.Fatalf("expected cached row a, got %q", rows[0].ID)
	}
}

func TestJobSetCacheSigChangeRebuilds(t *testing.T) {
	c := newMemJobSetCache(8, time.Minute)
	calls := 0

	if _, _, err := c.Get("k", "sig1", buildCounter("a", &calls)); err != nil {
		t.Fatal(err)
	}
	rows, hit, err := c.Get("k", "sig2", buildCounter("b", &calls))
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Fatal("changed sig should report a cache miss")
	}
	if calls != 2 {
		t.Fatalf("changed sig should rebuild, builds=%d", calls)
	}
	if rows[0].ID != "b" {
		t.Fatalf("expected rebuilt row b, got %q", rows[0].ID)
	}
}

func TestJobSetCacheReturnsCallerOwnedCopy(t *testing.T) {
	c := newMemJobSetCache(8, time.Minute)
	calls := 0

	rows, _, _ := c.Get("k", "sig1", buildCounter("a", &calls))
	rows[0].ID = "mutated" // a caller is free to sort/mutate its slice

	again, _, _ := c.Get("k", "sig1", buildCounter("a", &calls))
	if again[0].ID != "a" {
		t.Fatalf("mutating a returned slice must not corrupt the cache, got %q", again[0].ID)
	}
}

func TestJobSetCacheBuildErrorNotCached(t *testing.T) {
	c := newMemJobSetCache(8, time.Minute)
	boom := errors.New("boom")

	if _, _, err := c.Get("k", "sig1", func() ([]db.ListedJob, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("expected build error to propagate, got %v", err)
	}
	calls := 0
	if _, _, err := c.Get("k", "sig1", buildCounter("a", &calls)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("a failed build must not be cached; expected rebuild, builds=%d", calls)
	}
}

func TestJobSetCacheLRUEviction(t *testing.T) {
	c := newMemJobSetCache(2, time.Minute)
	calls := 0

	mustGet := func(k string) {
		if _, _, err := c.Get(k, "sig", buildCounter(k, &calls)); err != nil {
			t.Fatal(err)
		}
	}

	mustGet("a")
	mustGet("b")
	mustGet("a") // touch a so it is more-recently-used than b
	mustGet("c") // over capacity -> evicts the LRU entry, b

	before := calls
	mustGet("a") // still cached
	if calls != before {
		t.Fatalf("a should have survived eviction, rebuilds=%d", calls-before)
	}

	before = calls
	mustGet("b") // evicted -> rebuilds
	if calls != before+1 {
		t.Fatalf("b should have been evicted and rebuilt, delta=%d", calls-before)
	}
}

func TestJobSetCacheIdleExpiry(t *testing.T) {
	c := newMemJobSetCache(8, time.Minute)
	clock := time.Unix(0, 0)
	c.now = func() time.Time { return clock }
	calls := 0

	if _, _, err := c.Get("k", "sig", buildCounter("a", &calls)); err != nil {
		t.Fatal(err)
	}

	clock = clock.Add(2 * time.Minute) // past idleTTL
	if _, _, err := c.Get("k", "sig", buildCounter("a", &calls)); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("an entry idle past its TTL should rebuild, builds=%d", calls)
	}
}
