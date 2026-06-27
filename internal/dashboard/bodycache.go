package dashboard

import "sync"

// bodyCache memoizes rendered report bodies (analytics, activity-log) behind a
// cheap per-tenant signature, the same pull-based scheme as jobSetCache: a stored
// entry is served only while its signature matches, so any write that moves the
// jobs-table fingerprint (or the day bucket folded into the signature) supersedes
// it on the next request with no cross-process coordination.
//
// Entries are keyed by "tenant|view" so tenants never collide: no thrash when
// two tenants alternate requests, and no chance of one tenant's body being served
// to another if their signatures ever coincide. Held by pointer on Server so
// forRequest clones share one cache. Growth is bounded by (active tenants x
// cached views), which is small; no eviction is needed at this scale.
type bodyCache struct {
	mu      sync.Mutex
	entries map[string]bodyCacheEntry
}

type bodyCacheEntry struct {
	sig  string
	html string
}

func newBodyCache() *bodyCache {
	return &bodyCache{entries: make(map[string]bodyCacheEntry)}
}

// get returns the HTML stored under key when the stored signature equals sig;
// otherwise it runs build, stores the result under (key, sig), and returns it.
// build runs outside the lock so a slow render never serializes other tenants; a
// concurrent miss on the same key may build twice, which is wasted work, not
// incorrect, and rare at this scale.
func (c *bodyCache) get(key, sig string, build func() (string, error)) (string, bool, error) {
	c.mu.Lock()
	ent, ok := c.entries[key]
	c.mu.Unlock()
	if ok && ent.sig == sig {
		return ent.html, true, nil
	}
	html, err := build()
	if err != nil {
		return "", false, err
	}
	c.mu.Lock()
	c.entries[key] = bodyCacheEntry{sig: sig, html: html}
	c.mu.Unlock()
	return html, false, nil
}
