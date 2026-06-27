package dashboard

import (
	"net/http"
	"strconv"
	"time"
)

// This file ports buildAnalyticsAudit (lib/analytics-audit.js) and serves it at
// GET /api/analytics/audit. Verified by JSON parity against Node.

type analyticsAuditDefinitions struct {
	AppliedCohort string `json:"appliedCohort"`
	Reached       string `json:"reached"`
	Rejected      string `json:"rejected"`
	Responded     string `json:"responded"`
	Stale         string `json:"stale"`
}

// AnalyticsAudit is the reshaped metrics + reconciliation warnings + definitions.
// It omits last7 and adds definitions/warnings (matching buildAnalyticsAudit).
type AnalyticsAudit struct {
	AsOf             int64                     `json:"asOf"`
	StaleDays        int                       `json:"staleDays"`
	Thresholds       Thresholds                `json:"thresholds"`
	Health           Health                    `json:"health"`
	Funnel           Funnel                    `json:"funnel"`
	ScoreCalibration []ScoreCalRow             `json:"scoreCalibration"`
	ATS              []ATSRow                  `json:"ats"`
	Activity         Activity                  `json:"activity"`
	Actions          Actions                   `json:"actions"`
	Contributors     Contributors              `json:"contributors"`
	Reconciliation   Reconciliation            `json:"reconciliation"`
	Definitions      analyticsAuditDefinitions `json:"definitions"`
	Warnings         []string                  `json:"warnings"`
}

func buildAnalyticsAudit(m AnalyticsMetrics) AnalyticsAudit {
	warnings := []string{}

	funnelByStage := map[string]int{
		"phone_screen": m.Funnel.PhoneScreen, "interview": m.Funnel.Interview,
		"onsite": m.Funnel.Onsite, "offer": m.Funnel.Offer,
	}
	for _, stage := range []string{"phone_screen", "interview", "onsite", "offer"} {
		if funnelByStage[stage] > m.Funnel.Applied {
			warnings = append(warnings, "Funnel "+stage+" ("+strconv.Itoa(funnelByStage[stage])+") exceeds applied ("+strconv.Itoa(m.Funnel.Applied)+"); reached-set is broken.")
		}
	}

	advancedCount := len(m.Contributors.Advanced)
	floor := advancedCount
	if m.Health.Rejected > floor {
		floor = m.Health.Rejected
	}
	if m.Health.Responded < floor {
		warnings = append(warnings, "Responded ("+strconv.Itoa(m.Health.Responded)+") is below its own parts (advanced "+strconv.Itoa(advancedCount)+", rejected "+strconv.Itoa(m.Health.Rejected)+").")
	}

	lowConf := 0
	for _, r := range m.ScoreCalibration {
		if r.LowConfidence {
			lowConf++
		}
	}
	if lowConf > 0 {
		warnings = append(warnings, strconv.Itoa(lowConf)+" score row(s) have < "+strconv.Itoa(m.Thresholds.MinCalibrationSample)+" applications and are flagged low-confidence.")
	}

	thinAts := 0
	for _, a := range m.ATS {
		if a.LowSample {
			thinAts++
		}
	}
	if thinAts > 0 {
		warnings = append(warnings, strconv.Itoa(thinAts)+" ATS bucket(s) have < "+strconv.Itoa(m.Thresholds.MinAtsSample)+" applications and are suppressed in the page.")
	}
	if m.Reconciliation.AppliedEventOnly > 0 {
		warnings = append(warnings, strconv.Itoa(m.Reconciliation.AppliedEventOnly)+" application(s) are recovered from applied events because applied_at is missing.")
	}
	if m.Reconciliation.AdvancedWithoutAppliedMarker > 0 {
		warnings = append(warnings, strconv.Itoa(m.Reconciliation.AdvancedWithoutAppliedMarker)+" advanced application(s) are recovered from reached-stage history without an applied marker.")
	}
	if m.Reconciliation.PendingWithAppliedAt > 0 {
		warnings = append(warnings, strconv.Itoa(m.Reconciliation.PendingWithAppliedAt)+" row(s) have applied_at but current status is pending; counted as historical applications.")
	}

	return AnalyticsAudit{
		AsOf:             m.AsOf,
		StaleDays:        m.StaleDays,
		Thresholds:       m.Thresholds,
		Health:           m.Health,
		Funnel:           m.Funnel,
		ScoreCalibration: m.ScoreCalibration,
		ATS:              m.ATS,
		Activity:         m.Activity,
		Actions:          m.Actions,
		Contributors:     m.Contributors,
		Reconciliation:   m.Reconciliation,
		Definitions: analyticsAuditDefinitions{
			AppliedCohort: "historical application evidence: applied_at, applied event, or reached-stage history",
			Reached:       "ever reached the stage (stage_change to/from stage, current stage, or rejected_from_stage)",
			Rejected:      "stage_change event to rejected, or current stage = rejected, within the applied cohort",
			Responded:     "reached phone_screen or beyond, OR ever rejected (any company reply)",
			Stale:         "applied >= " + strconv.Itoa(m.StaleDays) + " days ago, status still applied, no forward stage, no reply",
		},
		Warnings: warnings,
	}
}

// handleAnalyticsAudit serves GET /api/analytics/audit.
func (s *Server) handleAnalyticsAudit(w http.ResponseWriter, _ *http.Request) {
	metrics, err := computeAnalyticsMetrics(s.repo, time.Now().UnixMilli())
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, err := marshalNoHTMLEscape(buildAnalyticsAudit(metrics))
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}
