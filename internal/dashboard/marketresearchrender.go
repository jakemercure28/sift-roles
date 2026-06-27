package dashboard

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"job-search-automation/internal/db"
)

type marketLevelMeta struct {
	Label      string
	YearsLabel string
}

var marketLevelMetaByID = map[string]marketLevelMeta{
	"junior": {Label: "Junior / Entry", YearsLabel: "0–2 yrs"},
	"mid":    {Label: "Mid-Level", YearsLabel: "3–4 yrs"},
	"senior": {Label: "Senior", YearsLabel: "5–7 yrs"},
	"staff":  {Label: "Staff / Lead+", YearsLabel: "8+ yrs"},
}

var vagueMarketSkills = map[string]bool{
	"data": true, "ai": true, "a.i.": true, "ml": true, "ai/ml": true,
	"machine learning": true, "artificial intelligence": true, "cloud": true,
	"platform": true, "infrastructure": true, "infra": true, "security": true,
}

func isVagueMarketSkill(name string) bool {
	return vagueMarketSkills[strings.ToLower(strings.TrimSpace(name))]
}

// remainderSkewsSenior takes seniorities already classified by the caller so it
// does not re-run the description regexes.
func remainderSkewsSenior(seniorities []Seniority, applicantYoe *int) bool {
	if applicantYoe == nil || len(seniorities) == 0 {
		return false
	}
	inaccessible := 0
	seniorish := 0
	for _, s := range seniorities {
		if isAccessibleFor(s, *applicantYoe) {
			continue
		}
		inaccessible++
		if (s.Years != nil && *s.Years >= 5) || s.Level == "senior" || s.Level == "staff" {
			seniorish++
		}
	}
	if inaccessible == 0 {
		return false
	}
	return float64(seniorish)/float64(inaccessible) >= 0.6
}

func renderSeniorityBreakdown(allJobs []db.MarketSeniorityJob, applicantYoe *int, id string, hidden bool, title, countLabel, sourceLabel string) string {
	style := ""
	if hidden {
		style = "display:none"
	}
	if len(allJobs) == 0 {
		return fmt.Sprintf(`
  <div id="market-view-%s" class="market-view-panel" style="%s">
    <div class="analytics-section market-section">
      <h3 class="report-subhead">%s</h3>
      <p class="analytics-hint">No roles available for this market view.</p>
    </div>
  </div>`, escapeHTML(id), style, escapeHTML(title))
	}

	// Classify each job once and reuse the result for bucketing, accessibility,
	// the senior-skew check, and the years histogram below. Re-deriving it per
	// section meant regex-parsing every description 3-4x per page load.
	seniorities := make([]Seniority, len(allJobs))
	for i, j := range allJobs {
		seniorities[i] = classifySeniority(j.Title, j.Description)
	}

	buckets := map[string][]db.MarketSeniorityJob{"junior": {}, "mid": {}, "senior": {}, "staff": {}}
	jdSourceCount := 0
	for i, j := range allJobs {
		s := seniorities[i]
		buckets[s.Level] = append(buckets[s.Level], j)
		if s.Source == "jd" {
			jdSourceCount++
		}
	}
	total := len(allJobs)
	maxCount := 1
	for _, b := range buckets {
		if len(b) > maxCount {
			maxCount = len(b)
		}
	}
	levels := []string{"junior", "mid", "senior", "staff"}
	applicantLevel := ""
	if applicantYoe != nil {
		applicantLevel = levelFromYears(*applicantYoe)
	}

	var barRows strings.Builder
	for _, level := range levels {
		meta := marketLevelMetaByID[level]
		count := len(buckets[level])
		pct := jsRound(float64(count) / float64(total) * 100)
		barPct := jsRound(float64(count) / float64(maxCount) * 100)
		isApplicant := applicantLevel != "" && level == applicantLevel
		marker := ""
		if isApplicant {
			marker = `<span style="color:var(--accent);font-size:11px;margin-left:6px;font-weight:700">◀ you</span>`
		}
		barColor := "var(--text-muted)"
		shadow := ""
		if isApplicant {
			barColor = "var(--accent)"
			shadow = ";box-shadow:0 0 0 1px var(--accent)"
		}
		inside := ""
		if barPct > 20 {
			inside = fmt.Sprintf(`<span style="font-size:11px;color:var(--inverse);font-weight:600;font-family:var(--font-mono)">%d</span>`, count)
		}
		outside := ""
		if barPct <= 20 {
			outside = fmt.Sprintf(`<span style="position:absolute;left:%d%%;top:50%%;transform:translateY(-50%%);font-size:11px;color:var(--text-primary);font-weight:600;font-family:var(--font-mono)">%d</span>`, barPct+2, count)
		}
		barRows.WriteString(fmt.Sprintf(`
    <div style="display:grid;grid-template-columns:140px 1fr 110px;align-items:center;gap:12px;margin-bottom:10px">
      <div style="font-size:13px;color:var(--text-primary);text-align:right;white-space:nowrap">
        %s<br><span style="font-size:11px;color:var(--text-muted)">%s</span>
      </div>
      <div style="height:28px;background:rgba(var(--tint-rgb),0.06);border-radius:4px;overflow:hidden;position:relative%s">
        <div style="height:100%%;width:%d%%;background:%s;border-radius:4px;min-width:4px;display:flex;align-items:center;padding-left:8px">
          %s
        </div>
        %s
      </div>
      <div style="font-size:13px;color:var(--text-primary);font-weight:600;font-family:var(--font-mono)">%d%%%s</div>
    </div>`, meta.Label, meta.YearsLabel, shadow, barPct, barColor, inside, outside, pct, marker))
	}

	accessible := []db.MarketSeniorityJob{}
	if applicantYoe != nil {
		for i, j := range allJobs {
			if isAccessibleFor(seniorities[i], *applicantYoe) {
				accessible = append(accessible, j)
			}
		}
	}
	accessiblePct := 0
	if applicantYoe != nil && total > 0 {
		accessiblePct = jsRound(float64(len(accessible)) / float64(total) * 100)
	}
	remainderSenior := applicantYoe != nil && remainderSkewsSenior(seniorities, applicantYoe)

	yearBuckets := map[int]int{}
	for _, s := range seniorities {
		if s.Years != nil {
			yearBuckets[*s.Years]++
		}
	}
	years := make([]int, 0, len(yearBuckets))
	for y := range yearBuckets {
		years = append(years, y)
	}
	sort.Ints(years)
	maxYearCount := 1
	for _, y := range years {
		if yearBuckets[y] > maxYearCount {
			maxYearCount = yearBuckets[y]
		}
	}
	var yearRows strings.Builder
	for _, yr := range years {
		count := yearBuckets[yr]
		barPct := jsRound(float64(count) / float64(maxYearCount) * 100)
		level := "staff"
		switch {
		case yr <= 2:
			level = "junior"
		case yr <= 4:
			level = "mid"
		case yr <= 7:
			level = "senior"
		}
		isMatch := applicantLevel != "" && level == applicantLevel
		color := "var(--text-secondary)"
		weight := "400"
		barColor := "var(--text-muted)"
		if isMatch {
			color = "var(--accent)"
			weight = "700"
			barColor = "var(--accent)"
		}
		yearRows.WriteString(fmt.Sprintf(`
    <div style="display:grid;grid-template-columns:60px 1fr 40px;align-items:center;gap:8px;margin-bottom:4px">
      <div style="font-size:12px;color:%s;text-align:right;font-weight:%s;font-family:var(--font-mono)">%d+ yrs</div>
      <div style="height:18px;background:rgba(var(--tint-rgb),0.06);border-radius:3px;overflow:hidden">
        <div style="height:100%%;width:%d%%;background:%s;border-radius:3px;min-width:3px"></div>
      </div>
      <div style="font-size:11px;color:var(--text-primary);font-family:var(--font-mono)">%d</div>
    </div>`, color, weight, yr, barPct, barColor, count))
	}

	experienceCard := ""
	if len(years) > 0 {
		experienceCard = fmt.Sprintf(`
  <div class="analytics-section market-section">
    <h3 class="report-subhead">Experience requirements</h3>
    <p class="analytics-hint">%d %s with stated years.</p>
    <div style="margin-top:10px">
      %s
    </div>
  </div>`, jdSourceCount, escapeHTML(sourceLabel), yearRows.String())
	}

	conclusion := ""
	if applicantYoe != nil {
		extra := ""
		if remainderSenior {
			extra = " Most of the rest ask 5+ years."
		}
		conclusion = fmt.Sprintf(`<p class="report-callout">%d/%d realistic at %d YOE (%d%%).%s</p>`, len(accessible), total, *applicantYoe, accessiblePct, extra)
	}
	gridClass := ""
	if len(years) > 0 {
		gridClass = "market-research-grid"
	}
	return fmt.Sprintf(`
  <div id="market-view-%s" class="market-view-panel" style="%s">
  <div class="%s">
    <div class="analytics-section market-section">
      <h3 class="report-subhead">%s</h3>
      <p class="analytics-hint">%d %s by seniority.</p>
      %s
      <div style="margin-top:16px">
        %s
      </div>
    </div>
    %s
  </div>
  </div>`, escapeHTML(id), style, gridClass, escapeHTML(title), total, escapeHTML(countLabel), conclusion, barRows.String(), experienceCard)
}

func renderMarketViewTabs() string {
	return `
  <div class="market-scope-tabs">
    <h2 class="analytics-title" style="margin:0">Market fit</h2>
    <div class="tracker-period-btns" role="tablist" aria-label="Market view">
      <button type="button" class="tracker-period-btn active" id="market-tab-current" data-market-view="current" onclick="setMarketView('current')" role="tab" aria-selected="true">Current Market</button>
      <button type="button" class="tracker-period-btn" id="market-tab-all-time" data-market-view="all-time" onclick="setMarketView('all-time')" role="tab" aria-selected="false">All Time</button>
    </div>
  </div>
  <script>
  function setMarketView(view) {
    var ids = ['current', 'all-time'];
    ids.forEach(function(id) {
      var panel = document.getElementById('market-view-' + id);
      var tab = document.getElementById('market-tab-' + id);
      var active = id === view;
      if (panel) panel.style.display = active ? '' : 'none';
      if (tab) {
        tab.classList.toggle('active', active);
        tab.setAttribute('aria-selected', active ? 'true' : 'false');
      }
    });
  }
  </script>`
}

func renderNewRolesSection() string {
	return `
<div class="analytics-section market-section tracker-section">
  <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:10px;gap:12px;flex-wrap:wrap">
    <h2 class="analytics-title" style="margin-bottom:0">Posting trend</h2>
    <div class="tracker-period-btns">
      <button class="tracker-period-btn" data-period="12w">12W</button>
      <button class="tracker-period-btn active" data-period="26w">26W</button>
      <button class="tracker-period-btn" data-period="52w">52W</button>
      <button class="tracker-period-btn" data-period="all">All</button>
    </div>
  </div>
  <p class="analytics-hint" style="margin-bottom:16px">New roles per week.</p>
  <div class="tracker-chart-wrap">
    <canvas id="newRolesChart"></canvas>
    <div id="newRolesEmpty" class="tracker-unavailable" style="display:none">Trend unavailable until more dated roles are collected.</div>
  </div>
</div>
<script>
(function() {
  var newRolesChart = null;
  var currentPeriod = '26w';

  function loadNewRoles(period) {
    window.loadDashboardChart(function() {
      fetch('/api/market-activity?period=' + period)
        .then(function(r){return r.json();})
        .then(function(rows) {
        var empty = document.getElementById('newRolesEmpty');
        var canvas = document.getElementById('newRolesChart');
        var wrap = canvas.parentElement;
        var sum = (rows || []).reduce(function(a, r){ return a + (r.count || 0); }, 0);
        if (!rows || rows.length === 0 || sum === 0) {
          canvas.style.display = 'none';
          empty.style.display = 'block';
          wrap.classList.add('tracker-chart-collapsed');
          return;
        }
        canvas.style.display = 'block';
        empty.style.display = 'none';
        wrap.classList.remove('tracker-chart-collapsed');
        var labels = rows.map(function(r){return r.label;});
        var data = rows.map(function(r){return r.count;});
        var dataset = {
          label: 'New roles',
          data: data,
          backgroundColor: 'rgba(120, 120, 120, 0.45)',
          borderColor: 'rgba(120, 120, 120, 0.7)',
          borderWidth: 1,
          borderRadius: 2,
          maxBarThickness: 26,
        };
        if (newRolesChart) {
          newRolesChart.data.labels = labels;
          newRolesChart.data.datasets = [dataset];
          newRolesChart.update();
        } else {
          var ctx = canvas.getContext('2d');
          newRolesChart = new Chart(ctx, {
            type: 'bar',
            data: { labels: labels, datasets: [dataset] },
            options: {
              responsive: true,
              maintainAspectRatio: false,
              plugins: {
                legend: { display: false },
                tooltip: {
                  backgroundColor: 'rgba(20,20,20,0.95)',
                  borderColor: 'rgba(128,128,128,0.25)',
                  borderWidth: 1,
                  titleColor: '#f0f0f0',
                  bodyColor: '#cccccc',
                  callbacks: {
                    title: function(items){return 'Week of ' + items[0].label;},
                    label: function(item){return item.parsed.y + ' new role' + (item.parsed.y === 1 ? '' : 's');}
                  }
                }
              },
              scales: {
                x: { ticks: { color: '#8A8A8A', maxTicksLimit: 12, maxRotation: 0 }, grid: { display: false } },
                y: { beginAtZero: true, ticks: { color: '#8A8A8A', precision: 0 }, grid: { color: 'rgba(128,128,128,0.12)' } }
              }
            }
          });
        }
      })
      .catch(function(){});
    });
  }

  document.querySelectorAll('.tracker-period-btn[data-period]').forEach(function(btn) {
    btn.addEventListener('click', function() {
      document.querySelectorAll('.tracker-period-btn[data-period]').forEach(function(b){b.classList.remove('active');});
      btn.classList.add('active');
      currentPeriod = btn.dataset.period;
      loadNewRoles(currentPeriod);
    });
  });

  loadNewRoles(currentPeriod);
})();
</script>
`
}

func renderAnalysisErrorBanner(msg string) string {
	if msg == "" {
		return ""
	}
	return fmt.Sprintf(`
<div class="market-error-banner">
  <p style="margin:0;color:var(--amber);font-size:14px;line-height:1.6">⚠ %s</p>
</div>`, escapeHTML(msg))
}

func marketDateShort(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).In(time.Local).Format("Jan 2")
}

// renderReportMasthead is the one-line page status for Market Research: counts,
// cache state, and the actions. The long what-this-does prose lives behind the
// info icon so repeat visits aren't paying for onboarding text.
func renderReportMasthead(currentCount, allTimeCount int, cache *marketCache, hasFreshCache, stale, sampleChanged bool, jobCount int) string {
	segs := []string{
		`<span class="report-meta-num">` + itoa(currentCount) + `</span> current roles`,
		`<span class="report-meta-num">` + itoa(allTimeCount) + `</span> all-time`,
	}
	// Cache state keys off GeneratedAt, not freshness: a stale or outgrown cache
	// was still analyzed at some point, so never claim "not yet analyzed".
	switch {
	case cache != nil && cache.GeneratedAt > 0:
		if hasFreshCache {
			n := cache.Data.SampleSize
			if n == 0 && cache.JobCount != nil {
				n = *cache.JobCount
			}
			if n > 0 {
				segs = append(segs, "analyzed "+itoa(n)+" live JDs")
			}
			segs = append(segs, "updated "+marketDateShort(cache.GeneratedAt))
		} else if sampleChanged && cache.JobCount != nil {
			segs = append(segs, "updated "+marketDateShort(cache.GeneratedAt),
				fmt.Sprintf("sample changed (%d &rarr; %d JDs), re-run recommended", *cache.JobCount, jobCount))
		} else if stale {
			segs = append(segs, "updated "+marketDateShort(cache.GeneratedAt), "stale, re-run recommended")
		} else {
			segs = append(segs, "updated "+marketDateShort(cache.GeneratedAt))
		}
	default:
		segs = append(segs, "not yet analyzed")
	}
	runLabel := "Run analysis"
	if cache != nil && cache.GeneratedAt > 0 {
		runLabel = "Re-run analysis"
	}
	return fmt.Sprintf(`
  <div class="report-masthead">
    <p class="report-meta">%s</p>
    <div class="report-masthead-actions">
      <form method="POST" action="/market-research" class="report-rerun-form" onsubmit="return mrRunSubmit(this)">
        <button type="submit" class="btn btn-sm btn-archive">%s</button>
      </form>
    </div>
  </div>
  `, strings.Join(segs, " &middot; "), runLabel)
}

type marketRenderContext struct {
	CurrentCount  int
	ApplicantYoe  *int
	Accessible    int
	AccessiblePct int
	RemotePct     int
}

func filterMarketGaps(gaps []marketGap) []marketGap {
	out := []marketGap{}
	for _, g := range gaps {
		if !isVagueMarketSkill(g.Skill) {
			out = append(out, g)
		}
	}
	return out
}

func renderKeyMetrics(data *marketAnalysisData, ctx marketRenderContext) string {
	type metric struct {
		Num, Label, Sub string
		Wide            bool
	}
	metrics := []metric{{Num: fmt.Sprintf("%d", ctx.CurrentCount), Label: "Current roles"}}
	if ctx.ApplicantYoe != nil {
		metrics = append(metrics, metric{Num: fmt.Sprintf("%d%%", ctx.AccessiblePct), Label: fmt.Sprintf("Accessible at %d YOE", *ctx.ApplicantYoe), Sub: fmt.Sprintf("%d of %d roles", ctx.Accessible, ctx.CurrentCount)})
	}
	if ctx.CurrentCount > 0 {
		metrics = append(metrics, metric{Num: fmt.Sprintf("%d%%", ctx.RemotePct), Label: "Remote share"})
	}
	if data != nil && len(data.TopSkills) > 0 {
		metrics = append(metrics, metric{Num: escapeHTML(data.TopSkills[0].Skill), Label: "Top required skill", Wide: true})
	}
	var cells strings.Builder
	for _, m := range metrics {
		wide := ""
		if m.Wide {
			wide = " stat-item-wide"
		}
		sub := ""
		if m.Sub != "" {
			sub = `<div class="stat-sub">` + escapeHTML(m.Sub) + `</div>`
		}
		cells.WriteString(fmt.Sprintf(`
    <div class="stat-item%s">
      <div class="stat-num">%s</div>
      <div class="stat-label">%s</div>
      %s
    </div>`, wide, m.Num, escapeHTML(m.Label), sub))
	}
	return `<div class="stat-strip">` + cells.String() + `</div>`
}

func renderSkillsSection(data marketAnalysisData) string {
	gaps := filterMarketGaps(append([]marketGap{}, data.GapAnalysis...))
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].Pct > gaps[j].Pct })
	var gapRows strings.Builder
	for _, s := range gaps {
		plural := "s"
		if s.Count == 1 {
			plural = ""
		}
		note := fmt.Sprintf("%d", s.Count)
		if data.SampleSize > 0 {
			note += fmt.Sprintf(" of %d", data.SampleSize)
		}
		note += fmt.Sprintf(" JD%s; weak on resume.", plural)
		gapRows.WriteString(fmt.Sprintf(`
    <tr>
      <td class="gap-skill">%s</td>
      <td class="num">%d%%</td>
      <td class="note">%s</td>
    </tr>`, escapeHTML(s.Skill), s.Pct, note))
	}
	var strengthRows strings.Builder
	for _, s := range data.ResumeStrengths {
		strengthRows.WriteString(fmt.Sprintf(`
    <tr>
      <td class="strength-skill">%s</td>
      <td class="num">%d</td>
    </tr>`, escapeHTML(s.Skill), s.Count))
	}
	strongSet := map[string]bool{}
	for _, s := range data.ResumeStrengths {
		strongSet[strings.ToLower(s.Skill)] = true
	}
	gapSet := map[string]bool{}
	for _, s := range data.GapAnalysis {
		gapSet[strings.ToLower(s.Skill)] = true
	}
	maxCount := 1
	if len(data.TopSkills) > 0 && data.TopSkills[0].Count > 0 {
		maxCount = data.TopSkills[0].Count
	}
	skillRow := func(s marketSkill) string {
		pct := jsRound(float64(s.Count) / float64(maxCount) * 100)
		key := strings.ToLower(s.Skill)
		tag := ""
		if gapSet[key] {
			tag = `<span class="skill-tag skill-tag-gap">gap</span>`
		} else if strongSet[key] {
			tag = `<span class="skill-tag skill-tag-strong">strong</span>`
		}
		return fmt.Sprintf(`
    <div class="skill-bar-row">
      <div class="skill-bar-label" title="%s"><span class="skill-name">%s</span>%s</div>
      <div class="skill-bar-track"><div class="skill-bar-fill" style="width:%d%%"></div></div>
      <div class="skill-bar-count">%d <span class="skill-bar-pct">(%d%%)</span></div>
    </div>`, escapeHTML(s.Skill), escapeHTML(s.Skill), tag, pct, s.Count, s.Pct)
	}
	var head strings.Builder
	for _, s := range data.TopSkills[:min(len(data.TopSkills), 10)] {
		head.WriteString(skillRow(s))
	}
	moreBlock := ""
	if len(data.TopSkills) > 10 {
		var rest strings.Builder
		for _, s := range data.TopSkills[10:] {
			rest.WriteString(skillRow(s))
		}
		moreBlock = fmt.Sprintf(`<details class="skills-more"><summary>Show all %d skills</summary><div class="skills-bar-list">%s</div></details>`, len(data.TopSkills), rest.String())
	}
	gapBody := gapRows.String()
	if gapBody == "" {
		gapBody = `<tr><td colspan="3" class="report-empty-cell">No significant gaps identified</td></tr>`
	}
	strengthBody := strengthRows.String()
	if strengthBody == "" {
		strengthBody = `<tr><td colspan="2" class="report-empty-cell">No data</td></tr>`
	}
	headBody := head.String()
	if headBody == "" {
		headBody = `<div class="report-empty">No data</div>`
	}
	return fmt.Sprintf(`
  <h2 class="analytics-title">Skills</h2>
  <p class="analytics-hint">JD demand and resume gaps.</p>

  <div class="skills-two-col">
    <div class="skills-col">
      <h3 class="report-subhead">Gaps</h3>
      <table class="calibration-table report-table">
        <thead><tr><th>Skill</th><th>%% of JDs</th><th>Note</th></tr></thead>
        <tbody>%s</tbody>
      </table>
    </div>
    <div class="skills-col">
      <h3 class="report-subhead">Strengths</h3>
      <table class="calibration-table report-table">
        <thead><tr><th>Skill</th><th>JD Count</th></tr></thead>
        <tbody>%s</tbody>
      </table>
    </div>
  </div>

  <div class="skills-demanded">
    <h3 class="report-subhead">Top skills</h3>
    <div class="skills-bar-list">%s</div>
    %s
  </div>`, gapBody, strengthBody, headBody, moreBlock)
}

func renderLocationBreakdown(lb marketLocationBreakdown, total int) string {
	cats := []struct {
		Key, Label string
		Count      int
	}{
		{"remote", "Remote", lb.Remote},
		{"hybrid", "Hybrid", lb.Hybrid},
		{"in_person", "In-Person", lb.InPerson},
		{"not_specified", "Not Specified", lb.NotSpecified},
	}
	maxVal := 1
	for _, c := range cats {
		if c.Count > maxVal {
			maxVal = c.Count
		}
	}
	var bars strings.Builder
	for _, c := range cats {
		pct, barPct := 0, 0
		if total > 0 {
			pct = jsRound(float64(c.Count) / float64(total) * 100)
		}
		barPct = jsRound(float64(c.Count) / float64(maxVal) * 100)
		minWidth := 0
		if c.Count > 0 {
			minWidth = 4
		}
		bars.WriteString(fmt.Sprintf(`
    <div style="display:grid;grid-template-columns:120px 1fr 100px;align-items:center;gap:12px;margin-bottom:8px">
      <div style="font-size:13px;color:var(--text-primary);text-align:right">%s</div>
      <div style="height:22px;background:rgba(var(--tint-rgb),0.06);border-radius:4px;overflow:hidden">
        <div style="height:100%%;width:%d%%;background:var(--text-muted);border-radius:4px;min-width:%dpx"></div>
      </div>
      <div style="font-size:12px;color:var(--text-primary);font-weight:600;font-family:var(--font-mono)">%d <span style="font-weight:400;color:var(--text-muted)">(%d%%)</span></div>
    </div>`, c.Label, barPct, minWidth, c.Count, pct))
	}
	metros := lb.TopMetros
	if len(metros) > 12 {
		metros = metros[:12]
	}
	maxMetroCount := 1
	for _, m := range metros {
		if m.Count > maxMetroCount {
			maxMetroCount = m.Count
		}
	}
	var metroBars strings.Builder
	for _, m := range metros {
		barPct := jsRound(float64(m.Count) / float64(maxMetroCount) * 100)
		metroPct := 0
		if total > 0 {
			metroPct = jsRound(float64(m.Count) / float64(total) * 100)
		}
		minWidth := 0
		if m.Count > 0 {
			minWidth = 4
		}
		metroBars.WriteString(fmt.Sprintf(`
    <div style="display:grid;grid-template-columns:150px 1fr 90px;align-items:center;gap:12px;margin-bottom:8px">
      <div style="font-size:13px;color:var(--text-primary);text-align:right;white-space:nowrap;overflow:hidden;text-overflow:ellipsis" title="%s">%s</div>
      <div style="height:20px;background:rgba(var(--tint-rgb),0.06);border-radius:4px;overflow:hidden">
        <div style="height:100%%;width:%d%%;background:var(--text-muted);border-radius:4px;min-width:%dpx"></div>
      </div>
      <div style="font-size:12px;color:var(--text-primary);font-weight:600;font-family:var(--font-mono)">%d <span style="font-weight:400;color:var(--text-muted)">(%d%%)</span></div>
    </div>`, escapeHTML(m.Metro), escapeHTML(m.Metro), barPct, minWidth, m.Count, metroPct))
	}
	otherLine := ""
	if lb.OtherLocatedCount > 0 {
		otherLine = fmt.Sprintf(`<div class="report-subhead-note" style="margin-top:8px">+ %d in other / unlisted metros</div>`, lb.OtherLocatedCount)
	}
	remotePct, inPersonPct := 0, 0
	if total > 0 {
		remotePct = jsRound(float64(lb.Remote) / float64(total) * 100)
		inPersonPct = jsRound(float64(lb.InPerson) / float64(total) * 100)
	}
	metroPhrase := ""
	if len(metros) > 0 {
		metroPhrase = " " + escapeHTML(metros[0].Metro) + " leads."
	}
	note := fmt.Sprintf("Remote %d%%; in-person %d%%.%s", remotePct, inPersonPct, metroPhrase)
	citiesSection := ""
	if len(metros) > 0 {
		citiesSection = fmt.Sprintf(`
    <div class="location-two-col-side">
      <h3 class="report-subhead">Top metros <span class="report-subhead-note">grouped by standard metro</span></h3>
      <div>%s</div>
      %s
    </div>`, metroBars.String(), otherLine)
	}
	return fmt.Sprintf(`
  <h2 class="analytics-title">Location</h2>
  <p class="analytics-hint"><span style="font-family:var(--font-mono)">%d</span> current roles by location.</p>
  <p class="location-note">%s</p>
  <div class="location-two-col">
    <div>%s</div>
    %s
  </div>`, total, note, bars.String(), citiesSection)
}

func reportBand(cls, inner string) string {
	if inner == "" {
		return ""
	}
	class := "report-band"
	if cls != "" {
		class += " " + cls
	}
	return `<section class="` + class + `">` + inner + `</section>`
}

func renderMarketResearch(page marketResearchPageData) string {
	currentJobs := page.Current.Jobs
	if len(currentJobs) == 0 {
		currentJobs = page.AllJobs
	}
	allTimeJobs := page.AllTime.Jobs
	if len(allTimeJobs) == 0 {
		allTimeJobs = currentJobs
	}
	currentCount := len(currentJobs)
	allTimeCount := page.AllTime.JobCount
	if allTimeCount == 0 {
		allTimeCount = len(allTimeJobs)
	}

	stale := false
	sampleChanged := false
	if page.Cache != nil {
		stale = time.Now().UnixMilli()-page.Cache.GeneratedAt > 48*3600*1000
		sampleChanged = page.Cache.JobCount != nil && *page.Cache.JobCount != page.JobCount
	}
	hasFreshCache := page.Cache != nil && !stale && !sampleChanged
	var data *marketAnalysisData
	if hasFreshCache {
		data = &page.Cache.Data
	}
	accessible := 0
	if page.ApplicantYoe != nil {
		for _, j := range currentJobs {
			if isAccessible(j.Title, j.Description, *page.ApplicantYoe) {
				accessible++
			}
		}
	}
	accessiblePct := 0
	if page.ApplicantYoe != nil && currentCount > 0 {
		accessiblePct = jsRound(float64(accessible) / float64(currentCount) * 100)
	}
	loc := computeMarketLocationBreakdown(currentJobs)
	ctx := marketRenderContext{
		CurrentCount: currentCount, ApplicantYoe: page.ApplicantYoe,
		Accessible: accessible, AccessiblePct: accessiblePct, RemotePct: loc.RemotePct,
	}
	bannerHTML := renderAnalysisErrorBanner(page.AnalysisError)
	masthead := renderReportMasthead(currentCount, allTimeCount, page.Cache, hasFreshCache, stale, sampleChanged, page.JobCount)
	fitHTML := renderMarketViewTabs() +
		renderSeniorityBreakdown(currentJobs, page.ApplicantYoe, "current", false, "Current market — seniority", "live open roles", "current JDs") +
		renderSeniorityBreakdown(allTimeJobs, page.ApplicantYoe, "all-time", true, "All-time — seniority", "all-time seen roles", "all-time JDs")

	keyMetricsData := data
	skills := ""
	if data != nil {
		skills = renderSkillsSection(*data)
	}
	location := ""
	if currentCount > 0 {
		location = renderLocationBreakdown(loc, loc.Total)
	}
	return fmt.Sprintf(`
<div class="market-report">
  %s
  %s
  %s
  %s
  %s
  %s
  %s
</div>`, bannerHTML, reportBand("report-band-intro", masthead), reportBand("", renderKeyMetrics(keyMetricsData, ctx)), reportBand("", fitHTML), reportBand("", skills), reportBand("", renderNewRolesSection()), reportBand("", location))
}
