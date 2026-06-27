package dashboard

import (
	"strconv"
	"strings"
	"time"

	"job-search-automation/internal/db"
)

// This file ports lib/html/analytics.js (renderAnalytics + renderActivityLog and
// their section helpers) to Go. Verified byte-for-byte against Node goldens.

// AnalyticsData bundles what the renderers need (fetchAnalyticsData output).
type AnalyticsData struct {
	Metrics           AnalyticsMetrics
	RecentEvents      []db.RecentEvent
	RejectionInsights []db.RejectionInsight
}

func mono(s string) string {
	return `<span style="font-family:var(--font-mono)">` + s + `</span>`
}

// scorePill mirrors the analytics scorePill (uses the Jobs score ladder).
func scorePill(score *int) string {
	disp := "?"
	if score != nil {
		disp = strconv.Itoa(*score)
	}
	return `<span class="score-pill ` + scoreClass(score) + `">` + disp + `</span>`
}

func fmtJSNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
func toFixed1(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }
func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func rateNum(r Rate) string {
	if r.D > 0 && r.Pct != nil {
		return strconv.Itoa(*r.Pct) + "%"
	}
	return "—"
}
func rateSub(r Rate) string {
	if r.D > 0 {
		return strconv.Itoa(r.N) + " of " + strconv.Itoa(r.D)
	}
	return "no applications yet"
}

func renderHealthBand(h Health) string {
	item := func(numHTML, label, sub string) string {
		return `
    <div class="stat-item">
      <div class="stat-num">` + numHTML + `</div>
      <div class="stat-label">` + label + `</div>
      <div class="stat-sub">` + sub + `</div>
    </div>`
	}
	items := item(strconv.Itoa(h.Applied), "Applied", "all-time") + `
      ` + item(strconv.Itoa(h.Rejected), "Rejected", "all-time") + `
      ` + item(rateNum(h.ResponseRate), "Response rate", rateSub(h.ResponseRate)) + `
      ` + item(rateNum(h.InterviewRate), "Interview rate", rateSub(h.InterviewRate)) + `
      ` + item(strconv.Itoa(h.Active), "Active pipeline", "open right now")
	if h.Stale > 0 {
		items += `
      ` + item(strconv.Itoa(h.Stale), "Stale", `applied &gt; 21d, no reply`)
	}
	return `
  <div class="report-band">
    <h3 class="report-subhead">Pipeline health</h3>
    <div class="stat-strip">
      ` + items + `
    </div>
  </div>`
}

func renderFunnelBand(funnel Funnel) string {
	stages := [][2]string{
		{"applied", "Applied"}, {"phone_screen", "Phone screen"}, {"interview", "Interview"},
		{"onsite", "Onsite"}, {"offer", "Offer"},
	}
	funnelVal := map[string]int{
		"applied": funnel.Applied, "phone_screen": funnel.PhoneScreen, "interview": funnel.Interview,
		"onsite": funnel.Onsite, "offer": funnel.Offer,
	}
	applied := funnel.Applied
	// Aligned columns with mini bars so the dropoff shape is visible at a
	// glance. Bar width is pct of Applied with a 4% floor for nonzero counts.
	var steps []string
	for _, s := range stages {
		n := funnelVal[s[0]]
		pct := 0
		if applied > 0 && n > 0 {
			pct = jsRound(float64(n) / float64(applied) * 100)
			if pct < 4 {
				pct = 4
			}
		}
		steps = append(steps, `
      <div class="an-funnel-col">
        <div class="an-funnel-label">`+s[1]+`</div>
        <div class="an-funnel-num">`+mono(strconv.Itoa(n))+`</div>
        <div class="an-funnel-bar"><span style="width:`+strconv.Itoa(pct)+`%"></span></div>
      </div>`)
	}
	interview := funnel.Interview
	var conv string
	if applied >= 5 {
		conv = `Interview conversion: ` + strconv.Itoa(interview) + ` of ` + strconv.Itoa(applied) + ` (` + strconv.Itoa(jsRound(float64(interview)/float64(applied)*100)) + `%).`
	} else {
		conv = `Interview conversion: ` + strconv.Itoa(interview) + ` of ` + strconv.Itoa(applied) + `.`
	}
	return `
  <div class="report-band">
    <h3 class="report-subhead">Pipeline funnel <span class="report-subhead-note">all-time</span></h3>
    <div class="an-funnel an-funnel-cols">` + strings.Join(steps, "") + `
    </div>
    <p class="analytics-hint" style="margin-top:10px">` + conv + `</p>
  </div>`
}

type rejectionSections struct{ summary string }

func buildRejectionSections(all []db.RejectionInsight) rejectionSections {
	isGood := func(v *float64) bool { return v != nil && *v >= 0 }
	dayCell := func(v *float64, emphasizeFast bool) string {
		if v == nil {
			return `<span style="color:var(--text-muted);font-family:var(--font-mono)">?</span>`
		}
		if *v < 0 {
			return `<span style="color:var(--text-muted);font-family:var(--font-mono)">date mismatch</span>`
		}
		if *v == 0 {
			return `<span style="color:var(--text-muted);font-family:var(--font-mono)">same day</span>`
		}
		color := colSlateLite // COLORS.muted
		if emphasizeFast && *v < 3 {
			color = colRed
		}
		return `<span style="color:` + color + `;font-family:var(--font-mono)">` + fmtJSNum(*v) + `d</span>`
	}
	rowHTML := func(r db.RejectionInsight) string {
		from := "applied"
		if r.RejectedFrom != nil && *r.RejectedFrom != "" {
			from = *r.RejectedFrom
		}
		return `
    <tr>
      <td>` + escapeHTML(r.Company) + `</td>
      <td>` + escapeHTML(r.Title) + `</td>
      <td>` + scorePill(r.Score) + `</td>
      <td>` + from + `</td>
      <td>` + dayCell(r.DaysToReject, true) + `</td>
      <td>` + dayCell(r.PostingAge, false) + `</td>
    </tr>`
	}
	tableHead := `<thead><tr><th>Company</th><th>Role</th><th>Score</th><th>Stage</th><th>Days to Reject</th><th>Posting Age</th></tr></thead>`

	stat := func(get func(db.RejectionInsight) *float64) (avg *string, n, total, bad int) {
		var sum float64
		total = len(all)
		for _, r := range all {
			v := get(r)
			if isGood(v) {
				sum += *v
				n++
			} else if v != nil && *v < 0 {
				bad++
			}
		}
		if n > 0 {
			s := toFixed1(sum / float64(n))
			avg = &s
		}
		return
	}
	dtrAvg, dtrN, dtrTotal, dtrBad := stat(func(r db.RejectionInsight) *float64 { return r.DaysToReject })
	ageAvg, ageN, ageTotal, ageBad := stat(func(r db.RejectionInsight) *float64 { return r.PostingAge })
	var noteParts []string
	if dtrAvg != nil {
		noteParts = append(noteParts, `Avg rejection: `+mono(*dtrAvg)+`d (`+strconv.Itoa(dtrN)+`/`+strconv.Itoa(dtrTotal)+`).`)
	}
	if ageAvg != nil {
		noteParts = append(noteParts, `Avg posting age: `+mono(*ageAvg)+`d (`+strconv.Itoa(ageN)+`/`+strconv.Itoa(ageTotal)+`).`)
	}
	bad := dtrBad
	if ageBad > bad {
		bad = ageBad
	}
	if bad > 0 {
		noteParts = append(noteParts, strconv.Itoa(bad)+` row`+pluralS(bad)+` with inconsistent dates excluded.`)
	}
	note := strings.Join(noteParts, " ")
	if note == "" {
		note = "No rejections with dated history yet."
	}

	var fastest, oldest []db.RejectionInsight
	for _, r := range all {
		if isGood(r.DaysToReject) && *r.DaysToReject > 0 {
			fastest = append(fastest, r)
		}
	}
	stableSortByFloatAsc(fastest, func(r db.RejectionInsight) float64 { return *r.DaysToReject })
	if len(fastest) > 2 {
		fastest = fastest[:2]
	}
	for _, r := range all {
		if isGood(r.PostingAge) && *r.PostingAge > 0 {
			oldest = append(oldest, r)
		}
	}
	stableSortByFloatDesc(oldest, func(r db.RejectionInsight) float64 { return *r.PostingAge })
	if len(oldest) > 2 {
		oldest = oldest[:2]
	}
	seen := map[string]bool{}
	var notable []db.RejectionInsight
	for _, r := range append(append([]db.RejectionInsight{}, fastest...), oldest...) {
		k := r.Company + "|" + r.Title
		if seen[k] {
			continue
		}
		seen[k] = true
		notable = append(notable, r)
		if len(notable) >= 3 {
			break
		}
	}
	notableTable := ""
	if len(notable) > 0 {
		var b strings.Builder
		for _, r := range notable {
			b.WriteString(rowHTML(r))
		}
		notableTable = `<table class="calibration-table report-table">` + tableHead + `<tbody>` + b.String() + `</tbody></table>`
	}

	summary := `
  <div class="report-band">
    <h3 class="report-subhead">Timing summary</h3>
    <p class="analytics-hint">` + note + `</p>
    ` + notableTable + `
  </div>`
	return rejectionSections{summary: summary}
}

// renderAnalytics ports renderAnalytics. dailyCounts is deferred (empty), so the
// applied-pace chart is omitted.
func renderAnalytics(data AnalyticsData) string {
	m := data.Metrics
	rej := buildRejectionSections(data.RejectionInsights)
	return `
<div class="analytics-report">
  ` + renderHealthBand(m.Health) + `
  ` + renderFunnelBand(m.Funnel) + `
  ` + rej.summary + `
</div>`
}

// --- event log ---

var eventLabels = map[string]string{"stage_change": "Pipeline", "outreach": "Outreach", "status_change": "Status", "auto_applied": "Auto Apply"}
var stageDisplay = map[string]string{
	"applied": "Applied", "phone_screen": "Phone Screen", "interview": "Interview", "onsite": "Onsite",
	"offer": "Offer", "rejected": "Rejected", "reached_out": "Reached Out", "archived": "Archived",
	"closed": "Closed", "ghosted": "Ghosted",
}

func activityDate(s string) string {
	if s == "" {
		return ""
	}
	v := strings.TrimSuffix(s, "Z")
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, v, time.UTC); err == nil {
			return t.UTC().Format("Jan 2, 3:04 PM")
		}
	}
	return ""
}

// activityISO normalizes a UTC-stored timestamp into an RFC3339 instant the
// browser can parse unambiguously (so client-side JS can render it in the
// viewer's local timezone instead of UTC).
func activityISO(s string) string {
	if s == "" {
		return ""
	}
	v := strings.TrimSuffix(s, "Z")
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, v, time.UTC); err == nil {
			return t.UTC().Format("2006-01-02T15:04:05Z")
		}
	}
	return ""
}

func renderActivityLog(recentEvents []db.RecentEvent) string {
	stageOrRaw := func(p *string) string {
		if p == nil || *p == "" {
			return "Pending"
		}
		if d, ok := stageDisplay[*p]; ok {
			return d
		}
		return *p
	}
	var rows strings.Builder
	for _, e := range recentEvents {
		from := stageOrRaw(e.FromValue)
		to := stageOrRaw(e.ToValue)
		label := eventLabels[e.EventType]
		if label == "" {
			label = e.EventType
		}
		rows.WriteString(`
      <tr>
        <td class="evt-time" data-ts="` + activityISO(e.CreatedAt) + `" style="color:` + colSlateLite + `;font-size:11px;white-space:nowrap;font-family:var(--font-mono)">` + activityDate(e.CreatedAt) + `</td>
        <td><span style="background:rgba(var(--primary-rgb),0.1);color:var(--primary);padding:2px 6px;border-radius:4px;font-size:11px">` + label + `</span></td>
        <td>` + escapeHTML(e.Company) + `</td>
        <td>` + from + ` &rarr; ` + to + `</td>
      </tr>`)
	}
	eventRows := rows.String()
	if eventRows == "" {
		eventRows = `<tr><td colspan="4" style="text-align:center;color:var(--text-muted)">No events yet</td></tr>`
	}
	tableBody := `<thead><tr><th>Time</th><th>Type</th><th>Company</th><th>Change</th></tr></thead><tbody>` + eventRows + `</tbody>`
	return `
<div class="analytics-wrap">
  <div class="analytics-section market-section">
    <h2 class="analytics-title">Activity Log</h2>
    <p class="analytics-hint"><span style="font-family:var(--font-mono)">` + strconv.Itoa(len(recentEvents)) + `</span> total events</p>
    ` + renderPaginatedTable("events-table", tableBody, len(recentEvents), "&larr; Newer", "Older &rarr;", "pageEvents") + `
  </div>
  <script>
  (function(){
    document.querySelectorAll('#events-table .evt-time[data-ts]').forEach(function(cell){
      var d=new Date(cell.getAttribute('data-ts'));
      if(isNaN(d)){return;}
      cell.textContent=d.toLocaleString('en-US',{month:'short',day:'numeric',hour:'numeric',minute:'2-digit'});
    });
  })();
  </script>
</div>`
}

func renderPaginatedTable(tableID, rowsHTML string, totalRows int, prevLabel, nextLabel, pagerFn string) string {
	pager := ""
	if totalRows > 20 {
		pager = `
    <div style="display:flex;align-items:center;gap:8px;margin-top:12px">
      <button class="btn btn-sm btn-archive" onclick="` + pagerFn + `(-1)" id="` + tableID + `-prev" disabled>` + prevLabel + `</button>
      <span style="font-size:12px;color:var(--text-muted)" id="` + tableID + `-page-info"></span>
      <button class="btn btn-sm btn-archive" onclick="` + pagerFn + `(1)" id="` + tableID + `-next">` + nextLabel + `</button>
    </div>
    <script>
    (function(){
      var PAGE_SIZE=20, page=0;
      var rows=document.querySelectorAll('#` + tableID + ` tbody tr');
      var total=rows.length, pages=Math.ceil(total/PAGE_SIZE);
      function show(){
        rows.forEach(function(r,i){r.style.display=(i>=page*PAGE_SIZE&&i<(page+1)*PAGE_SIZE)?'':'none';});
        document.getElementById('` + tableID + `-prev').disabled=page===0;
        document.getElementById('` + tableID + `-next').disabled=page>=pages-1;
        document.getElementById('` + tableID + `-page-info').textContent='Page '+(page+1)+' of '+pages;
      }
      window.` + pagerFn + `=function(d){page=Math.max(0,Math.min(pages-1,page+d));show();};
      show();
    })();
    </script>`
	}
	return `
    <table class="calibration-table" id="` + tableID + `">
      ` + rowsHTML + `
    </table>
    ` + pager
}
