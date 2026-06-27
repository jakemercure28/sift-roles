package dashboard

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"job-search-automation/internal/db"
)

// This file ports the deterministic analytics layer (lib/analytics-audit.js
// computeAnalyticsMetrics + lib/stage-stats.js + lib/atsDetector.js) to Go.
// Verified by JSON parity against Node (analyticsdata_test.go golden).

const (
	staleDaysConst             = 21
	highScoreConst             = 8
	lowScoreConst              = 4
	minATSSample               = 5
	minCalibrationSample       = 3
	dayMS                int64 = 86400000
	weekMS               int64 = 7 * 86400000
)

var (
	trackedStageSet  = map[string]bool{"phone_screen": true, "interview": true, "onsite": true, "offer": true}
	interviewPlusSet = map[string]bool{"interview": true, "onsite": true, "offer": true}
	terminalStageSet = map[string]bool{"closed": true, "rejected": true, "ghosted": true}
	activeStatusSet  = map[string]bool{"applied": true, "responded": true}
)

// --- atsDetector port ---

type atsPattern struct {
	platform string
	re       *regexp.Regexp
}

var atsPatterns = []atsPattern{
	{"Ashby", regexp.MustCompile(`^https?://jobs\.ashbyhq\.com/([^/?#]+)`)},
	{"Greenhouse", regexp.MustCompile(`^https?://(?:boards|job-boards)\.greenhouse\.io/([^/?#]+)`)},
	{"Lever", regexp.MustCompile(`^https?://jobs\.lever\.co/([^/?#]+)`)},
	{"Workable", regexp.MustCompile(`^https?://apply\.workable\.com/([^/?#]+)`)},
	{"Workday", regexp.MustCompile(`^https?://([^/.]+)\.(?:wd\d+\.|)myworkdayjobs\.com/`)},
	{"Rippling", regexp.MustCompile(`^https?://ats\.rippling\.com/([^/?#]+)/`)},
}

var ghJidRe = regexp.MustCompile(`[?&]gh_jid=\d+`)

// atsOf returns the ATS platform for a URL, or "Other" (atsOf in analytics-audit.js).
func atsOf(url string) string {
	if url == "" {
		return "Other"
	}
	if ghJidRe.MatchString(url) {
		return "Greenhouse"
	}
	for _, p := range atsPatterns {
		if p.re.MatchString(url) {
			return p.platform
		}
	}
	return "Other"
}

// --- timestamp helpers (parseTs/daysSince) ---

var tzOffsetRe = regexp.MustCompile(`[+-]\d\d:?\d\d$`)

func parseTs(s string) (int64, bool) {
	v := strings.TrimSpace(s)
	if v == "" {
		return 0, false
	}
	if !strings.ContainsAny(v, "zZ") && !tzOffsetRe.MatchString(v) {
		if strings.Contains(v, "T") {
			v += "Z"
		} else {
			v = strings.Replace(v, " ", "T", 1) + "Z"
		}
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UnixMilli(), true
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t.UnixMilli(), true
	}
	return 0, false
}

func daysSince(s string, now int64) (float64, bool) {
	t, ok := parseTs(s)
	if !ok {
		return 0, false
	}
	return float64(now-t) / float64(dayMS), true
}

// jsRound matches JS Math.round (round half toward +Infinity).
func jsRound(x float64) int { return int(math.Floor(x + 0.5)) }

// --- metric value types (JSON shape matches Node) ---

type Rate struct {
	N   int  `json:"n"`
	D   int  `json:"d"`
	Pct *int `json:"pct"`
}

func newRate(n, d int) Rate {
	var pct *int
	if d > 0 {
		p := jsRound(float64(n) / float64(d) * 100)
		pct = &p
	}
	return Rate{N: n, D: d, Pct: pct}
}

type Contributor struct {
	ID               string  `json:"id"`
	Title            string  `json:"title"`
	Company          string  `json:"company"`
	Score            *int    `json:"score"`
	Status           string  `json:"status"`
	Stage            string  `json:"stage"`
	ATS              string  `json:"ats"`
	AppliedAt        *string `json:"applied_at"`
	DaysSinceApplied *int    `json:"daysSinceApplied"`
}

func contributorOf(j db.AnalyticsJob, now int64) Contributor {
	status := j.Status
	if status == "" {
		status = "pending"
	}
	c := Contributor{
		ID: j.ID, Title: j.Title, Company: j.Company, Score: j.Score,
		Status: status, Stage: j.Stage, ATS: atsOf(j.URL), AppliedAt: j.AppliedAt,
	}
	if j.AppliedAt != nil {
		if ds, ok := daysSince(*j.AppliedAt, now); ok {
			d := jsRound(ds)
			c.DaysSinceApplied = &d
		}
	}
	return c
}

func mapContributors(jobs []db.AnalyticsJob, now int64) []Contributor {
	out := make([]Contributor, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, contributorOf(j, now))
	}
	return out
}

type ScoreCalRow struct {
	Score         int  `json:"score"`
	Total         int  `json:"total"`
	Advanced      int  `json:"advanced"`
	Rejected      int  `json:"rejected"`
	AdvanceRate   Rate `json:"advanceRate"`
	LowConfidence bool `json:"lowConfidence"`
}

type ATSRow struct {
	Platform    string        `json:"platform"`
	Applied     int           `json:"applied"`
	Advanced    int           `json:"advanced"`
	Rejected    int           `json:"rejected"`
	AdvanceRate Rate          `json:"advanceRate"`
	LowSample   bool          `json:"lowSample"`
	Jobs        []Contributor `json:"jobs"`
}

type Week struct {
	Label        string `json:"label"`
	Applications int    `json:"applications"`
	Responses    int    `json:"responses"`
	Interviews   int    `json:"interviews"`
}

type WeekTotals struct {
	Applications int `json:"applications"`
	Responses    int `json:"responses"`
	Interviews   int `json:"interviews"`
}

type Activity struct {
	Weeks  []Week     `json:"weeks"`
	Totals WeekTotals `json:"totals"`
}

type Thresholds struct {
	HighScore            int `json:"highScore"`
	LowScore             int `json:"lowScore"`
	MinAtsSample         int `json:"minAtsSample"`
	MinCalibrationSample int `json:"minCalibrationSample"`
}

type Health struct {
	Active        int  `json:"active"`
	Applied       int  `json:"applied"`
	Responded     int  `json:"responded"`
	Interviews    int  `json:"interviews"`
	Rejected      int  `json:"rejected"`
	Stale         int  `json:"stale"`
	ResponseRate  Rate `json:"responseRate"`
	InterviewRate Rate `json:"interviewRate"`
}

type Funnel struct {
	Applied     int `json:"applied"`
	PhoneScreen int `json:"phone_screen"`
	Interview   int `json:"interview"`
	Onsite      int `json:"onsite"`
	Offer       int `json:"offer"`
}

type ActionSet struct {
	Count int           `json:"count"`
	Jobs  []Contributor `json:"jobs"`
}

type Actions struct {
	Stale              ActionSet `json:"stale"`
	HighScoreUnapplied ActionSet `json:"highScoreUnapplied"`
	LowScoreSink       ActionSet `json:"lowScoreSink"`
}

type Contributors struct {
	Applied   []Contributor `json:"applied"`
	Active    []Contributor `json:"active"`
	Responded []Contributor `json:"responded"`
	Rejected  []Contributor `json:"rejected"`
	Advanced  []Contributor `json:"advanced"`
}

type Reconciliation struct {
	AppliedAtOnly                    int           `json:"appliedAtOnly"`
	AppliedEventOnly                 int           `json:"appliedEventOnly"`
	AdvancedWithoutAppliedMarker     int           `json:"advancedWithoutAppliedMarker"`
	PendingWithAppliedAt             int           `json:"pendingWithAppliedAt"`
	AdvancedWithoutAppliedMarkerJobs []Contributor `json:"advancedWithoutAppliedMarkerJobs"`
}

type Last7 struct {
	Applications int `json:"applications"`
	Responses    int `json:"responses"`
	Interviews   int `json:"interviews"`
}

// AnalyticsMetrics matches the object returned by computeAnalyticsMetrics.
type AnalyticsMetrics struct {
	AsOf             int64          `json:"asOf"`
	StaleDays        int            `json:"staleDays"`
	Thresholds       Thresholds     `json:"thresholds"`
	Health           Health         `json:"health"`
	Funnel           Funnel         `json:"funnel"`
	ScoreCalibration []ScoreCalRow  `json:"scoreCalibration"`
	ATS              []ATSRow       `json:"ats"`
	Activity         Activity       `json:"activity"`
	Last7            Last7          `json:"last7"`
	Actions          Actions        `json:"actions"`
	Contributors     Contributors   `json:"contributors"`
	Reconciliation   Reconciliation `json:"reconciliation"`
}

// reached aggregates the stage-stats CTE rows (summarizeReachedRows).
type reachedStats struct {
	interviews  int
	funnel      Funnel
	advancedIDs map[string]bool
}

func summarizeReached(rows []db.ReachedRow) reachedStats {
	phone, interview, onsite, offer := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range rows {
		switch r.Stage {
		case "phone_screen":
			phone[r.JobID] = true
		case "interview":
			interview[r.JobID] = true
		case "onsite":
			onsite[r.JobID] = true
		case "offer":
			offer[r.JobID] = true
		}
	}
	interviewOrBeyond := unionSets(interview, onsite, offer)
	onsiteOrBeyond := unionSets(onsite, offer)
	anyAdvanced := unionSets(phone, interview, onsite, offer)
	return reachedStats{
		interviews:  len(interviewOrBeyond),
		funnel:      Funnel{PhoneScreen: len(phone), Interview: len(interviewOrBeyond), Onsite: len(onsiteOrBeyond), Offer: len(offer)},
		advancedIDs: anyAdvanced,
	}
}

func unionSets(sets ...map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, s := range sets {
		for k := range s {
			out[k] = true
		}
	}
	return out
}

// computeAnalyticsPageMetrics builds only the metrics rendered by the Analytics
// page. The full audit path intentionally stays on computeAnalyticsMetrics.
func computeAnalyticsPageMetrics(repo *db.Repository, now int64) (AnalyticsMetrics, error) {
	jobs, err := repo.AnalyticsJobs()
	if err != nil {
		return AnalyticsMetrics{}, err
	}
	events, err := repo.AnalyticsStageEvents()
	if err != nil {
		return AnalyticsMetrics{}, err
	}
	reachedRows, err := repo.ReachedStageRows(db.ReachedCutoff(time.UnixMilli(now)))
	if err != nil {
		return AnalyticsMetrics{}, err
	}
	return computeAnalyticsPageMetricsFromInputs(jobs, events, reachedRows, now), nil
}

func computeAnalyticsPageMetricsFromInputs(jobs []db.AnalyticsJob, events []db.AnalyticsEvent, reachedRows []db.ReachedRow, now int64) AnalyticsMetrics {
	hist := summarizeReached(reachedRows)

	jobIDs := map[string]bool{}
	appliedAtIDs := map[string]bool{}
	appliedEventIDs := map[string]bool{}
	firstAppliedEvent := map[string]string{}
	for _, j := range jobs {
		jobIDs[j.ID] = true
		if j.AppliedAt != nil && *j.AppliedAt != "" {
			appliedAtIDs[j.ID] = true
		}
	}
	for _, e := range events {
		if e.EventType != "stage_change" || !jobIDs[e.JobID] {
			continue
		}
		if e.ToValue == "applied" {
			appliedEventIDs[e.JobID] = true
			if e.CreatedAt != "" {
				if cur, ok := firstAppliedEvent[e.JobID]; !ok || e.CreatedAt < cur {
					firstAppliedEvent[e.JobID] = e.CreatedAt
				}
			}
		}
	}

	appliedIDs := unionSets(appliedAtIDs, appliedEventIDs, hist.advancedIDs)
	var appliedJobs []db.AnalyticsJob
	for _, j := range jobs {
		if !appliedIDs[j.ID] {
			continue
		}
		jj := j
		if (jj.AppliedAt == nil || *jj.AppliedAt == "") && firstAppliedEvent[jj.ID] != "" {
			s := firstAppliedEvent[jj.ID]
			jj.AppliedAt = &s
		}
		appliedJobs = append(appliedJobs, jj)
	}

	advancedIDs := map[string]bool{}
	for id := range hist.advancedIDs {
		if appliedIDs[id] {
			advancedIDs[id] = true
		}
	}

	rejectedIDs := map[string]bool{}
	for _, e := range events {
		if e.EventType == "stage_change" && e.ToValue == "rejected" && appliedIDs[e.JobID] {
			rejectedIDs[e.JobID] = true
		}
	}
	for _, j := range appliedJobs {
		if j.Stage == "rejected" {
			rejectedIDs[j.ID] = true
		}
	}

	respondedIDs := map[string]bool{}
	for id := range advancedIDs {
		respondedIDs[id] = true
	}
	for id := range rejectedIDs {
		respondedIDs[id] = true
	}

	active := 0
	for _, j := range jobs {
		if activeStatusSet[j.Status] && !terminalStageSet[j.Stage] {
			active++
		}
	}

	stale := 0
	for _, j := range appliedJobs {
		dv := 0.0
		if j.AppliedAt != nil {
			if ds, ok := daysSince(*j.AppliedAt, now); ok {
				dv = ds
			}
		}
		if j.Status == "applied" && !trackedStageSet[j.Stage] && !terminalStageSet[j.Stage] &&
			!respondedIDs[j.ID] && dv >= staleDaysConst {
			stale++
		}
	}

	applied := len(appliedJobs)
	responded := len(respondedIDs)
	rejected := len(rejectedIDs)
	return AnalyticsMetrics{
		AsOf:      now,
		StaleDays: staleDaysConst,
		Thresholds: Thresholds{
			HighScore: highScoreConst, LowScore: lowScoreConst,
			MinAtsSample: minATSSample, MinCalibrationSample: minCalibrationSample,
		},
		Health: Health{
			Active: active, Applied: applied, Responded: responded,
			Interviews: hist.interviews, Rejected: rejected, Stale: stale,
			ResponseRate: newRate(responded, applied), InterviewRate: newRate(hist.interviews, applied),
		},
		Funnel: Funnel{Applied: applied, PhoneScreen: hist.funnel.PhoneScreen, Interview: hist.funnel.Interview, Onsite: hist.funnel.Onsite, Offer: hist.funnel.Offer},
	}
}

// computeAnalyticsMetrics ports computeAnalyticsMetrics(db, now). now is epoch ms.
func computeAnalyticsMetrics(repo *db.Repository, now int64) (AnalyticsMetrics, error) {
	jobs, err := repo.AnalyticsJobs()
	if err != nil {
		return AnalyticsMetrics{}, err
	}
	events, err := repo.AnalyticsEvents()
	if err != nil {
		return AnalyticsMetrics{}, err
	}
	reachedRows, err := repo.ReachedStageRows(db.ReachedCutoff(time.UnixMilli(now)))
	if err != nil {
		return AnalyticsMetrics{}, err
	}
	isPending := func(j db.AnalyticsJob) bool {
		st := j.Status
		if st == "" {
			st = "pending"
		}
		return st == "pending"
	}

	hist := summarizeReached(reachedRows)

	jobIDs := map[string]bool{}
	appliedAtIDs := map[string]bool{}
	appliedEventIDs := map[string]bool{}
	firstAppliedEvent := map[string]string{}
	for _, j := range jobs {
		jobIDs[j.ID] = true
		if j.AppliedAt != nil && *j.AppliedAt != "" {
			appliedAtIDs[j.ID] = true
		}
	}
	for _, e := range events {
		if e.EventType != "stage_change" || e.ToValue != "applied" || !jobIDs[e.JobID] {
			continue
		}
		appliedEventIDs[e.JobID] = true
		if e.CreatedAt != "" {
			if cur, ok := firstAppliedEvent[e.JobID]; !ok || e.CreatedAt < cur {
				firstAppliedEvent[e.JobID] = e.CreatedAt
			}
		}
	}

	// Cohorts. All-time applied is historical: current applied_at, explicit
	// applied events, or reached-stage evidence all count even if the current row
	// is now rejected, closed, ghosted, archived, or was imported incompletely.
	appliedIDs := unionSets(appliedAtIDs, appliedEventIDs, hist.advancedIDs)
	var appliedJobs []db.AnalyticsJob
	var advancedWithoutAppliedMarker []db.AnalyticsJob
	for _, j := range jobs {
		if !appliedIDs[j.ID] {
			continue
		}
		jj := j
		if (jj.AppliedAt == nil || *jj.AppliedAt == "") && firstAppliedEvent[jj.ID] != "" {
			s := firstAppliedEvent[jj.ID]
			jj.AppliedAt = &s
		}
		appliedJobs = append(appliedJobs, jj)
		if hist.advancedIDs[j.ID] && !appliedAtIDs[j.ID] && !appliedEventIDs[j.ID] {
			advancedWithoutAppliedMarker = append(advancedWithoutAppliedMarker, jj)
		}
	}

	advancedIDs := map[string]bool{}
	for id := range hist.advancedIDs {
		if appliedIDs[id] {
			advancedIDs[id] = true
		}
	}

	rejectedIDs := map[string]bool{}
	for _, e := range events {
		if e.EventType == "stage_change" && e.ToValue == "rejected" && appliedIDs[e.JobID] {
			rejectedIDs[e.JobID] = true
		}
	}
	for _, j := range appliedJobs {
		if j.Stage == "rejected" {
			rejectedIDs[j.ID] = true
		}
	}

	respondedIDs := map[string]bool{}
	for id := range advancedIDs {
		respondedIDs[id] = true
	}
	for id := range rejectedIDs {
		respondedIDs[id] = true
	}

	// Current-state sets.
	var activeJobs []db.AnalyticsJob
	for _, j := range jobs {
		if activeStatusSet[j.Status] && !terminalStageSet[j.Stage] {
			activeJobs = append(activeJobs, j)
		}
	}

	var staleJobs []db.AnalyticsJob
	for _, j := range appliedJobs {
		dv := 0.0
		if j.AppliedAt != nil {
			if ds, ok := daysSince(*j.AppliedAt, now); ok {
				dv = ds
			}
		}
		if j.Status == "applied" && !trackedStageSet[j.Stage] && !terminalStageSet[j.Stage] &&
			!respondedIDs[j.ID] && dv >= staleDaysConst {
			staleJobs = append(staleJobs, j)
		}
	}

	// Recommended-move sets.
	var highScoreUnapplied []db.AnalyticsJob
	for _, j := range jobs {
		score := 0
		if j.Score != nil {
			score = *j.Score
		}
		notApplied := !appliedIDs[j.ID]
		if score >= highScoreConst && notApplied &&
			!(j.Status == "archived" || j.Status == "closed" || j.Status == "rejected" || j.Status == "ghosted") {
			highScoreUnapplied = append(highScoreUnapplied, j)
		}
	}
	sort.SliceStable(highScoreUnapplied, func(a, b int) bool {
		return scoreOr(highScoreUnapplied[a].Score, 0) > scoreOr(highScoreUnapplied[b].Score, 0)
	})

	var lowScoreSink []db.AnalyticsJob
	for _, j := range appliedJobs {
		sc := 99
		if j.Score != nil {
			sc = *j.Score
		}
		if sc <= lowScoreConst && activeStatusSet[j.Status] && !terminalStageSet[j.Stage] && !advancedIDs[j.ID] {
			lowScoreSink = append(lowScoreSink, j)
		}
	}

	// Score outcome buckets.
	type calBucket struct {
		score, total, rejected int
	}
	byScoreOrder := []int{}
	byScore := map[int]*calBucket{}
	for _, j := range appliedJobs {
		if j.Score == nil {
			continue
		}
		sc := *j.Score
		b := byScore[sc]
		if b == nil {
			b = &calBucket{score: sc}
			byScore[sc] = b
			byScoreOrder = append(byScoreOrder, sc)
		}
		b.total++
		if rejectedIDs[j.ID] {
			b.rejected++
		}
	}
	scoreCalibration := make([]ScoreCalRow, 0, len(byScoreOrder))
	for _, sc := range byScoreOrder {
		b := byScore[sc]
		advanced := 0
		for _, j := range appliedJobs {
			if j.Score != nil && *j.Score == sc && advancedIDs[j.ID] {
				advanced++
			}
		}
		scoreCalibration = append(scoreCalibration, ScoreCalRow{
			Score: b.score, Total: b.total, Advanced: advanced, Rejected: b.rejected,
			AdvanceRate: newRate(advanced, b.total), LowConfidence: b.total < minCalibrationSample,
		})
	}
	sort.SliceStable(scoreCalibration, func(a, b int) bool { return scoreCalibration[a].Score < scoreCalibration[b].Score })

	// Outcomes by ATS.
	type atsBucket struct {
		platform                    string
		applied, advanced, rejected int
		jobs                        []db.AnalyticsJob
	}
	atsOrder := []string{}
	atsMap := map[string]*atsBucket{}
	for _, j := range appliedJobs {
		p := atsOf(j.URL)
		b := atsMap[p]
		if b == nil {
			b = &atsBucket{platform: p}
			atsMap[p] = b
			atsOrder = append(atsOrder, p)
		}
		b.applied++
		if advancedIDs[j.ID] {
			b.advanced++
		}
		if rejectedIDs[j.ID] {
			b.rejected++
		}
		b.jobs = append(b.jobs, j)
	}
	ats := make([]ATSRow, 0, len(atsOrder))
	for _, p := range atsOrder {
		b := atsMap[p]
		ats = append(ats, ATSRow{
			Platform: b.platform, Applied: b.applied, Advanced: b.advanced, Rejected: b.rejected,
			AdvanceRate: newRate(b.advanced, b.applied), LowSample: b.applied < minATSSample,
			Jobs: mapContributors(b.jobs, now),
		})
	}
	sort.SliceStable(ats, func(a, b int) bool {
		if ats[a].Applied != ats[b].Applied {
			return ats[a].Applied > ats[b].Applied
		}
		return ats[a].Platform < ats[b].Platform
	})

	// Activity over last 4 weeks.
	weeks := make([]Week, 4)
	weekBounds := make([][2]int64, 4)
	for idx, i := 0, 3; i >= 0; i, idx = i-1, idx+1 {
		end := now - int64(i)*weekMS
		start := end - weekMS
		weekBounds[idx] = [2]int64{start, end}
		weeks[idx] = Week{Label: weekLabel(start, end)}
	}
	windowStart := now - 4*weekMS
	for _, e := range events {
		if e.EventType != "stage_change" {
			continue
		}
		t, ok := parseTs(e.CreatedAt)
		if !ok || t < windowStart || t > now {
			continue
		}
		wi := len(weeks) - 1
		for k := range weekBounds {
			if t >= weekBounds[k][0] && t < weekBounds[k][1] {
				wi = k
				break
			}
		}
		if e.ToValue == "applied" {
			weeks[wi].Applications++
		}
		if trackedStageSet[e.ToValue] || e.ToValue == "rejected" {
			weeks[wi].Responses++
		}
		if interviewPlusSet[e.ToValue] {
			weeks[wi].Interviews++
		}
	}
	totals := WeekTotals{}
	for _, w := range weeks {
		totals.Applications += w.Applications
		totals.Responses += w.Responses
		totals.Interviews += w.Interviews
	}
	last7 := weeks[len(weeks)-1]

	applied := len(appliedJobs)
	responded := len(respondedIDs)
	rejected := len(rejectedIDs)
	recon := Reconciliation{
		AdvancedWithoutAppliedMarker:     len(advancedWithoutAppliedMarker),
		AdvancedWithoutAppliedMarkerJobs: mapContributors(advancedWithoutAppliedMarker, now),
	}
	for _, j := range jobs {
		if appliedAtIDs[j.ID] && !appliedEventIDs[j.ID] {
			recon.AppliedAtOnly++
		}
		if appliedEventIDs[j.ID] && !appliedAtIDs[j.ID] {
			recon.AppliedEventOnly++
		}
		if appliedAtIDs[j.ID] && isPending(j) {
			recon.PendingWithAppliedAt++
		}
	}

	filterByID := func(set map[string]bool) []db.AnalyticsJob {
		var out []db.AnalyticsJob
		for _, j := range appliedJobs {
			if set[j.ID] {
				out = append(out, j)
			}
		}
		return out
	}

	return AnalyticsMetrics{
		AsOf:      now,
		StaleDays: staleDaysConst,
		Thresholds: Thresholds{
			HighScore: highScoreConst, LowScore: lowScoreConst,
			MinAtsSample: minATSSample, MinCalibrationSample: minCalibrationSample,
		},
		Health: Health{
			Active: len(activeJobs), Applied: applied, Responded: responded,
			Interviews: hist.interviews, Rejected: rejected, Stale: len(staleJobs),
			ResponseRate: newRate(responded, applied), InterviewRate: newRate(hist.interviews, applied),
		},
		Funnel:           Funnel{Applied: applied, PhoneScreen: hist.funnel.PhoneScreen, Interview: hist.funnel.Interview, Onsite: hist.funnel.Onsite, Offer: hist.funnel.Offer},
		ScoreCalibration: scoreCalibration,
		ATS:              ats,
		Activity:         Activity{Weeks: weeks, Totals: totals},
		Last7:            Last7{Applications: last7.Applications, Responses: last7.Responses, Interviews: last7.Interviews},
		Actions: Actions{
			Stale:              ActionSet{Count: len(staleJobs), Jobs: mapContributors(staleJobs, now)},
			HighScoreUnapplied: ActionSet{Count: len(highScoreUnapplied), Jobs: mapContributors(highScoreUnapplied, now)},
			LowScoreSink:       ActionSet{Count: len(lowScoreSink), Jobs: mapContributors(lowScoreSink, now)},
		},
		Contributors: Contributors{
			Applied:   mapContributors(appliedJobs, now),
			Active:    mapContributors(activeJobs, now),
			Responded: mapContributors(filterByID(respondedIDs), now),
			Rejected:  mapContributors(filterByID(rejectedIDs), now),
			Advanced:  mapContributors(filterByID(advancedIDs), now),
		},
		Reconciliation: recon,
	}, nil
}

func scoreOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// weekLabel ports weekLabel: "Mon D–Mon D", en-dash separator. Node formats in
// the process TZ; we use UTC so the output is machine-independent (the parity
// golden is generated with TZ=UTC). A non-UTC deploy may shift a label by a day
// near midnight, a cosmetic difference accepted for the migration.
func weekLabel(start, end int64) string {
	fmtDay := func(t int64) string { return time.UnixMilli(t).UTC().Format("Jan 2") }
	return fmtDay(start) + "–" + fmtDay(end-dayMS)
}

// marshalNoHTMLEscape serializes like JSON.stringify (no <, >, & escaping).
func marshalNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
