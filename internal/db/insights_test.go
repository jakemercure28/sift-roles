package db

import (
	"strings"
	"testing"
	"time"
)

// Fixed reference: 2026-06-10 is a Wednesday, so its Monday week-start is
// 2026-06-08; 2026-06-01 is the prior Monday.
func TestComputeNewRolesByWeek(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	rows := []weekRow{
		{postedAt: "2026-06-01"},              // week of Jun 1 (posted date)
		{postedAt: "2026-06-03"},              // week of Jun 1
		{postedAt: "2026-06-08"},              // week of Jun 8
		{firstSeen: "2026-06-02 10:00:00"},    // week of Jun 1 (first_seen fallback)
		{created: "2026-06-09 09:00:00"},      // week of Jun 8 (created fallback)
		{postedAt: "not a date", created: ""}, // unparseable -> dropped
	}

	all := computeNewRolesByWeek(rows, "all", now)
	want := []WeekCount{
		{WeekStart: "2026-06-01", Label: "Jun 1", Count: 3},
		{WeekStart: "2026-06-08", Label: "Jun 8", Count: 2},
	}
	if len(all) != len(want) {
		t.Fatalf("all: got %d weeks, want %d: %+v", len(all), len(want), all)
	}
	for i, w := range want {
		if all[i] != w {
			t.Errorf("all[%d] = %+v, want %+v", i, all[i], w)
		}
	}

	// 12w spans 12 weeks ending this week (Jun 8), zero-filling empty weeks.
	twelve := computeNewRolesByWeek(rows, "12w", now)
	if len(twelve) != 12 {
		t.Fatalf("12w: got %d weeks, want 12", len(twelve))
	}
	if twelve[0].WeekStart != "2026-03-23" {
		t.Errorf("12w first week = %q, want 2026-03-23", twelve[0].WeekStart)
	}
	if got := twelve[10]; got.WeekStart != "2026-06-01" || got.Count != 3 {
		t.Errorf("12w[10] = %+v, want Jun 1 count 3", got)
	}
	if got := twelve[11]; got.WeekStart != "2026-06-08" || got.Count != 2 {
		t.Errorf("12w[11] = %+v, want Jun 8 count 2", got)
	}
}

func TestComputeNewRolesByWeekEmpty(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	got := computeNewRolesByWeek(nil, "26w", now)
	if got == nil {
		t.Fatal("want non-nil empty slice (encodes as [], not null)")
	}
	if len(got) != 0 {
		t.Fatalf("want empty, got %+v", got)
	}
}

// NewRolesByWeek must only count jobs with a description longer than 100 chars
// (the SQL filter), matching the Node query.
func TestNewRolesByWeekDescriptionFilter(t *testing.T) {
	repo := newTestRepo(t)
	long := strings.Repeat("x", 150) // > 100 chars
	insert := func(id, desc, posted string) {
		t.Helper()
		if _, err := repo.db.Exec(
			`INSERT INTO jobs (id, title, company, description, posted_at, status)
			 VALUES (?, 'Eng', 'Co', ?, ?, 'pending')`,
			id, desc, posted,
		); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	insert("a", long, "2026-06-01")
	insert("b", long, "2026-06-03")
	insert("c", "too short", "2026-06-02") // excluded by length filter

	rows, err := repo.NewRolesByWeek("all")
	if err != nil {
		t.Fatalf("NewRolesByWeek: %v", err)
	}
	total := 0
	for _, w := range rows {
		total += w.Count
	}
	if total != 2 {
		t.Fatalf("counted %d roles, want 2 (short-description job excluded)", total)
	}
}
