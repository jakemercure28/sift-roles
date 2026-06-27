package dashboard

import (
	"container/list"
	"strconv"
	"sync"
	"time"

	"job-search-automation/internal/db"
)

// jobSetCache memoizes per-tenant, per-view filtered job sets behind a cheap
// jobs-table signature. The dashboard Server is shared across all in-flight
// requests (forRequest only shallow-clones it), so implementations MUST be safe
// for concurrent use and MUST return a caller-owned slice the caller may freely
// re-sort or mutate.
//
// Get returns the rows stored under key when the stored signature equals sig;
// otherwise it runs build, stores the result under (key, sig), and returns it.
// The bool reports whether the rows came from cache.
// Because sig is derived from COUNT(*)+MAX(updated_at) for the tenant, any write
// supersedes every stale entry for that tenant on its next request: invalidation
// is pull-based and needs no cross-process coordination, so this stays correct
// even across multiple replicas (each one self-heals on the next sig it sees).
//
// The interface is the seam. A Redis-backed implementation (key -> {sig, encoded
// rows}, shared across replicas) can replace memJobSetCache for large
// deployments where duplicating every working set in every replica's heap costs
// too much, without touching a single call site.
type jobSetCache interface {
	Get(key, sig string, build func() ([]db.ListedJob, error)) ([]db.ListedJob, bool, error)
}

// Default bounds for the in-process cache, sized for one container serving tens
// of tenants. maxEntries caps heap growth as signups climb; idleTTL releases a
// tenant's rows after they stop browsing. Each entry holds one lightweight
// (tenant, filter, sort) row set; free-text fields are hydrated per visible page.
const (
	defaultJobCacheMaxEntries = 128
	defaultJobCacheIdleTTL    = 15 * time.Minute
)

// memJobSetCache is the default in-process jobSetCache: a bounded LRU keyed by
// "tenant|filter|sort", with per-entry signature gating and idle expiry.
type memJobSetCache struct {
	mu         sync.Mutex
	ll         *list.List // front = most recently used; back = eviction candidate
	index      map[string]*list.Element
	maxEntries int
	idleTTL    time.Duration
	now        func() time.Time // injectable so tests can drive expiry deterministically
}

type jobSetEntry struct {
	key      string
	sig      string
	rows     []db.ListedJob
	lastUsed time.Time
}

func newMemJobSetCache(maxEntries int, idleTTL time.Duration) *memJobSetCache {
	if maxEntries < 1 {
		maxEntries = 1
	}
	return &memJobSetCache{
		ll:         list.New(),
		index:      make(map[string]*list.Element),
		maxEntries: maxEntries,
		idleTTL:    idleTTL,
		now:        time.Now,
	}
}

func (c *memJobSetCache) Get(key, sig string, build func() ([]db.ListedJob, error)) ([]db.ListedJob, bool, error) {
	if rows, ok := c.lookup(key, sig); ok {
		return rows, true, nil
	}
	// build runs outside the lock so a slow DB fetch never serializes every other
	// request. A concurrent miss on the same key can rebuild twice; that is wasted
	// work, not incorrect, and is rare at this scale.
	rows, err := build()
	if err != nil {
		return nil, false, err
	}
	c.store(key, sig, rows)
	return cloneRows(rows), false, nil
}

func (c *memJobSetCache) lookup(key, sig string) ([]db.ListedJob, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[key]
	if !ok {
		return nil, false
	}
	ent := el.Value.(*jobSetEntry)
	now := c.now()
	if ent.sig != sig || now.Sub(ent.lastUsed) > c.idleTTL {
		// A write moved the signature, or the entry sat idle past its TTL: drop it
		// and force a rebuild rather than serve stale rows.
		c.removeElement(el)
		return nil, false
	}
	ent.lastUsed = now
	c.ll.MoveToFront(el)
	return cloneRows(ent.rows), true
}

func (c *memJobSetCache) store(key, sig string, rows []db.ListedJob) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if el, ok := c.index[key]; ok {
		ent := el.Value.(*jobSetEntry)
		ent.sig, ent.rows, ent.lastUsed = sig, rows, now
		c.ll.MoveToFront(el)
	} else {
		el := c.ll.PushFront(&jobSetEntry{key: key, sig: sig, rows: rows, lastUsed: now})
		c.index[key] = el
	}
	c.evict(now)
}

// evict drops the least-recently-used entry while the cache is over its size
// bound, and any idle-expired entry at the back (the back is the oldest lastUsed,
// so if it is not expired none are). Called under c.mu.
func (c *memJobSetCache) evict(now time.Time) {
	for c.ll.Len() > 0 {
		back := c.ll.Back()
		ent := back.Value.(*jobSetEntry)
		if c.ll.Len() > c.maxEntries || now.Sub(ent.lastUsed) > c.idleTTL {
			c.removeElement(back)
			continue
		}
		break
	}
}

func (c *memJobSetCache) removeElement(el *list.Element) {
	c.ll.Remove(el)
	delete(c.index, el.Value.(*jobSetEntry).key)
}

// cloneRows returns a shallow copy so callers (and concurrent requests) never
// share, and so cannot mutate, the slice the cache holds. ListedJob fields are
// value strings / shared-but-never-mutated *int, so a shallow copy is sufficient
// and cheap relative to the avoided Postgres round trip.
func cloneRows(rows []db.ListedJob) []db.ListedJob {
	if rows == nil {
		return nil
	}
	return append([]db.ListedJob(nil), rows...)
}

// filteredJobsCached returns the lightweight (no free-text) SQL-filtered, ordered
// job set for a list view, served from s.jobs when the tenant's jobs-table
// signature is unchanged. The per-request location/pagination work then runs in Go
// on the returned caller-owned slice, and the caller hydrates the free-text fields
// for the visible page only. So a tenant browsing filters/sorts/pages between
// scrapes hits Postgres for one cheap signature query instead of dragging the full
// row set across the wire, and even a cache miss no longer pulls every row's
// description. The search path uses fetchFilteredJobsLightSearch (the substring
// match runs in SQL and still returns lightweight rows), so it too avoids pulling
// the free-text fields; it is not cached here.
//
// s.jobs is nil for bare test Servers; that path bypasses the cache entirely.
func (s *Server) filteredJobsCached(filter, sortKey string) ([]db.ListedJob, bool, error) {
	if s.jobs == nil {
		rows, err := fetchFilteredJobsLight(s.repo, filter, sortKey)
		return rows, false, err
	}
	// MarketDataSignature is the generic jobs-table fingerprint (COUNT(*) +
	// MAX(updated_at)); reused here so the cache invalidates the instant a scrape,
	// score, or pipeline transition touches this tenant's rows.
	count, maxUpdated, err := s.repo.MarketDataSignature()
	if err != nil {
		return nil, false, err
	}
	sig := strconv.Itoa(count) + "|" + maxUpdated
	key := s.repo.UserID() + "|" + filter + "|" + sortKey
	return s.jobs.Get(key, sig, func() ([]db.ListedJob, error) {
		return fetchFilteredJobsLight(s.repo, filter, sortKey)
	})
}
