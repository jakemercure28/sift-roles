package opsagent

import (
	"context"
	"strings"
	"testing"
)

// baseConfig is a fully-armed config: dry-run off and every action enabled. The
// tests below take capability away from it, so a gate that silently stops
// enforcing fails loudly rather than passing by default.
func baseConfig() Config {
	return Config{
		DryRun: false,
		EnabledActions: map[string]bool{
			ActionRolloutRestart: true,
			ActionDeletePod:      true,
			ActionScaleDeploy:    true,
			ActionArgoSync:       true,
		},
		WritableNamespaces: map[string]bool{"jsa-prod": true, "jsa-staging": true},
		MaxReplicas:        3,
		SyncableApps:       map[string]bool{"jsa-prod": true, "jsa-staging": true},
	}
}

func TestCheckActionAllowsArmedActionInWritableNamespace(t *testing.T) {
	got := baseConfig().checkAction(ActionRolloutRestart, "jsa-prod")
	if !got.Allowed || got.DryRun {
		t.Fatalf("armed action in a writable namespace should execute, got %+v", got)
	}
}

func TestCheckActionRejectsUnknownAction(t *testing.T) {
	// The model cannot invoke a tool that does not exist, but the gate is the
	// backstop for a future tool wired up without an allowlist entry.
	got := baseConfig().checkAction("exec_into_pod", "jsa-prod")
	if got.Allowed {
		t.Fatalf("unknown action must be denied, got %+v", got)
	}
	if !strings.Contains(got.Reason, "allowlisted") {
		t.Errorf("denial reason should explain the allowlist, got %q", got.Reason)
	}
}

func TestEvictScoreBatchIsNotAnAction(t *testing.T) {
	// Clearing a wedged scoring batch needs database and Gemini credentials, so
	// it is escalation-only. It must not be reachable as an action, and arming it
	// must fail loudly rather than be accepted as a no-op.
	if got := baseConfig().checkAction("evict_score_batch", "jsa-prod"); got.Allowed {
		t.Errorf("evict_score_batch must not be an allowlisted action, got %+v", got)
	}
	t.Setenv("GEMINI_API_KEY", "test-key")
	t.Setenv("OPSAGENT_ENABLED_ACTIONS", "evict_score_batch")
	if _, err := LoadConfig(); err == nil {
		t.Error("arming evict_score_batch should fail startup as an unknown action")
	}
}

func TestCheckActionRejectsNamespacesOutsideTheAppScope(t *testing.T) {
	// These are readable so the agent can diagnose, but never writable: the
	// agent must not be able to restart Argo, Prometheus, or itself.
	for _, ns := range []string{"monitoring", "argocd", "kube-system", "default"} {
		t.Run(ns, func(t *testing.T) {
			got := baseConfig().checkAction(ActionDeletePod, ns)
			if got.Allowed {
				t.Fatalf("namespace %q must be read-only, got %+v", ns, got)
			}
			if !strings.Contains(got.Reason, "read-only") {
				t.Errorf("denial reason should say the namespace is read-only, got %q", got.Reason)
			}
		})
	}
}

func TestCheckActionFallsBackToDryRunNotDenial(t *testing.T) {
	// An action that is understood but not armed should still be reported to
	// the model as a dry run, so the transcript records what it wanted to do.
	// Denying outright would hide the intent.
	cfg := baseConfig()
	cfg.EnabledActions = map[string]bool{ActionRolloutRestart: true}

	got := cfg.checkAction(ActionDeletePod, "jsa-prod")
	if !got.Allowed || !got.DryRun {
		t.Fatalf("un-armed action should be a dry run, got %+v", got)
	}
	if !strings.Contains(got.Reason, "OPSAGENT_ENABLED_ACTIONS") {
		t.Errorf("reason should name the env var that arms it, got %q", got.Reason)
	}
}

func TestDryRunOverridesEveryEnabledAction(t *testing.T) {
	cfg := baseConfig()
	cfg.DryRun = true
	for _, action := range allActions {
		got := cfg.checkAction(action, "jsa-prod")
		if !got.DryRun {
			t.Errorf("%s: DryRun must win over EnabledActions, got %+v", action, got)
		}
	}
}

func TestDryRunDoesNotRescueAnOutOfScopeNamespace(t *testing.T) {
	// Scope is checked before dry-run so the denial reason names the real
	// problem. A dry-run "success" against kube-system would teach the model
	// that the namespace is fair game once writes are armed.
	cfg := baseConfig()
	cfg.DryRun = true
	got := cfg.checkAction(ActionDeletePod, "kube-system")
	if got.Allowed {
		t.Fatalf("namespace scope must be enforced even in dry run, got %+v", got)
	}
}

func TestCheckActionRejectsAnEmptyNamespaceExceptForArgoSync(t *testing.T) {
	// An empty namespace must not skip the allowlist. argocd_sync is the sole
	// exemption: Argo Applications are not scoped by deployment namespace, so
	// checkArgoApp bounds that one instead.
	cfg := baseConfig()
	for _, action := range []string{ActionRolloutRestart, ActionDeletePod, ActionScaleDeploy} {
		if got := cfg.checkAction(action, ""); got.Allowed {
			t.Errorf("%s with no namespace must be denied, got %+v", action, got)
		}
	}
	if got := cfg.checkAction(ActionArgoSync, ""); !got.Allowed {
		t.Errorf("argocd_sync legitimately has no namespace, got %+v", got)
	}
}

func TestCheckArgoAppRefusesSelfSyncAndTheAppOfApps(t *testing.T) {
	// Syncing its own Application would let the agent arm itself, since arming
	// lives in that Deployment's env. Syncing root reaches every app.
	cfg := baseConfig()
	for _, app := range []string{"opsagent", "root", "monitoring", "alloy"} {
		if err := cfg.checkArgoApp(app); err == nil {
			t.Errorf("%s must not be syncable", app)
		}
	}
	for _, app := range []string{"jsa-prod", "jsa-staging"} {
		if err := cfg.checkArgoApp(app); err != nil {
			t.Errorf("%s should be syncable, got %v", app, err)
		}
	}
}

func TestLoadConfigRefusesASelfSyncableConfig(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "test-key")
	for _, bad := range []string{"jsa-prod,opsagent", "root"} {
		t.Setenv("OPSAGENT_SYNCABLE_APPS", bad)
		if _, err := LoadConfig(); err == nil {
			t.Errorf("OPSAGENT_SYNCABLE_APPS=%q should refuse to start", bad)
		}
	}
}

func TestCheckReplicasBoundsScaling(t *testing.T) {
	cfg := baseConfig()
	tests := []struct {
		name     string
		replicas int
		wantErr  bool
	}{
		{"scale to zero stops a thrashing workload", 0, false},
		{"scale to the ceiling", 3, false},
		{"one past the ceiling", 4, true},
		{"far past the ceiling", 50, true},
		{"negative", -1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := cfg.checkReplicas(tc.replicas)
			if tc.wantErr && err == nil {
				t.Fatalf("replicas=%d should have been rejected", tc.replicas)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("replicas=%d should have been allowed: %v", tc.replicas, err)
			}
		})
	}
}

// TestPerformDryRunNeverExecutes is the load-bearing test: it asserts that
// nothing reaches the cluster when the gate says dry run. A regression here is
// the one that lets an unvetted agent write to prod.
func TestPerformDryRunNeverExecutes(t *testing.T) {
	cases := []struct {
		name      string
		cfg       Config
		action    string
		namespace string
	}{
		{"global dry run", func() Config { c := baseConfig(); c.DryRun = true; return c }(), ActionRolloutRestart, "jsa-prod"},
		{"action not armed", func() Config {
			c := baseConfig()
			c.EnabledActions = map[string]bool{}
			return c
		}(), ActionRolloutRestart, "jsa-prod"},
		{"namespace out of scope", baseConfig(), ActionDeletePod, "kube-system"},
		{"unknown action", baseConfig(), "drop_database", "jsa-prod"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executed := false
			ts := &toolset{cfg: tc.cfg, logger: discardLogger(), audit: newAuditLog()}

			result := ts.perform(context.Background(), tc.action, tc.namespace,
				"deployment/scraper-service", "test", func() error {
					executed = true
					return nil
				})
			if executed {
				t.Fatal("the cluster was touched despite the gate refusing")
			}
			if result.executed {
				t.Fatal("a gated action must not report itself as executed")
			}
			body := result.note
			if !strings.HasPrefix(body, "DRY RUN:") && !strings.HasPrefix(body, "DENIED:") {
				t.Errorf("result should tell the model it did not happen, got %q", body)
			}
		})
	}
}

func TestPerformExecutesWhenFullyArmed(t *testing.T) {
	executed := false
	ts := &toolset{cfg: baseConfig(), logger: discardLogger(), audit: newAuditLog()}

	result := ts.perform(context.Background(), ActionRolloutRestart, "jsa-prod",
		"deployment/scraper-service", "wedged pool", func() error {
			executed = true
			return nil
		})
	if !executed {
		t.Fatal("a fully armed action should have executed")
	}
	if !result.executed {
		t.Error("an executed action should report executed=true")
	}
	if !strings.HasPrefix(result.note, "EXECUTED:") {
		t.Errorf("expected an EXECUTED result, got %q", result.note)
	}
	if !ts.audit.executed() {
		t.Error("audit log should record that the cluster changed")
	}
}

func TestPerformReportsExecutionFailure(t *testing.T) {
	// A failed action must come back as a tool result, not a Go error: the
	// model should get the chance to escalate rather than the run aborting.
	ts := &toolset{cfg: baseConfig(), logger: discardLogger(), audit: newAuditLog()}

	result := ts.perform(context.Background(), ActionRolloutRestart, "jsa-prod",
		"deployment/scraper-service", "wedged pool", func() error {
			return errForbidden{}
		})
	if !strings.HasPrefix(result.note, "FAILED:") {
		t.Errorf("expected a FAILED result, got %q", result.note)
	}
	if result.executed {
		t.Error("a failed action must not report executed=true")
	}
	if ts.audit.executed() {
		t.Error("a failed action must not count as remediation")
	}
}

func TestAuditRecordsDryRunsAndDenials(t *testing.T) {
	cfg := baseConfig()
	cfg.DryRun = true
	ts := &toolset{cfg: cfg, logger: discardLogger(), audit: newAuditLog()}

	_ = ts.perform(context.Background(), ActionRolloutRestart, "jsa-prod", "deployment/x", "why", func() error { return nil })
	_ = ts.perform(context.Background(), ActionDeletePod, "kube-system", "pod/y", "why", func() error { return nil })

	entries := ts.audit.all()
	if len(entries) != 2 {
		t.Fatalf("expected both attempts recorded, got %d", len(entries))
	}
	if entries[0].Outcome != "dry_run" {
		t.Errorf("first entry outcome = %q, want dry_run", entries[0].Outcome)
	}
	if entries[1].Outcome != "denied" {
		t.Errorf("second entry outcome = %q, want denied", entries[1].Outcome)
	}
	if ts.audit.executed() {
		t.Error("no action executed, so the run must not be reported as remediated")
	}
	if md := ts.audit.markdown(); !strings.Contains(md, "rollout_restart") || !strings.Contains(md, "denied") {
		t.Errorf("escalation table should show both attempts, got:\n%s", md)
	}
}

func TestLoadConfigRejectsUnknownEnabledAction(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "test-key")
	t.Setenv("OPSAGENT_ENABLED_ACTIONS", "rollout_restart,exec_into_pod")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("a typo in OPSAGENT_ENABLED_ACTIONS should fail at startup, not silently arm nothing")
	}
}

func TestLoadConfigDefaultsToDryRunWithNoActionsArmed(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "test-key")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.DryRun {
		t.Error("dry run must be the default: writes are opt-in")
	}
	if len(cfg.EnabledActions) != 0 {
		t.Errorf("no action should be armed by default, got %v", cfg.EnabledActionNames())
	}
	if cfg.WritableNamespaces["monitoring"] || cfg.WritableNamespaces["kube-system"] {
		t.Errorf("default writable namespaces must be app-only, got %v", cfg.WritableNamespaceNames())
	}
}

func TestLoadConfigRejectsAZeroDailyBudget(t *testing.T) {
	// A budget of zero would mean the agent silently never calls the model,
	// which looks identical to it working and finding nothing.
	t.Setenv("GEMINI_API_KEY", "test-key")
	t.Setenv("OPSAGENT_DAILY_CALL_BUDGET", "0")
	if _, err := LoadConfig(); err == nil {
		t.Error("a daily call budget of 0 should fail startup")
	}
}

type errForbidden struct{}

func (errForbidden) Error() string { return "deployments.apps is forbidden" }
