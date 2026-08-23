package scheduler

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"job-search-automation/internal/closedcheck"
	"job-search-automation/internal/db"
	"job-search-automation/internal/discovery"
	"job-search-automation/internal/jobfilter"
	"job-search-automation/internal/metrics"
	"job-search-automation/internal/scraper"
	"job-search-automation/internal/searchterms"
)

// Global-first scraping: instead of scraping every tenant's boards separately
// (N tenants watching the same company = N full board fetches per cycle), the
// cron computes the distinct-board union across tenants, scrapes each board
// exactly once in board mode, harvests everything into global_jobs, and then
// feeds each tenant's queue from that cache (watchlist company OR search-term
// match, with the worker-parity term matcher). Closed detection for board
// jobs becomes a diff: a posting present in the previous harvest but absent
// from this one is marked closed on every tenant row that holds it.

// GlobalScrapeConfig carries the global-pass knobs (from config.Config).
type GlobalScrapeConfig struct {
	// BoardBatch is how many boards each POST /scrape request carries, bounding
	// worker response size on big unions.
	BoardBatch int
	// DistributeLimit caps global rows scanned per tenant per cycle.
	DistributeLimit int
	// InitialMaxAgeDays bounds a tenant's FIRST distribution pass (no cursor
	// yet) so it doesn't replay the whole cache into a fresh queue.
	InitialMaxAgeDays int
	// DistributeGraceHours is how far below its cursor each distribution pass
	// re-sweeps to catch rows orphaned by the within-pass write-order race (a
	// board scraped early in a pass gets a lower last_seen_at than the row the
	// cursor jumped to, so the strict > cursor forward query never revisits it).
	// It must exceed one scrape pass's duration; the config default is a few
	// cycles' worth. 0 disables the backfill.
	DistributeGraceHours int
	// CloseGraceHours is how long a posting must stay missing from complete
	// fetches of its board before the diff closes it (the config default is a
	// day, 8 three-hour cycles): Workday boards flap live postings out of one
	// listing and back into a later one (nike/usc, 2026-07-05), so absence
	// from a single fetch is not proof of removal. Genuine closes still land,
	// one grace window late. 0 disables the window so a single missed cycle
	// closes, which is how live postings got buried.
	CloseGraceHours int
	// CloseVerify probes each Workday diff-close candidate against its board's
	// search endpoint before the close commits. Listing truncation can look
	// complete (a 429-starved fetch served 115 of intel's 640 postings with a
	// self-corroborating total, 2026-07-05), and a posting such fetches never
	// re-see goes stale past the grace window, where neither the grace nor the
	// reopen pass can save it. The probe is the posting-level ground truth: a
	// live hit vetoes the close and stamps the sighting; an inconclusive probe
	// skips the close (absence unproven); only a clean miss lets it commit.
	CloseVerify bool
	// WorkdayRegistryBatch caps how many registry Workday boards are folded into
	// the union per cycle. Workday has no etag/content-hash conditional fetch
	// (every board is a full fetch) and is 429-prone, so unlike GH/Ashby/Lever
	// its ~5k registry boards cannot all be pulled every cycle without risking
	// the cycle timeout and rate limits. Instead a persisted cursor rotates
	// through them a bounded slice at a time, so every board is refreshed within
	// a few cycles. 0 disables the Workday registry fold; a value >= the board
	// count folds them all (no rotation).
	WorkdayRegistryBatch int
	// DistributeReserve is held back from the cycle budget so the board scrape
	// (which shares the cycle context) cannot consume all of it and starve the
	// per-tenant distribution/scoring that runs afterward. RunOnce bounds the
	// board scrape to (deadline - DistributeReserve). 0 disables the reserve.
	DistributeReserve time.Duration
}

// EnableGlobalScrape switches the cron fan-out to global-first: one board-mode
// scrape of the cross-tenant union, then per-tenant distribution; the
// per-tenant scrape keeps only the aggregator platforms board mode can't cover.
func (s *Scheduler) EnableGlobalScrape(cfg GlobalScrapeConfig) {
	if cfg.BoardBatch < 1 {
		cfg.BoardBatch = 150
	}
	if cfg.DistributeLimit < 1 {
		cfg.DistributeLimit = 500
	}
	s.global = &cfg
}

// boardPlatformNames maps board-mode platform keys to the ATSPlatformName the
// harvest stores, for the closed diff's platform match.
var boardModePlatforms = map[string]bool{
	"greenhouse": true, "lever": true, "ashby": true, "workable": true, "workday": true,
	"gem": true, "smartrecruiters": true,
}

// aggregatorPlatforms are the platforms board mode cannot cover (search-term
// feeds and non-board scrapers); the per-tenant cron scrape keeps running these
// when global mode is on. Rippling stays here until board mode supports it.
var aggregatorPlatforms = []string{
	"wellfound", "remoteok", "jobicy", "arbeitnow", "wwr", "builtin", "rippling",
}

// cronPlatforms is the platform list for the cron fan-out's per-tenant scrape:
// explicit config wins, otherwise all platforms, or only the aggregators when
// the board platforms are covered by the global pass. The user-triggered
// first-run path (RunOnceForUser) always scrapes everything so onboarding
// freshness never depends on the global cron having run.
func (s *Scheduler) cronPlatforms() []string {
	if len(s.platforms) > 0 {
		return s.platforms
	}
	if s.global != nil {
		return aggregatorPlatforms
	}
	return nil
}

// globalCycle is what one global board pass learned, consumed by the
// per-tenant distribution/diff steps.
type globalCycle struct {
	// cutoff is the pass start (UTC, the global_jobs timestamp format): rows on
	// fetched boards seen before it have vanished from their board.
	cutoff string
	// fetched maps a lowercase platform name to the lowercase companies that
	// returned at least one lead this pass. Boards with zero leads are omitted
	// on purpose: a blocked/failed fetch must read as "unknown", never as
	// "everything on this board closed".
	fetched map[string][]string
	// incomplete maps a lowercase platform name to the lowercase companies
	// whose board fetch the worker reported as incomplete (blocked page,
	// pagination cap). Such a board can still contribute leads, but absence
	// from a truncated listing proves nothing, so it must not feed the diff.
	incomplete map[string]map[string]bool
	// harvested counts leads upserted into global_jobs.
	harvested int
	// verify, when non-nil, is the cycle's shared pre-close liveness prober;
	// tenants routinely hold the same postings, so verdicts are cached per
	// posting URL across the whole distribution fan-out.
	verify *diffCloseVerifier
}

// diffCloseVerifier double-checks a Workday diff-close candidate against its
// own board before the close commits, caching one verdict per posting URL per
// cycle. The distribution fan-out is sequential, so no locking is needed.
type diffCloseVerifier struct {
	post     closedcheck.PostFetcher
	verdicts map[string]string // lowercased posting URL -> "open"|"closed"
}

func newDiffCloseVerifier(post closedcheck.PostFetcher) *diffCloseVerifier {
	if post == nil {
		post = closedcheck.DefaultPostFetcher(nil)
	}
	return &diffCloseVerifier{post: post, verdicts: map[string]string{}}
}

// check returns "open", "closed", or "" (inconclusive). Only definite
// verdicts are cached: an inconclusive probe may be transient and a later
// tenant's candidate should retry it.
func (v *diffCloseVerifier) check(ctx context.Context, postingURL string) string {
	key := strings.ToLower(postingURL)
	if verdict, ok := v.verdicts[key]; ok {
		return verdict
	}
	verdict, err := closedcheck.CheckWorkdayPosting(ctx, v.post, postingURL)
	if err != nil {
		return ""
	}
	if verdict != "" {
		v.verdicts[key] = verdict
	}
	return verdict
}

// runGlobalBoards scrapes the distinct-board union across tenants in board
// mode and harvests every lead into global_jobs. Best-effort: a chunk failure
// is logged and skipped (those boards simply don't participate in this cycle's
// distribution or diff).
func (s *Scheduler) runGlobalBoards(ctx context.Context, tenants []string, log *slog.Logger) globalCycle {
	gc := globalCycle{
		cutoff:     time.Now().UTC().Format("2006-01-02 15:04:05"),
		fetched:    map[string][]string{},
		incomplete: map[string]map[string]bool{},
	}
	if s.global.CloseVerify {
		gc.verify = newDiffCloseVerifier(s.verifyPost)
	}
	boards := s.collectBoardUnion(ctx, tenants)
	if len(boards) == 0 {
		return gc
	}
	log.Info("global board scrape started", "boards", len(boards), "tenants", len(tenants))
	metrics.SetGlobalBoardUnionSize(len(boards))

	seen := map[string]map[string]bool{}    // platform -> companyLower -> true
	outcomes := map[string]map[string]int{} // platform -> fetch status -> boards
	unchanged := 0
	for start := 0; start < len(boards); start += s.global.BoardBatch {
		end := start + s.global.BoardBatch
		if end > len(boards) {
			end = len(boards)
		}
		chunk := boards[start:end]
		s.fillBoardFetchState(chunk, log)
		leads, boardResults, err := s.client.ScrapeBoards(ctx, chunk)
		if err != nil {
			log.Warn("global board chunk failed", "from", start, "to", end, "error", err)
			continue
		}
		unchanged += s.saveBoardFetchResults(boardResults, log)
		for _, res := range boardResults {
			platform := strings.ToLower(strings.TrimSpace(res.Platform))
			if platform != "" && res.Status != "" {
				if outcomes[platform] == nil {
					outcomes[platform] = map[string]int{}
				}
				outcomes[platform][res.Status]++
			}
		}
		for _, res := range boardResults {
			if res.Status != "failed" || res.Company == "" {
				continue
			}
			platform := strings.ToLower(strings.TrimSpace(res.Platform))
			company := strings.ToLower(strings.TrimSpace(res.Company))
			if gc.incomplete[platform] == nil {
				gc.incomplete[platform] = map[string]bool{}
			}
			gc.incomplete[platform][company] = true
			log.Warn("board fetch incomplete, excluded from closed diff",
				"platform", platform, "slug", res.Slug, "company", company)
		}
		for _, lead := range leads {
			if err := s.repo.HarvestGlobalLead(lead); err != nil {
				log.Warn("global harvest failed", "id", lead.ID, "error", err)
				continue
			}
			gc.harvested++
			platform := strings.ToLower(strings.TrimSpace(lead.ATSPlatformName))
			company := strings.ToLower(strings.TrimSpace(lead.Company))
			if platform == "" || company == "" {
				continue
			}
			if seen[platform] == nil {
				seen[platform] = map[string]bool{}
			}
			seen[platform][company] = true
		}
	}
	for platform, companies := range seen {
		for c := range companies {
			if gc.incomplete[platform][c] {
				continue
			}
			gc.fetched[platform] = append(gc.fetched[platform], c)
		}
	}
	metrics.ObserveGlobalScrape(len(boards), gc.harvested)
	metrics.AddBoardsSkippedUnchanged(unchanged)

	// Per-platform coverage: distinct companies that returned leads vs those the
	// worker reported incomplete. A platform whose company count collapses toward
	// zero while its registry holds thousands of boards is being starved or
	// blocked (the Ashby=11 regression), so this is the signal to alert on.
	companyCounts := make(map[string]int, len(seen))
	for platform, companies := range seen {
		companyCounts[platform] = len(companies)
	}
	incompleteCounts := make(map[string]int, len(gc.incomplete))
	for platform, companies := range gc.incomplete {
		incompleteCounts[platform] = len(companies)
	}
	metrics.SetGlobalBoardCoverage(companyCounts, incompleteCounts)
	metrics.SetGlobalBoardPassOutcomes(outcomes)

	// The board scrape shares the cycle context but is bounded to leave a
	// distribution reserve. If that sub-budget expired before every board was
	// fetched, coverage this cycle is partial: flag it so a persistently
	// truncated pass (union too big for the budget) is visible.
	if ctx.Err() != nil {
		metrics.IncGlobalBoardScrapeTruncated()
		log.Warn("global board scrape truncated by budget", "boards", len(boards), "error", ctx.Err())
	}

	log.Info("global board scrape complete",
		"boards", len(boards), "harvested", gc.harvested, "unchanged", unchanged)
	return gc
}

// fillBoardFetchState stamps the previous cycle's etag/content-hash onto the
// chunk's single-request boards so the worker can skip unchanged ones.
// Best-effort: on error the chunk is simply fully fetched.
func (s *Scheduler) fillBoardFetchState(chunk []scraper.BoardRef, log *slog.Logger) {
	var keys []db.BoardKey
	for _, b := range chunk {
		if incrementalBoardPlatforms[b.Platform] {
			keys = append(keys, db.BoardKey{Platform: b.Platform, RegistryKey: discovery.SlugKey(b.Slug)})
		}
	}
	states, err := s.repo.LoadBoardFetchStates(keys)
	if err != nil {
		log.Warn("board fetch state load failed", "error", err)
		return
	}
	for i := range chunk {
		if !incrementalBoardPlatforms[chunk[i].Platform] {
			continue
		}
		if st, ok := states[db.BoardKey{Platform: chunk[i].Platform, RegistryKey: discovery.SlugKey(chunk[i].Slug)}]; ok {
			chunk[i].Etag = st.Etag
			chunk[i].ContentHash = st.ContentHash
		}
	}
}

// saveBoardFetchResults persists per-board outcomes and returns how many
// boards were skipped as unchanged. Only the single-request incremental
// platforms persist state: a Workday tenant can host several boards under one
// slug, so its per-board outcomes (used only for the closed diff) would
// clobber each other in the registry.
func (s *Scheduler) saveBoardFetchResults(results []scraper.BoardFetchResult, log *slog.Logger) int {
	unchanged := 0
	for _, res := range results {
		if res.Status == "unchanged" {
			unchanged++
		}
		if !incrementalBoardPlatforms[res.Platform] {
			continue
		}
		key := db.BoardKey{Platform: res.Platform, RegistryKey: discovery.SlugKey(res.Slug)}
		if err := s.repo.SaveBoardFetchResult(key, res.Status, res.Etag, res.ContentHash); err != nil {
			log.Warn("board fetch state save failed", "platform", res.Platform, "slug", res.Slug, "error", err)
		}
	}
	return unchanged
}

// incrementalBoardPlatforms are the single-request board platforms whose
// fetches support etag/content-hash skipping (mirrors the worker).
var incrementalBoardPlatforms = map[string]bool{
	"greenhouse": true, "lever": true, "ashby": true,
}

// collectBoardUnion merges every tenant's configured + suggested boards, plus
// the shared registry's verified Gem and SmartRecruiters boards, into one
// deduped BoardRef list.
func (s *Scheduler) collectBoardUnion(ctx context.Context, tenants []string) []scraper.BoardRef {
	seen := map[string]int{}
	var boards []scraper.BoardRef
	add := func(platform, slug, label string, wd int, board string) {
		slug = strings.TrimSpace(slug)
		if slug == "" {
			return
		}
		key := platform + "|" + strings.ToLower(slug)
		if platform == "workday" {
			// One Workday tenant can host several brand boards, so the board is
			// part of the identity. Its casing is not: Workday serves the same
			// site for any casing of the board path (intel/External vs
			// intel/external, paypal/Jobs vs paypal/jobs, verified 2026-07-05),
			// and registry harvesting created lowercase twins of labeled entries
			// whose duplicate fetches doubled the 429 pressure on the host.
			key += "|" + strings.ToLower(board)
			if wd == 0 || board == "" {
				return
			}
		}
		if i, ok := seen[key]; ok {
			// Keep the labeled spelling when a twin carries one: the label is
			// the company name leads are attributed to and displayed under.
			if label != "" && boards[i].Label == "" {
				boards[i] = scraper.BoardRef{Platform: platform, Slug: slug, Label: label, WD: wd, Board: board}
			}
			return
		}
		seen[key] = len(boards)
		boards = append(boards, scraper.BoardRef{Platform: platform, Slug: slug, Label: label, WD: wd, Board: board})
	}

	for _, uid := range tenants {
		dir := db.TenantDataDir(s.dataDir, s.repo.DBType(), uid)
		cc, err := discovery.LoadCompanyConfig(dir)
		if err == nil {
			for _, c := range cc.Greenhouse {
				add("greenhouse", c, "", 0, "")
			}
			for _, c := range cc.Lever {
				add("lever", c, "", 0, "")
			}
			for _, c := range cc.Ashby {
				add("ashby", c, "", 0, "")
			}
			for _, c := range cc.Workable {
				add("workable", c, "", 0, "")
			}
			for _, w := range cc.Workday {
				add("workday", w.Sub, w.Label, w.WD, w.Board)
			}
			for _, c := range cc.Gem {
				add("gem", c, "", 0, "")
			}
			for _, c := range cc.SmartRecruiters {
				add("smartrecruiters", c, "", 0, "")
			}
		}
		sugg := discovery.LoadSuggested(dir)
		for _, c := range sugg.Greenhouse {
			add("greenhouse", c, "", 0, "")
		}
		for _, c := range sugg.Lever {
			add("lever", c, "", 0, "")
		}
		for _, c := range sugg.Ashby {
			add("ashby", c, "", 0, "")
		}
		for _, w := range sugg.Workday {
			add("workday", w.Sub, w.Label, w.WD, w.Board)
		}
		for _, c := range sugg.Gem {
			add("gem", c, "", 0, "")
		}
		for _, c := range sugg.SmartRecruiters {
			add("smartrecruiters", c, "", 0, "")
		}
	}

	// Fold the shared registry's verified boards into the union directly so they
	// get scraped every cycle, regardless of whether any tenant's search terms
	// happened to LIKE-match them into a config. The registry is seeded LLM-free
	// from web-archive and Common Crawl discovery and holds ~17k boards; before
	// this fold only Gem and SmartRecruiters were pulled, on the assumption that
	// Greenhouse/Ashby/Lever boards reached the scraper through keyword-gated
	// distribution. In practice that gate left the vast majority dormant: of the
	// ~12k GH/Ashby/Lever registry boards only ~1.3k were ever fetched, so real
	// boards (Fluidstack, Lambda, Iterable, Spring Health, ...) that no tenant's
	// terms matched were never scraped at all. These three platforms support
	// etag/content-hash conditional fetches (see incrementalBoardPlatforms), so
	// unchanged boards cost a 304 and steady-state stays cheap. distributeTenant
	// still filters each tenant's queue by its own search terms afterward, so
	// scoring stays bounded by term match no matter how many boards are scraped.
	//
	// Workday is folded too, but throttled: it has no conditional-fetch path
	// (every board is a full fetch) and is 429-prone, so selectWorkdayRegistrySlice
	// rotates a bounded slice through the union per cycle via a persisted cursor
	// instead of pulling all ~5k every cycle.
	//
	// The registry platforms are interleaved round-robin rather than appended one
	// full platform at a time. The board scrape processes the union in order under
	// a fixed cycle timeout, so a platform with far more boards (registry
	// Greenhouse ~6.7k vs Ashby ~3.3k, Lever ~2.4k) would otherwise monopolize the
	// budget and starve the platforms after it: verified 2026-07-08, a cold first
	// cycle spent its whole 60m in registry Greenhouse (3.3k companies harvested)
	// and never reached registry Ashby/Lever (only the ~10 tenant-config boards
	// each landed, Fluidstack/Lambda/Raptive stranded). Round-robin gives every
	// platform a proportional share each cycle, so etags warm across all of them
	// and coverage converges evenly even when one cold cycle can't fetch the lot.
	if s.repo != nil {
		if reg, err := s.repo.CompanyRegistry().Known(ctx); err != nil {
			s.log.Warn("registry board load failed", "error", err)
		} else {
			regLists := []struct {
				platform string
				slugs    []string
			}{
				{"greenhouse", reg.Greenhouse},
				{"ashby", reg.Ashby},
				{"lever", reg.Lever},
				{"gem", reg.Gem},
				{"smartrecruiters", reg.SmartRecruiters},
			}
			wd := s.selectWorkdayRegistrySlice(reg.Workday)
			maxLen := len(wd)
			for _, rl := range regLists {
				if len(rl.slugs) > maxLen {
					maxLen = len(rl.slugs)
				}
			}
			for i := 0; i < maxLen; i++ {
				for _, rl := range regLists {
					if i < len(rl.slugs) {
						add(rl.platform, rl.slugs[i], "", 0, "")
					}
				}
				if i < len(wd) {
					add("workday", wd[i].Sub, wd[i].Label, wd[i].WD, wd[i].Board)
				}
			}
		}
	}
	return boards
}

// workdayRegistryCursorFile persists the global rotation position across the
// registry's Workday boards (which board key the last cycle stopped at).
const workdayRegistryCursorFile = "workday-registry-cursor.json"

type workdayRegistryCursor struct {
	LastKey string `json:"lastKey"`
}

// workdayEntryKey is the stable per-board identity used for both cursor ordering
// and dedupe. It mirrors the union's Workday identity (sub + wd + board, case
// folded) so rotation advances deterministically regardless of stored casing.
func workdayEntryKey(w discovery.WorkdayEntry) string {
	return strings.ToLower(w.Sub) + "|" + strconv.Itoa(w.WD) + "|" + strings.ToLower(w.Board)
}

// foldWorkdayRegistry adds a bounded, rotating slice of the registry's Workday
// boards to the union via add. Thin wrapper over selectWorkdayRegistrySlice.
func (s *Scheduler) foldWorkdayRegistry(entries []discovery.WorkdayEntry, add func(platform, slug, label string, wd int, board string)) {
	for _, w := range s.selectWorkdayRegistrySlice(entries) {
		add("workday", w.Sub, w.Label, w.WD, w.Board)
	}
}

// selectWorkdayRegistrySlice returns the bounded, rotating slice of the
// registry's Workday boards to fold this cycle, advancing the persisted cursor.
// Because Workday fetches are full (no etag) and 429-prone, pulling all ~5k
// every cycle would risk the cycle timeout and rate limits; instead the cursor
// walks the sorted board list a WorkdayRegistryBatch slice at a time, wrapping
// around, so every board is refreshed within a few cycles. distributeTenant
// still filters each tenant's queue by search terms, so scoring stays bounded.
func (s *Scheduler) selectWorkdayRegistrySlice(entries []discovery.WorkdayEntry) []discovery.WorkdayEntry {
	batch := 0
	if s.global != nil {
		batch = s.global.WorkdayRegistryBatch
	}
	if batch == 0 || len(entries) == 0 {
		return nil // fold disabled or nothing to fold
	}

	sorted := make([]discovery.WorkdayEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return workdayEntryKey(sorted[i]) < workdayEntryKey(sorted[j])
	})

	// Batch covers (or exceeds) the whole list: fold all, no rotation needed.
	if batch < 0 || batch >= len(sorted) {
		return sorted
	}

	start := 0
	if last := s.readWorkdayRegistryCursor(); last != "" {
		// Resume at the first board strictly after last cycle's stopping point;
		// if the cursor points past the end (list shrank), restart from 0.
		start = sort.Search(len(sorted), func(i int) bool { return workdayEntryKey(sorted[i]) > last })
		if start >= len(sorted) {
			start = 0
		}
	}

	out := make([]discovery.WorkdayEntry, 0, batch)
	for i := 0; i < batch; i++ {
		out = append(out, sorted[(start+i)%len(sorted)])
	}
	s.writeWorkdayRegistryCursor(workdayEntryKey(out[len(out)-1]))
	return out
}

func (s *Scheduler) readWorkdayRegistryCursor() string {
	data, err := os.ReadFile(filepath.Join(s.dataDir, workdayRegistryCursorFile))
	if err != nil {
		return ""
	}
	var c workdayRegistryCursor
	if json.Unmarshal(data, &c) != nil {
		return ""
	}
	return c.LastKey
}

func (s *Scheduler) writeWorkdayRegistryCursor(key string) {
	data, err := json.Marshal(workdayRegistryCursor{LastKey: key})
	if err != nil {
		return
	}
	if err := os.WriteFile(filepath.Join(s.dataDir, workdayRegistryCursorFile), data, 0o644); err != nil {
		s.log.Warn("workday registry cursor write failed", "error", err)
	}
}

// distributionCursor persists a tenant's incremental distribution position in
// its profile dir.
const distributionCursorFile = "distribution-cursor.json"

type distributionCursor struct {
	LastSeenAt string `json:"lastSeenAt"`
}

func readDistributionCursor(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, distributionCursorFile))
	if err != nil {
		return ""
	}
	var c distributionCursor
	if json.Unmarshal(data, &c) != nil {
		return ""
	}
	return c.LastSeenAt
}

func writeDistributionCursor(dir, lastSeenAt string) error {
	out, err := json.MarshalIndent(distributionCursor{LastSeenAt: lastSeenAt}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, distributionCursorFile), append(out, '\n'), 0o644)
}

// tenantWatch is a tenant's distribution identity: which companies it watches
// and which search terms it matches titles against.
type tenantWatch struct {
	companies []string // lowercased
	terms     []string
}

func loadTenantWatch(dir string) tenantWatch {
	var w tenantWatch
	seen := map[string]bool{}
	addCompany := func(c string) {
		c = strings.ToLower(strings.TrimSpace(c))
		if c == "" || seen[c] {
			return
		}
		seen[c] = true
		w.companies = append(w.companies, c)
	}
	cc, err := discovery.LoadCompanyConfig(dir)
	if err == nil {
		// Widen the entered terms to their phrasing variants (deterministic, no
		// LLM/IO) so the title pre-filter catches the many ways employers phrase
		// the same role; only ADDS terms, so it never matches fewer jobs.
		w.terms = searchterms.Expand(cc.SearchTerms)
		for _, lists := range [][]string{cc.Greenhouse, cc.Lever, cc.Ashby, cc.Workable, cc.Rippling} {
			for _, c := range lists {
				addCompany(c)
			}
		}
		for _, wd := range cc.Workday {
			// Workday leads store the label as company (slug when label-less).
			addCompany(wd.Label)
			addCompany(wd.Sub)
		}
	}
	sugg := discovery.LoadSuggested(dir)
	for _, lists := range [][]string{sugg.Greenhouse, sugg.Lever, sugg.Ashby} {
		for _, c := range lists {
			addCompany(c)
		}
	}
	for _, wd := range sugg.Workday {
		addCompany(wd.Label)
		addCompany(wd.Sub)
	}
	return w
}

// distributeTenant feeds one tenant's queue from the global cache (rows past
// its cursor that match its watchlist companies or search terms) and then
// marks board-vanished rows closed. Best-effort: errors are logged, never
// fatal to the cycle. Returns how many rows were inserted.
func (s *Scheduler) distributeTenant(ctx context.Context, repo *db.Repository, gc globalCycle, log *slog.Logger) int {
	dir := repo.ProfileDir(s.dataDir)
	watch := loadTenantWatch(dir)
	if len(watch.companies) == 0 && len(watch.terms) == 0 {
		return 0
	}

	cursor := readDistributionCursor(dir)
	if cursor == "" && s.global.InitialMaxAgeDays > 0 {
		cursor = time.Now().UTC().AddDate(0, 0, -s.global.InitialMaxAgeDays).Format("2006-01-02 15:04:05")
	}

	candidates, err := repo.GlobalLeadsForDistribution(cursor, watch.companies, watch.terms, s.global.DistributeLimit)
	if err != nil {
		log.Warn("distribution query failed", "user_id", repo.UserID(), "error", err)
		return 0
	}

	watchSet := map[string]bool{}
	for _, c := range watch.companies {
		watchSet[c] = true
	}
	inserted, maxSeen := 0, cursor
	// emit applies the exact worker-parity term matcher (the SQL LIKE prefilter
	// is broad) then inserts, so distribution keeps the same jobs the per-tenant
	// scrape path would have. Watchlist companies skip the term check only if the
	// tenant has no terms at all (matching worker behavior of filtering titles on
	// every company-based platform). Shared by the forward pass and the backfill.
	emit := func(cand db.DistributionLead) {
		if !jobfilter.MatchesSearchTerms(cand.Lead.Title, watch.terms) &&
			!(len(watch.terms) == 0 && watchSet[strings.ToLower(cand.Lead.Company)]) {
			return
		}
		ok, err := repo.InsertDistributedLead(cand.Lead)
		if err != nil {
			log.Warn("distribution insert failed", "id", cand.Lead.ID, "error", err)
			return
		}
		if !ok {
			return
		}
		inserted++
		jobID := repo.JobRowID(cand.Lead.ID)
		if err := repo.LogEvent(jobID, "scraped", "", cand.Lead.ATSPlatformName); err != nil {
			log.Warn("log event failed", "id", jobID, "error", err)
		}
		metrics.ObserveScrapedJob(cand.Lead.ATSPlatformName)
	}
	for _, cand := range candidates {
		if cand.LastSeenAt > maxSeen {
			maxSeen = cand.LastSeenAt
		}
		emit(cand)
	}
	// Grace-window backfill: recover rows stranded just below the cursor by the
	// within-pass write-order race (boards land in global_jobs at different times
	// within one scrape pass, so a row scraped early gets a lower last_seen_at
	// than the row the cursor jumped to). Re-offer the window below the cursor
	// newest-first without touching maxSeen; InsertDistributedLead dedups, so
	// already-distributed rows are no-ops. Skipped on the first pass (no cursor),
	// which already scans wide via InitialMaxAgeDays.
	if g := s.global.DistributeGraceHours; g > 0 && cursor != "" {
		if t, perr := time.Parse("2006-01-02 15:04:05", cursor); perr == nil {
			since := t.Add(-time.Duration(g) * time.Hour).Format("2006-01-02 15:04:05")
			back, berr := repo.GlobalLeadsBackfill(since, cursor, watch.companies, watch.terms, s.global.DistributeLimit)
			if berr != nil {
				log.Warn("distribution backfill query failed", "user_id", repo.UserID(), "error", berr)
			}
			for _, cand := range back {
				emit(cand)
			}
		}
	}
	if maxSeen != cursor || readDistributionCursor(dir) == "" {
		if err := writeDistributionCursor(dir, maxSeen); err != nil {
			log.Warn("distribution cursor write failed", "user_id", repo.UserID(), "error", err)
		}
	}

	closed := s.closeVanished(ctx, repo, gc, log)
	reopened := s.reopenResurfaced(repo, gc, log)
	if inserted > 0 || closed > 0 || reopened > 0 {
		log.Info("distribution complete", "user_id", repo.UserID(),
			"inserted", inserted, "closed_by_diff", closed, "reopened", reopened,
			"scanned", len(candidates))
	}
	metrics.ObserveDistribution(inserted, closed)
	// Per-tenant distribution recency: reaching this point means distribution ran
	// for this tenant this cycle. A tenant whose last-distribution timestamp stops
	// advancing is being starved (the board scrape ate the cycle budget), which is
	// the failure this metric exists to make visible.
	metrics.SetTenantDistribution(repo.UserID(), inserted, time.Now())
	return inserted
}

// maxDiffClosesPerCompany is a circuit breaker on the closed-by-diff pass: no
// real board sheds this many of one tenant's postings in a single six-hour
// cycle, so a vanished set this large means the fetch was silently partial in
// a way the completeness reporting didn't catch. Those closes are suppressed
// and logged; a genuinely emptied board settles over later cycles or via the
// closedcheck probes.
const maxDiffClosesPerCompany = 25

// closeVanished marks this tenant's rows closed when their posting vanished
// from a board the global pass successfully fetched in full AND has stayed
// missing for the close grace window: one missed listing is routinely a flap,
// not a removal, so the staleness cutoff is the pass start shifted back by
// the grace period. Workday candidates additionally get a per-posting
// liveness probe (when configured): listing truncation has read as complete
// before, and a posting those fetches never re-see goes stale past any grace
// window, so absence from listings alone never suffices there.
func (s *Scheduler) closeVanished(ctx context.Context, repo *db.Repository, gc globalCycle, log *slog.Logger) int {
	cutoff := graceCutoff(gc.cutoff, s.global.CloseGraceHours)
	closed := 0
	for platform, companies := range gc.fetched {
		vanished, err := repo.VanishedBoardJobs(platform, companies, cutoff)
		if err != nil {
			log.Warn("board diff query failed", "user_id", repo.UserID(), "platform", platform, "error", err)
			continue
		}
		byCompany := map[string][]db.VanishedBoardJob{}
		for _, v := range vanished {
			byCompany[v.Company] = append(byCompany[v.Company], v)
		}
		for company, jobs := range byCompany {
			if len(jobs) > maxDiffClosesPerCompany {
				log.Warn("board diff mass-close suppressed",
					"user_id", repo.UserID(), "platform", platform, "company", company, "vanished", len(jobs))
				continue
			}
			for _, v := range jobs {
				if gc.verify != nil && platform == "workday" {
					switch gc.verify.check(ctx, v.URL) {
					case "open":
						log.Warn("board diff close vetoed, posting live on its board",
							"user_id", repo.UserID(), "id", v.ID, "url", v.URL)
						if err := repo.TouchGlobalJobSeen(v.GlobalID); err != nil {
							log.Warn("global job touch failed", "id", v.GlobalID, "error", err)
						}
						continue
					case "":
						log.Warn("board diff close skipped, liveness probe inconclusive",
							"user_id", repo.UserID(), "id", v.ID, "url", v.URL)
						continue
					}
				}
				if err := repo.SetPipelineStage(v.ID, "closed"); err != nil {
					log.Warn("board diff close failed", "id", v.ID, "error", err)
					continue
				}
				closed++
			}
		}
	}
	return closed
}

// graceCutoff shifts the pass-start cutoff back by the close grace window. A
// non-positive grace or an unparseable cutoff returns the cutoff unchanged
// (close on first miss, the pre-grace behavior).
func graceCutoff(cutoff string, graceHours int) string {
	if graceHours <= 0 {
		return cutoff
	}
	t, err := time.Parse("2006-01-02 15:04:05", cutoff)
	if err != nil {
		return cutoff
	}
	return t.Add(-time.Duration(graceHours) * time.Hour).Format("2006-01-02 15:04:05")
}

// reopenResurfaced restores this tenant's diff/probe-closed rows whose
// posting was seen on a board again this pass: a fresh sighting proves the
// posting is live, so the close was wrong (listing flap, case-drifted twin)
// or the job was reposted. Without this, a false close was permanent, because
// distribution never re-inserts a row that already exists. Uses the pass
// cutoff, not the grace-shifted one: only a sighting by THIS cycle's fetch is
// evidence.
func (s *Scheduler) reopenResurfaced(repo *db.Repository, gc globalCycle, log *slog.Logger) int {
	ids, err := repo.ResurfacedClosedJobs(gc.cutoff)
	if err != nil {
		log.Warn("resurfaced query failed", "user_id", repo.UserID(), "error", err)
		return 0
	}
	reopened := 0
	for _, id := range ids {
		if err := repo.SetPipelineStage(id, ""); err != nil {
			log.Warn("reopen failed", "id", id, "error", err)
			continue
		}
		reopened++
	}
	return reopened
}
