// Package opsagent is a bounded self-healing agent for the k3s deployment.
//
// It receives alerts from Alertmanager (and sweeps periodically), investigates
// with read-only tools against the Kubernetes API, both Prometheus instances,
// Argo CD and GitHub, and may then execute a small allowlisted set of
// remediations. Anything outside that allowlist is escalated to a GitHub issue
// rather than attempted.
//
// Two independent layers bound what it can do. The action gate in gate.go is
// enforced in Go, so the model cannot be prompted past it; the ServiceAccount's
// RBAC Role is the backstop, so even a bug in the gate cannot exceed the
// permissions the cluster grants. Writes are opt-in per action and default off.
package opsagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"job-search-automation/internal/metrics"
)

// Agent runs investigations. One Agent is shared across triggers; each run gets
// its own audit log so two investigations never interleave.
type Agent struct {
	cfg    Config
	gemini *geminiClient
	k8s    *k8sClient
	promIn *promClient
	promCo *promClient
	argo   *argoClient
	gh     *githubClient
	logger *slog.Logger
}

// New builds an Agent from config. Optional integrations (Argo CD, GitHub) are
// nil when their credential is absent, and their tools are simply not offered.
func New(cfg Config, logger *slog.Logger) (*Agent, error) {
	k8s, err := newK8sClient()
	if err != nil {
		return nil, err
	}
	a := &Agent{
		cfg:    cfg,
		gemini: newGeminiClient(cfg, logger),
		k8s:    k8s,
		promIn: newPromClient("in_cluster", cfg.PromInCluster),
		promCo: newPromClient("compose", cfg.PromCompose),
		logger: logger,
	}
	if cfg.ArgoCDToken != "" {
		a.argo = newArgoClient(cfg.ArgoCDServer, cfg.ArgoCDToken)
	} else {
		logger.Warn("opsagent: OPSAGENT_ARGOCD_TOKEN unset, Argo CD tools disabled")
	}
	if cfg.GitHubToken != "" {
		a.gh = newGitHubClient(cfg.GitHubToken, cfg.GitHubRepo)
	} else {
		logger.Warn("opsagent: OPSAGENT_GITHUB_TOKEN unset, escalation and commit tools disabled")
	}
	return a, nil
}

// Result is the outcome of one investigation.
type Result struct {
	// Trigger is "alert" or "sweep".
	Trigger string
	// Fingerprint dedupes escalations for the same underlying problem.
	Fingerprint string
	// Summary is the model's final write-up.
	Summary string
	// Remediated reports whether the agent actually changed the cluster.
	Remediated bool
	// EscalationURL is set when an issue was filed or commented on.
	EscalationURL string
	// Duration is the wall-clock time of the run.
	Duration time.Duration
	// InputTokens and OutputTokens are the run's token usage.
	InputTokens  int64
	OutputTokens int64
}

// systemPrompt is stable across every run so it caches cleanly. It carries the
// operating rules; the per-run trigger goes in the user turn.
func (a *Agent) systemPrompt() string {
	var b strings.Builder
	b.WriteString(`You are the on-call SRE for a small self-hosted production system. You investigate alerts and either apply a narrow, pre-approved fix or hand the problem to Jake, the sole operator.

# The system

A personal job-search pipeline running on single-node k3s on a ThinkPad, deployed by Argo CD from the private repo job-search-automation-private.

- Namespace jsa-prod runs production; jsa-staging runs staging. Each has two workloads: go-backend (dashboard, scorer, scraping and maintenance crons) and scraper-service (a Fastify plus Playwright job-board scraper).
- Argo CD applies manifests from deploy/k8s. The dev branch deploys to staging; main deploys to prod. Promoting dev to main is a deliberate manual step Jake performs. This matters constantly: a fix merged to dev is NOT running in prod yet.
- Two Prometheus instances: in_cluster covers pods, containers and nodes; compose covers the jsa_* application metrics.
- Postgres (self-hosted Supabase) runs outside the cluster on the same host. You cannot touch it.

# What you are given

You do not call tools. Everything already gathered is in the message: pod state and restart counts, deployments, firing alerts with their annotations, Argo CD sync and health, recent warning events, logs from unhealthy pods, and the list of commits that are on dev but not yet on main.

That last section is the one people forget to check. If it is non-empty, prod is running older code than dev, and a commit there may already fix what you are looking at.

Answer only from that evidence. If it does not contain what you need, say which specific thing is missing rather than inferring it — a confident wrong root cause costs more than an honest gap, and Jake can go look.

Return exactly one action. If nothing is wrong, use "none". If a human must act, use "escalate" and put the exact command in the command field.

# When a fix is NOT a restart

Restarting is the tool people reach for and it is usually wrong. It only helps when a process has fallen into a bad in-memory state that a fresh start clears: a wedged connection pool, a stuck lock, a corrupt cache.

It does not help when the problem is in the code or the configuration, because the replacement pod runs the same image and hits the same fault. The clearest signal is the restart count: Kubernetes has already tried restarting, and if a pod has restarted several times and is still failing, another restart is not a fix. It is noise that also destroys the evidence in the current instance's logs.

In that situation, find out whether the fix already exists. Compare main to dev. If a commit on dev addresses the symptom and prod runs main, the correct outcome is an escalation asking Jake to promote dev to main. Say so plainly and do not restart anything.

# Your authority

You may apply these actions and nothing else:

`)
	for _, act := range allActions {
		b.WriteString("- " + act + "\n")
	}
	b.WriteString(`
Every action is checked in code before it runs, and is confined to the jsa-prod and jsa-staging namespaces. If an action is denied, do not try to route around it: that denial is deliberate. Escalate instead.

An action may come back as DRY RUN, which means it was permitted in principle but deliberately not applied. Continue your investigation as if it had been applied, and state clearly in your summary that it still needs to be done by hand.

Some remediations are outside your reach entirely and must always be escalated with the exact command for Jake to run:
- A wedged scoring batch (ScoreBatchStuckOpen) is cleared with ` + "`server batch evict <batch-name>`" + ` in the go-backend pod. You do not have database access.
- Promoting dev to main.
- Anything touching Postgres, Supabase, backups, or the host itself.

# Your summary

Finish with a write-up for someone who was not watching. Open with one sentence saying what is wrong and whether it is now fixed. Then the evidence that led you there, then what you did or what needs doing. Name pods, deployments, commits and metrics explicitly rather than referring to them as "the pod" or "the fix". Write complete sentences, not shorthand or arrow chains.

Say plainly when you could not determine the cause. A clear "I could not tell, here is what I ruled out" is more useful than a confident guess, and a wrong root cause sends Jake down the wrong path.`)
	return b.String()
}

// Run investigates one trigger and returns the outcome.
//
// The shape is: gather evidence deterministically, ask the model once what it
// means, then route the answer through the same gate the tool loop used to.
// There is no tool loop — see the note at the top of gemini.go for why a
// 20-request/day free tier makes one impossible, and why that turned out to be
// a better design rather than a compromise.
//
// The model is the optional step. If it is over budget, rate-limited, or
// broken, the evidence is still escalated. Alert delivery does not depend on an
// LLM being reachable.
func (a *Agent) Run(ctx context.Context, trigger, fingerprint, task string) (Result, error) {
	started := time.Now()
	res := Result{Trigger: trigger, Fingerprint: fingerprint}
	audit := newAuditLog()

	ev := a.collect(ctx, trigger, task)

	// A quiet cluster on a scheduled sweep costs zero model calls. This is what
	// keeps daily usage far below the free-tier ceiling: we only spend a
	// request when something actually looks wrong.
	if trigger == "sweep" && ev.healthy() {
		res.Summary = "Sweep found no firing alerts, no unhealthy pods, and no degraded Argo applications. No model call made."
		res.Duration = time.Since(started)
		metrics.ObserveOpsAgentRun(trigger, "healthy", res.Duration)
		a.logger.Info("opsagent: sweep clean, skipped model call", "fingerprint", fingerprint)
		return res, nil
	}

	rendered := ev.render()

	v, tokens, err := a.gemini.diagnose(ctx, a.systemPrompt(), rendered)
	res.InputTokens = int64(tokens)
	switch {
	case err == nil:
		res.Summary = fmt.Sprintf("%s\n\n%s", v.Diagnosis, v.Reason)
	case errors.Is(err, errBudgetExhausted):
		spent, budget := a.gemini.spentToday()
		res.Summary = fmt.Sprintf(
			"No diagnosis: the daily model-call budget is exhausted (%d/%d used today). "+
				"The evidence below was still collected and is complete.", spent, budget)
		a.logger.Warn("opsagent: budget exhausted, escalating evidence only",
			"fingerprint", fingerprint, "spent", spent, "budget", budget)
		metrics.ObserveOpsAgentRun(trigger, "budget_exhausted", time.Since(started))
	default:
		res.Summary = fmt.Sprintf(
			"No diagnosis: the model call failed (%v). The evidence below was still collected and is complete.", err)
		a.logger.Error("opsagent: model call failed, escalating evidence only",
			"fingerprint", fingerprint, "error", err)
		metrics.ObserveOpsAgentRun(trigger, "model_error", time.Since(started))
	}

	// Apply the action if the model named one and the gate allows it. A failed
	// or skipped diagnosis leaves v zero-valued, so action is "" and this is a
	// no-op — evidence-only escalation is the natural fallthrough, not a
	// special case.
	if v.Action != "" && v.Action != "escalate" && v.Action != "none" {
		out := a.apply(ctx, audit, v)
		res.Remediated = out.executed
		res.Summary += "\n\n" + out.note
	}

	// Escalate unless the agent both understood the problem and fixed it.
	if !res.Remediated {
		url, escErr := a.escalate(ctx, fingerprint, v, rendered, res.Summary)
		if escErr != nil {
			a.logger.Error("opsagent: escalation failed", "fingerprint", fingerprint, "error", escErr)
		}
		res.EscalationURL = url
	}

	res.Duration = time.Since(started)
	if err == nil {
		metrics.ObserveOpsAgentRun(trigger, verdictOutcome(v, res.Remediated), res.Duration)
	}
	a.logger.Info("opsagent audit", "fingerprint", fingerprint, "entries", len(audit.all()))
	return res, nil
}

// applyResult reports what happened to a proposed action.
type applyResult struct {
	executed bool
	note     string
}

// apply routes the model's proposed action through the gate. Every write the
// agent can perform still goes through runAction, so the containment rules and
// their tests are unchanged by the move off the tool loop.
func (a *Agent) apply(ctx context.Context, audit *auditLog, v verdict) applyResult {
	ts := &toolset{
		cfg: a.cfg, k8s: a.k8s, promIn: a.promIn, promCo: a.promCo,
		argo: a.argo, gh: a.gh, logger: a.logger, audit: audit,
	}

	switch v.Action {
	case ActionArgoSync:
		if err := a.cfg.checkArgoApp(v.Target); err != nil {
			return applyResult{note: "DENIED: " + err.Error() + ". Escalating instead."}
		}
		return ts.perform(ctx, ActionArgoSync, "", "application/"+v.Target, v.Reason,
			func() error { return a.argo.sync(ctx, v.Target) })

	case ActionRolloutRestart:
		return ts.perform(ctx, ActionRolloutRestart, v.Namespace, "deployment/"+v.Target, v.Reason,
			func() error { return a.k8s.rolloutRestart(ctx, v.Namespace, v.Target) })

	case ActionDeletePod:
		return ts.perform(ctx, ActionDeletePod, v.Namespace, "pod/"+v.Target, v.Reason,
			func() error { return a.k8s.deletePod(ctx, v.Namespace, v.Target) })

	case ActionScaleDeploy:
		// The model has no replica field: scaling is only ever used to stop a
		// workload that is actively causing harm, and a model that can pick an
		// arbitrary replica count is a bigger blast radius than the remediation
		// is worth. Scaling up again is a human decision.
		if err := a.cfg.checkReplicas(0); err != nil {
			return applyResult{note: "DENIED: " + err.Error()}
		}
		return ts.perform(ctx, ActionScaleDeploy, v.Namespace, "deployment/"+v.Target+" -> 0 replicas", v.Reason,
			func() error { return a.k8s.scaleDeployment(ctx, v.Namespace, v.Target, 0) })
	}
	return applyResult{note: fmt.Sprintf("Unknown action %q; escalating instead.", v.Action)}
}

// escalate files (or comments on) a GitHub issue carrying the diagnosis and the
// full evidence. Without a GitHub token this degrades to a log line, which is
// worse but still not silent.
func (a *Agent) escalate(ctx context.Context, fingerprint string, v verdict, rendered, summary string) (string, error) {
	var b strings.Builder
	b.WriteString(summary)
	b.WriteString("\n\n")
	if v.Command != "" {
		fmt.Fprintf(&b, "**Suggested command:**\n\n```\n%s\n```\n\n", v.Command)
	}
	b.WriteString("<details>\n<summary>Evidence collected (this is exactly what the model was shown)</summary>\n\n")
	b.WriteString(rendered)
	b.WriteString("\n</details>\n")

	title := fmt.Sprintf("opsagent: %s", firstLine(v.Diagnosis))
	if v.Diagnosis == "" {
		title = "opsagent: " + fingerprint + " needs attention"
	}

	if a.gh == nil {
		a.logger.Warn("opsagent: no GitHub token, escalation stays in logs only",
			"fingerprint", fingerprint, "summary", summary)
		return "", nil
	}
	metrics.ObserveOpsAgentEscalation()
	return a.gh.escalate(ctx, fingerprint, truncate(title, 120), b.String())
}

func verdictOutcome(v verdict, remediated bool) string {
	switch {
	case remediated:
		return "remediated"
	case v.Action == "none":
		return "no_action_needed"
	default:
		return "escalated"
	}
}
