package db

import (
	"testing"
	"time"
)

func TestNormalizePostedAt(t *testing.T) {
	// Fixed clock so relative phrases resolve deterministically.
	now := time.Date(2026, 6, 12, 15, 30, 0, 0, time.UTC)

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"iso date passes through", "2026-06-10", "2026-06-10"},
		{"iso timestamp passes through", "2026-06-10 12:00:00", "2026-06-10 12:00:00"},
		{"rfc3339 passes through", "2026-06-10T12:00:00Z", "2026-06-10T12:00:00Z"},
		{"posted N days ago", "Posted 6 Days Ago", "2026-06-06"},
		{"plain days ago", "2 days ago", "2026-06-10"},
		{"one day ago singular", "1 day ago", "2026-06-11"},
		{"plus suffix", "30+ days ago", "2026-05-13"},
		{"hours ago is today", "5 hours ago", "2026-06-12"},
		{"weeks ago", "2 weeks ago", "2026-05-29"},
		{"months ago", "3 months ago", "2026-03-12"},
		{"yesterday", "Yesterday", "2026-06-11"},
		{"today", "Posted today", "2026-06-12"},
		{"just posted", "Just posted", "2026-06-12"},
		{"unrecognized junk drops", "Featured", ""},
		{"unrecognized empty-ish drops", "n/a", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := normalizePostedAt(c.in, now); got != c.want {
				t.Errorf("normalizePostedAt(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
