package opsagent

import (
	"strings"
	"testing"
	"time"
)

func testGeminiClient(budget int) *geminiClient {
	return &geminiClient{
		budget:  budget,
		logger:  discardLogger(),
		tickets: time.Now,
	}
}

func TestBudgetStopsCallsOnceSpent(t *testing.T) {
	// The point of a local budget is that exhaustion is a decision we log, not
	// a 429 we discover at 3am. Verify it actually stops claiming.
	g := testGeminiClient(2)
	for i := 0; i < 2; i++ {
		if !g.claim() {
			t.Fatalf("claim %d should have been allowed within a budget of 2", i+1)
		}
	}
	if g.claim() {
		t.Error("the third claim should be refused")
	}
	spent, budget := g.spentToday()
	if spent != 2 || budget != 2 {
		t.Errorf("spentToday() = %d/%d, want 2/2", spent, budget)
	}
}

func TestBudgetResetsOnANewDay(t *testing.T) {
	g := testGeminiClient(1)
	now := time.Date(2026, 7, 30, 23, 0, 0, 0, time.UTC)
	g.tickets = func() time.Time { return now }

	if !g.claim() {
		t.Fatal("first claim of the day should be allowed")
	}
	if g.claim() {
		t.Fatal("budget of 1 should refuse the second claim same-day")
	}

	// Advance past the Pacific day boundary.
	now = now.Add(30 * time.Hour)
	if !g.claim() {
		t.Error("a new day should reset the budget")
	}
}

func TestDiagnoseReportsBudgetExhaustionDistinctly(t *testing.T) {
	// The caller branches on this: budget exhaustion still escalates the
	// evidence, so it must be distinguishable from a transport failure.
	g := testGeminiClient(0)
	_, _, err := g.diagnose(t.Context(), "system", "evidence")
	if err != errBudgetExhausted {
		t.Errorf("err = %v, want errBudgetExhausted", err)
	}
}

func TestIsQuotaErrorOnlyMatches429(t *testing.T) {
	if !isQuotaError(&geminiAPIError{Code: 429}) {
		t.Error("429 should be treated as a quota error and trigger the fallback model")
	}
	for _, code := range []int{400, 403, 500, 503} {
		if isQuotaError(&geminiAPIError{Code: code}) {
			t.Errorf("%d must not trigger the fallback model: a different model will not help", code)
		}
	}
}

func TestResponseSchemaOffersOnlyGatedActions(t *testing.T) {
	// The model picks from an enum, but the enum is not the security boundary —
	// checkAction is. This asserts they stay in sync so a new action cannot be
	// offered to the model without also being gate-aware.
	props := responseSchema["properties"].(map[string]any)
	enum := props["action"].(map[string]any)["enum"].([]string)

	allowed := map[string]bool{"escalate": true, "none": true}
	for _, a := range allActions {
		allowed[a] = true
	}
	for _, e := range enum {
		if !allowed[e] {
			t.Errorf("schema offers %q, which is not an allowlisted action", e)
		}
	}
	for _, a := range allActions {
		found := false
		for _, e := range enum {
			if e == a {
				found = true
			}
		}
		if !found {
			t.Errorf("action %q exists but is not offered to the model", a)
		}
	}
}

func TestHealthyIgnoresWatchdogAndOpsagentItself(t *testing.T) {
	// Watchdog fires forever by design and opsagent is manual-sync, so both
	// would otherwise make every sweep look unhealthy and burn the budget.
	ev := evidence{
		Alerts:   []firingAlert{{Name: "Watchdog"}, {Name: "InfoInhibitor"}},
		ArgoApps: []appStatus{{Name: "opsagent", Health: "Missing"}},
	}
	if !ev.healthy() {
		t.Error("Watchdog and a Missing opsagent app must not count as unhealthy")
	}
}

func TestHealthyDetectsRealTrouble(t *testing.T) {
	cases := map[string]evidence{
		"firing alert":    {Alerts: []firingAlert{{Name: "ScraperDown"}}},
		"restarting pod":  {Namespaces: map[string][]podSummary{"jsa-prod": {{Phase: "Running", Ready: "1/1", Restarts: 3}}}},
		"not ready pod":   {Namespaces: map[string][]podSummary{"jsa-prod": {{Phase: "Running", Ready: "0/1"}}}},
		"degraded argo":   {ArgoApps: []appStatus{{Name: "jsa-prod", Health: "Degraded"}}},
		"pod not running": {Namespaces: map[string][]podSummary{"jsa-prod": {{Phase: "CrashLoopBackOff"}}}},
	}
	for name, ev := range cases {
		if ev.healthy() {
			t.Errorf("%s: should not be reported healthy", name)
		}
	}
}

func TestHealthyOnAQuietCluster(t *testing.T) {
	ev := evidence{
		Namespaces: map[string][]podSummary{
			"jsa-prod": {{Phase: "Running", Ready: "1/1", Restarts: 0}},
		},
		ArgoApps: []appStatus{{Name: "jsa-prod", Health: "Healthy", Sync: "Synced"}},
	}
	if !ev.healthy() {
		t.Error("a quiet cluster should skip the model call entirely")
	}
}

func TestRenderNeverClaimsMainIsUpToDateWhenUnchecked(t *testing.T) {
	// Caught in the first live run: with no GitHub token the comparison never
	// ran, Unpromoted stayed empty, and the evidence rendered "main is up to
	// date with dev" — a positive claim about something never examined. The
	// model then ruled out an unpromoted fix on that basis, which is precisely
	// the #646 signal this agent exists to catch.
	ev := evidence{Skipped: map[string]string{"unpromoted": "no OPSAGENT_GITHUB_TOKEN configured"}}
	out := ev.render()

	if strings.Contains(out, "main is up to date with dev") &&
		!strings.Contains(out, "Do NOT conclude that main is up to") {
		t.Error("unchecked comparison must not render as an up-to-date finding")
	}
	if !strings.Contains(out, "NOT CHECKED") {
		t.Error("a skipped check must be labelled NOT CHECKED")
	}
}

func TestRenderDistinguishesCheckedEmptyFromUnchecked(t *testing.T) {
	checked := evidence{Skipped: map[string]string{}}.render()
	if !strings.Contains(checked, "checked: none") {
		t.Error("a comparison that ran and found nothing should say so explicitly")
	}
	if strings.Contains(checked, "NOT CHECKED") {
		t.Error("a completed check must not be labelled NOT CHECKED")
	}
}
