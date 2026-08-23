package db

import (
	"path/filepath"
	"testing"

	"job-search-automation/internal/model"
)

func distLead(id, title, company, platform string) model.Lead {
	return model.Lead{
		JobLead: model.JobLead{
			Title:            title,
			Company:          company,
			Description:      "desc for " + id,
			DirectApplyURL:   "https://example.com/" + id,
			ATSPlatformName:  platform,
			ScrapedTimestamp: "2026-07-01T00:00:00.000Z",
			Location:         "Remote",
		},
		ID: id,
	}
}

func TestGlobalLeadsForDistribution(t *testing.T) {
	repo, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	for _, l := range []model.Lead{
		distLead("g1", "DevOps Engineer", "Acme", "Greenhouse"),
		distLead("g2", "Accountant", "Acme", "Greenhouse"),
		distLead("g3", "DevOps Lead", "Other", "Lever"),
		distLead("g4", "Barista", "Cafe", "Ashby"),
	} {
		if err := repo.HarvestGlobalLead(l); err != nil {
			t.Fatalf("harvest %s: %v", l.ID, err)
		}
	}

	// Watchlist company OR title-term prefilter: g1/g2 by company, g3 by term;
	// g4 matches neither and must not be scanned.
	got, err := repo.GlobalLeadsForDistribution("", []string{"acme"}, []string{"devops"}, 100)
	if err != nil {
		t.Fatalf("GlobalLeadsForDistribution: %v", err)
	}
	ids := map[string]bool{}
	for _, d := range got {
		ids[d.Lead.ID] = true
		if d.LastSeenAt == "" {
			t.Errorf("lead %s missing LastSeenAt cursor value", d.Lead.ID)
		}
	}
	if !ids["g1"] || !ids["g2"] || !ids["g3"] || ids["g4"] {
		t.Fatalf("got ids %v, want g1,g2,g3 and not g4", ids)
	}

	// A cursor past every row returns nothing.
	got, err = repo.GlobalLeadsForDistribution("9999-01-01 00:00:00", []string{"acme"}, []string{"devops"}, 100)
	if err != nil {
		t.Fatalf("cursor query: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("cursor past all rows returned %d leads", len(got))
	}
}

func TestGlobalLeadsBackfill(t *testing.T) {
	repo, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	for _, l := range []model.Lead{
		distLead("below", "Platform Engineer", "Acme", "Gem"),  // inside window
		distLead("newer", "Platform Engineer", "Acme", "Gem"),  // inside window, newest
		distLead("tooOld", "Platform Engineer", "Acme", "Gem"), // before window
		distLead("above", "Platform Engineer", "Acme", "Gem"),  // after upper bound
		distLead("nomatch", "Barista", "Acme", "Gem"),          // matches window but not term
	} {
		if err := repo.HarvestGlobalLead(l); err != nil {
			t.Fatalf("harvest %s: %v", l.ID, err)
		}
	}
	set := func(id, ts string) {
		if _, err := repo.exec("UPDATE global_jobs SET last_seen_at = ? WHERE id = ?", ts, id); err != nil {
			t.Fatalf("set last_seen %s: %v", id, err)
		}
	}
	set("tooOld", "2026-07-08 06:00:00")
	set("below", "2026-07-08 13:30:00")
	set("newer", "2026-07-08 13:36:00")
	set("nomatch", "2026-07-08 13:33:00")
	set("above", "2026-07-08 13:50:00")

	// Window (13:00, 13:47]: recovers the two term-matching rows stranded below a
	// 13:47 cursor, newest first; excludes the pre-window row, the post-cursor
	// row, and the non-matching title.
	got, err := repo.GlobalLeadsBackfill("2026-07-08 13:00:00", "2026-07-08 13:47:53", nil, []string{"platform engineer"}, 100)
	if err != nil {
		t.Fatalf("GlobalLeadsBackfill: %v", err)
	}
	var order []string
	for _, d := range got {
		order = append(order, d.Lead.ID)
	}
	want := []string{"newer", "below"} // DESC by last_seen_at
	if len(order) != len(want) || order[0] != want[0] || order[1] != want[1] {
		t.Fatalf("backfill window = %v, want %v (newest first, bounded, term-filtered)", order, want)
	}

	// Empty upper bound is a no-op guard: the backfill must never run unbounded.
	if got, err := repo.GlobalLeadsBackfill("2026-07-08 13:00:00", "", nil, []string{"platform engineer"}, 100); err != nil || got != nil {
		t.Fatalf("empty until: got %v err %v, want nil,nil", got, err)
	}
}

func TestVanishedBoardJobs(t *testing.T) {
	repo, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	// Tenant holds two Acme greenhouse jobs.
	for _, l := range []model.Lead{
		distLead("keep", "DevOps Engineer", "Acme", "Greenhouse"),
		distLead("gone", "SRE", "Acme", "Greenhouse"),
	} {
		if _, err := repo.InsertScrapedLead(l); err != nil {
			t.Fatalf("insert %s: %v", l.ID, err)
		}
	}

	// Age both global rows behind the cutoff, then re-harvest only "keep"
	// (the board scrape saw it again; "gone" vanished).
	if _, err := repo.exec("UPDATE global_jobs SET last_seen_at = '2026-01-01 00:00:00'"); err != nil {
		t.Fatalf("age rows: %v", err)
	}
	cutoff := "2026-06-01 00:00:00"
	if err := repo.HarvestGlobalLead(distLead("keep", "DevOps Engineer", "Acme", "Greenhouse")); err != nil {
		t.Fatalf("re-harvest: %v", err)
	}

	vanished, err := repo.VanishedBoardJobs("greenhouse", []string{"acme"}, cutoff)
	if err != nil {
		t.Fatalf("VanishedBoardJobs: %v", err)
	}
	if len(vanished) != 1 || vanished[0].ID != "gone" || vanished[0].Company != "acme" {
		t.Fatalf("vanished = %+v, want [{gone acme}]", vanished)
	}

	// A closed row never reappears in the diff.
	if err := repo.SetPipelineStage("gone", "closed"); err != nil {
		t.Fatalf("close: %v", err)
	}
	vanished, err = repo.VanishedBoardJobs("greenhouse", []string{"acme"}, cutoff)
	if err != nil {
		t.Fatalf("VanishedBoardJobs after close: %v", err)
	}
	if len(vanished) != 0 {
		t.Fatalf("closed row re-reported: %+v", vanished)
	}

	// A board that wasn't fetched (different company) reports nothing.
	vanished, err = repo.VanishedBoardJobs("greenhouse", []string{"otherco"}, cutoff)
	if err != nil {
		t.Fatalf("VanishedBoardJobs other board: %v", err)
	}
	if len(vanished) != 0 {
		t.Fatalf("unfetched board reported vanished rows: %+v", vanished)
	}
}

// TestVanishedBoardJobsSurvivesIDDrift: when the scraper's id derivation
// changes, a still-live posting is re-harvested under a NEW global id while
// the tenant's row still points at the old one. The old row must not read as
// vanished, because a global row with the same URL was seen this cycle.
func TestVanishedBoardJobsSurvivesIDDrift(t *testing.T) {
	repo, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	old := distLead("drift-old", "DevOps Engineer", "Acme", "Greenhouse")
	if _, err := repo.InsertScrapedLead(old); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := repo.exec("UPDATE global_jobs SET last_seen_at = '2026-01-01 00:00:00'"); err != nil {
		t.Fatalf("age rows: %v", err)
	}
	cutoff := "2026-06-01 00:00:00"

	// The same posting comes back under a new id scheme but the same URL.
	drifted := distLead("drift-new", "DevOps Engineer", "Acme", "Greenhouse")
	drifted.DirectApplyURL = old.DirectApplyURL
	if err := repo.HarvestGlobalLead(drifted); err != nil {
		t.Fatalf("re-harvest under new id: %v", err)
	}

	vanished, err := repo.VanishedBoardJobs("greenhouse", []string{"acme"}, cutoff)
	if err != nil {
		t.Fatalf("VanishedBoardJobs: %v", err)
	}
	if len(vanished) != 0 {
		t.Fatalf("id-drifted live posting reported vanished: %+v", vanished)
	}
}

// TestVanishedBoardJobsSurvivesURLCaseDrift: board-path casing drifted on top
// of id drift (registry-harvested lowercase Workday entries replaced labeled
// ones, /en-US/Jobs/ vs /en-US/jobs/, 2026-07-05), so the still-live posting
// is re-harvested under a new id AND a case-variant URL. The old row must not
// read as vanished: the URL survival check compares case-folded.
func TestVanishedBoardJobsSurvivesURLCaseDrift(t *testing.T) {
	repo, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	old := distLead("case-old", "DevOps Engineer", "PayPal", "Workday")
	old.DirectApplyURL = "https://example.com/en-US/Jobs/job/case-old"
	if _, err := repo.InsertScrapedLead(old); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := repo.exec("UPDATE global_jobs SET last_seen_at = '2026-01-01 00:00:00'"); err != nil {
		t.Fatalf("age rows: %v", err)
	}
	cutoff := "2026-06-01 00:00:00"

	// The same posting comes back from the lowercase board entry: new id, new
	// company casing, same URL up to case.
	drifted := distLead("case-new", "DevOps Engineer", "paypal", "Workday")
	drifted.DirectApplyURL = "https://example.com/en-US/jobs/job/case-old"
	if err := repo.HarvestGlobalLead(drifted); err != nil {
		t.Fatalf("re-harvest under new casing: %v", err)
	}

	vanished, err := repo.VanishedBoardJobs("workday", []string{"paypal"}, cutoff)
	if err != nil {
		t.Fatalf("VanishedBoardJobs: %v", err)
	}
	if len(vanished) != 0 {
		t.Fatalf("case-drifted live posting reported vanished: %+v", vanished)
	}
}

// TestVanishedBoardJobsSkipsProtectedRows: rows the diff must never touch stay
// out of the vanished set even when their posting is gone from the board. Only
// unapplied rows are eligible for auto-close: once a job is applied to or being
// interviewed for, a posting coming down (a repost under a new ATS id, or an
// Ashby/Workday board flap) must not close the row out from under an in-flight
// application. Applied-pipeline rows are the user's to manage by hand.
func TestVanishedBoardJobsSkipsProtectedRows(t *testing.T) {
	repo, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	for _, l := range []model.Lead{
		distLead("eligible", "DevOps Engineer", "Acme", "Greenhouse"),
		distLead("in-flight", "Cloud Engineer", "Acme", "Greenhouse"),
		distLead("interviewing", "Site Reliability Engineer", "Acme", "Greenhouse"),
		distLead("user-archived", "SRE", "Acme", "Greenhouse"),
		distLead("user-rejected", "Platform Engineer", "Acme", "Greenhouse"),
		distLead("has-offer", "Infra Engineer", "Acme", "Greenhouse"),
	} {
		if _, err := repo.InsertScrapedLead(l); err != nil {
			t.Fatalf("insert %s: %v", l.ID, err)
		}
	}
	if err := repo.ArchiveJob("user-archived"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	for id, stage := range map[string]string{
		"in-flight": "applied", "interviewing": "phone_screen",
		"user-rejected": "rejected", "has-offer": "offer",
	} {
		if err := repo.SetPipelineStage(id, stage); err != nil {
			t.Fatalf("stage %s: %v", id, err)
		}
	}

	// Every posting vanished from the board.
	if _, err := repo.exec("UPDATE global_jobs SET last_seen_at = '2026-01-01 00:00:00'"); err != nil {
		t.Fatalf("age rows: %v", err)
	}

	vanished, err := repo.VanishedBoardJobs("greenhouse", []string{"acme"}, "2026-06-01 00:00:00")
	if err != nil {
		t.Fatalf("VanishedBoardJobs: %v", err)
	}
	got := map[string]bool{}
	for _, v := range vanished {
		got[v.ID] = true
	}
	if !got["eligible"] {
		t.Errorf("vanished = %v, want unapplied 'eligible' row reported", got)
	}
	for _, id := range []string{"in-flight", "interviewing", "user-archived", "user-rejected", "has-offer"} {
		if got[id] {
			t.Errorf("protected row %s reported vanished", id)
		}
	}
}

// TestResurfacedClosedJobs: a diff-closed row whose posting is seen on its
// board again must be reported for reopening; rows still missing, and rows
// the user moved (rejected), must not.
func TestResurfacedClosedJobs(t *testing.T) {
	repo, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	for _, l := range []model.Lead{
		distLead("flap", "SRE", "Acme", "Greenhouse"),
		distLead("stay-gone", "DevOps Engineer", "Acme", "Greenhouse"),
		distLead("user-rejected", "Platform Engineer", "Acme", "Greenhouse"),
	} {
		if _, err := repo.InsertScrapedLead(l); err != nil {
			t.Fatalf("insert %s: %v", l.ID, err)
		}
	}
	for id, stage := range map[string]string{
		"flap": "closed", "stay-gone": "closed", "user-rejected": "rejected",
	} {
		if err := repo.SetPipelineStage(id, stage); err != nil {
			t.Fatalf("stage %s: %v", id, err)
		}
	}
	if _, err := repo.exec("UPDATE global_jobs SET last_seen_at = '2026-01-01 00:00:00'"); err != nil {
		t.Fatalf("age rows: %v", err)
	}

	// This cycle's fetch saw flap and user-rejected again; stay-gone is still
	// missing.
	for _, id := range []string{"flap", "user-rejected"} {
		if err := repo.HarvestGlobalLead(distLead(id, "re-seen", "Acme", "Greenhouse")); err != nil {
			t.Fatalf("re-harvest %s: %v", id, err)
		}
	}

	ids, err := repo.ResurfacedClosedJobs("2026-06-01 00:00:00")
	if err != nil {
		t.Fatalf("ResurfacedClosedJobs: %v", err)
	}
	if len(ids) != 1 || ids[0] != "flap" {
		t.Fatalf("resurfaced = %v, want [flap]", ids)
	}
}
