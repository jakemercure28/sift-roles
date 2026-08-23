package opsagent

import (
	"context"
	"fmt"
	"log/slog"

	"job-search-automation/internal/metrics"
)

// This file used to define Anthropic tool schemas for an agentic loop. The loop
// is gone (see the note at the top of gemini.go), but the enforcement point it
// wrapped is not: every state-changing operation still funnels through
// perform -> checkAction, and the containment tests still exercise exactly this
// path. Moving off the tool loop changed how an action is *proposed*, not how
// it is *authorized*.
type toolset struct {
	cfg    Config
	k8s    *k8sClient
	promIn *promClient
	promCo *promClient
	argo   *argoClient
	gh     *githubClient
	logger *slog.Logger
	// audit accumulates every action considered during a run, so the escalation
	// write-up and the logs agree on what happened.
	audit *auditLog
}

// perform is the single path every state-changing operation goes through: gate,
// audit, then either execute or record the dry run. Nothing writes to the
// cluster except through here.
func (t *toolset) perform(ctx context.Context, action, namespace, target, reason string, exec func() error) applyResult {
	_ = ctx

	decision := t.cfg.checkAction(action, namespace)
	if !decision.Allowed {
		t.audit.record(action, namespace, target, reason, "denied", decision.Reason)
		metrics.ObserveOpsAgentAction(action, namespace, "denied")
		t.logger.Warn("opsagent action denied", "action", action, "namespace", namespace,
			"target", target, "reason", decision.Reason)
		return applyResult{note: "DENIED: " + decision.Reason + ". Escalated instead."}
	}

	if decision.DryRun {
		t.audit.record(action, namespace, target, reason, "dry_run", decision.Reason)
		metrics.ObserveOpsAgentAction(action, namespace, "dry_run")
		t.logger.Info("opsagent action (dry run)", "action", action, "namespace", namespace,
			"target", target, "reason", reason, "why_dry_run", decision.Reason)
		return applyResult{note: fmt.Sprintf(
			"DRY RUN: would have run %s on %s in namespace %q (%s). Nothing was changed, "+
				"so this still needs to be applied by hand.",
			action, target, namespace, decision.Reason)}
	}

	if err := exec(); err != nil {
		t.audit.record(action, namespace, target, reason, "failed", err.Error())
		metrics.ObserveOpsAgentAction(action, namespace, "failed")
		t.logger.Error("opsagent action failed", "action", action, "namespace", namespace,
			"target", target, "error", err)
		return applyResult{note: "FAILED: " + err.Error()}
	}

	t.audit.record(action, namespace, target, reason, "executed", "")
	metrics.ObserveOpsAgentAction(action, namespace, "executed")
	t.logger.Info("opsagent action executed", "action", action, "namespace", namespace,
		"target", target, "reason", reason)
	return applyResult{
		executed: true,
		note: fmt.Sprintf("EXECUTED: %s on %s in namespace %q. Verify the effect before "+
			"trusting it: re-check pod state after a short wait.", action, target, namespace),
	}
}
