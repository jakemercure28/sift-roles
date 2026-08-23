package opsagent

import (
	"context"
	"fmt"
	"strings"
)

// Evidence gathering is deterministic and runs before the model is consulted.
//
// This is the inversion that makes a 20-request/day free tier workable: rather
// than letting a model decide which of a dozen tools to call across ten round
// trips, we collect the same fixed set of facts every time and spend a single
// request asking what they mean. It also means the model is optional — every
// escalation carries this evidence whether or not a diagnosis was produced.
//
// What gets collected is deliberately the set of things that would have
// explained the incidents this repo has actually had: pod restart counts (a
// crashloop that restarts is not fixed by another restart), Argo sync state
// (drift between git and cluster), and main-vs-dev commits (the fix is merged
// but not promoted).

// evidence is the full deterministic picture handed to both the model and,
// verbatim, to the GitHub issue.
type evidence struct {
	Trigger     string
	Alert       string
	Namespaces  map[string][]podSummary
	Deployments map[string][]deploymentSummary
	Events      map[string][]eventSummary
	Logs        map[string]string
	Alerts      []firingAlert
	ArgoApps    []appStatus
	Unpromoted  []commitInfo
	Errors      []string
	// Skipped names checks that never ran, so the rendered evidence can say
	// "not checked" instead of reporting an empty result as a finding. Without
	// this, a nil GitHub client made the branch comparison render as "main is
	// up to date with dev" — a positive claim about something never examined,
	// which is exactly the signal this agent exists to notice.
	Skipped map[string]string
}

// collect gathers everything in one pass. Individual failures are recorded as
// Errors rather than aborting: a partial picture still beats no notification,
// which is the failure mode this project exists to remove.
func (a *Agent) collect(ctx context.Context, trigger, alertText string) evidence {
	ev := evidence{
		Trigger:     trigger,
		Alert:       alertText,
		Namespaces:  map[string][]podSummary{},
		Deployments: map[string][]deploymentSummary{},
		Events:      map[string][]eventSummary{},
		Logs:        map[string]string{},
	}
	ev.Skipped = map[string]string{}
	note := func(what string, err error) {
		if err != nil {
			ev.Errors = append(ev.Errors, fmt.Sprintf("%s: %v", what, err))
		}
	}

	for ns := range a.cfg.WritableNamespaces {
		pods, err := a.k8s.listPods(ctx, ns, "")
		note("list pods "+ns, err)
		if err == nil {
			ev.Namespaces[ns] = pods
		}

		deps, err := a.k8s.listDeployments(ctx, ns)
		note("list deployments "+ns, err)
		if err == nil {
			ev.Deployments[ns] = deps
		}

		events, err := a.k8s.listEvents(ctx, ns)
		note("list events "+ns, err)
		if err == nil {
			ev.Events[ns] = events
		}

		// Logs only for pods that look unhealthy. Pulling logs for every pod
		// would bury the signal and blow up the prompt for no benefit.
		for _, p := range pods {
			if !unhealthy(p) {
				continue
			}
			// previous=true when it has restarted: the current container's log
			// starts after the crash, so the evidence is in the dead one.
			logs, err := a.k8s.podLogs(ctx, ns, p.Name, "", 40, p.Restarts > 0)
			if err != nil {
				// Fall back to the live container — `previous` 400s when there
				// is no prior instance, which is common and not worth an error.
				logs, err = a.k8s.podLogs(ctx, ns, p.Name, "", 40, false)
			}
			note("logs "+ns+"/"+p.Name, err)
			if err == nil {
				ev.Logs[ns+"/"+p.Name] = logs
			}
		}
	}

	alerts, err := a.promIn.alerts(ctx)
	note("in-cluster alerts", err)
	ev.Alerts = append(ev.Alerts, alerts...)

	if a.promCo != nil {
		composeAlerts, err := a.promCo.alerts(ctx)
		note("compose alerts", err)
		ev.Alerts = append(ev.Alerts, composeAlerts...)
	}

	if a.argo != nil {
		apps, err := a.argo.listApps(ctx)
		note("argo apps", err)
		ev.ArgoApps = apps
	} else {
		ev.Skipped["argo"] = "no OPSAGENT_ARGOCD_TOKEN configured"
	}

	// The #646 question, asked every single run rather than only when someone
	// thinks to ask it: is the fix already merged and just not promoted?
	if a.gh != nil {
		commits, err := a.gh.compareBranches(ctx, "main", "dev")
		note("compare main..dev", err)
		if err != nil {
			ev.Skipped["unpromoted"] = fmt.Sprintf("the comparison failed: %v", err)
		} else {
			ev.Unpromoted = commits
		}
	} else {
		ev.Skipped["unpromoted"] = "no OPSAGENT_GITHUB_TOKEN configured"
	}

	return ev
}

// unhealthy decides which pods are worth pulling logs for.
func unhealthy(p podSummary) bool {
	if p.Phase != "Running" && p.Phase != "Succeeded" {
		return true
	}
	if p.Restarts > 0 {
		return true
	}
	// "1/1" style readiness; a mismatch means containers are not all up.
	if parts := strings.SplitN(p.Ready, "/", 2); len(parts) == 2 && parts[0] != parts[1] {
		return true
	}
	return false
}

// render turns the evidence into the text handed to the model and pasted into
// the escalation issue. One representation for both, so what a human reads is
// exactly what the model saw — no hidden context, no divergence to debug.
func (e evidence) render() string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Trigger\n\n%s\n\n", e.Trigger)
	if e.Alert != "" {
		fmt.Fprintf(&b, "# Alert\n\n%s\n", e.Alert)
	}

	b.WriteString("\n# Pods\n\n")
	for ns, pods := range e.Namespaces {
		fmt.Fprintf(&b, "## %s\n", ns)
		if len(pods) == 0 {
			b.WriteString("(none)\n")
		}
		for _, p := range pods {
			fmt.Fprintf(&b, "- %s  phase=%s ready=%s restarts=%d age=%s",
				p.Name, p.Phase, p.Ready, p.Restarts, p.Age)
			if p.LastReason != "" {
				fmt.Fprintf(&b, " last_termination=%s", p.LastReason)
			}
			if p.Message != "" {
				fmt.Fprintf(&b, " msg=%q", truncate(p.Message, 160))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("# Deployments\n\n")
	for ns, deps := range e.Deployments {
		for _, d := range deps {
			fmt.Fprintf(&b, "- %s/%s  replicas=%s image=%s age=%s\n",
				ns, d.Name, d.Replicas, d.Image, d.Age)
		}
	}

	b.WriteString("\n# Firing alerts\n\n")
	if len(e.Alerts) == 0 {
		b.WriteString("(none firing)\n")
	}
	for _, al := range e.Alerts {
		fmt.Fprintf(&b, "- [%s] %s severity=%s since=%s\n", al.Source, al.Name, al.Severity, al.ActiveSince)
		if d := al.Annotations["description"]; d != "" {
			fmt.Fprintf(&b, "    %s\n", truncate(d, 240))
		}
		// alerts.yml carries remediation hints; they are often the fastest
		// route to the right answer, so surface them rather than making the
		// model infer a fix that a human already wrote down.
		if r := al.Annotations["runbook"]; r != "" {
			fmt.Fprintf(&b, "    runbook: %s\n", truncate(r, 240))
		}
	}

	if why, skipped := e.Skipped["argo"]; skipped {
		fmt.Fprintf(&b, "\n# Argo CD\n\nNOT CHECKED (%s). Sync and health state is unknown.\n", why)
	} else if len(e.ArgoApps) > 0 {
		b.WriteString("\n# Argo CD\n\n")
		for _, app := range e.ArgoApps {
			fmt.Fprintf(&b, "- %s  sync=%s health=%s rev=%s\n",
				app.Name, app.Sync, app.Health, truncate(app.Revision, 8))
		}
	}

	b.WriteString("\n# Commits on dev but NOT on main (unpromoted)\n\n")
	if why, skipped := e.Skipped["unpromoted"]; skipped {
		// Never claim "main is up to date" when the comparison did not run.
		fmt.Fprintf(&b, "NOT CHECKED (%s). Do NOT conclude that main is up to "+
			"date with dev, and do not rule out an unpromoted fix on this basis. "+
			"If the symptom looks like a code fault, say that promotion status "+
			"could not be verified.\n", why)
	} else if len(e.Unpromoted) == 0 {
		b.WriteString("(checked: none — main is up to date with dev)\n")
	}
	for _, c := range e.Unpromoted {
		fmt.Fprintf(&b, "- %s %s\n", truncate(c.SHA, 8), truncate(firstLine(c.Message), 120))
	}

	if len(e.Events) > 0 {
		b.WriteString("\n# Recent warning events\n\n")
		for ns, events := range e.Events {
			for _, ev := range events {
				if ev.Type == "Normal" {
					continue
				}
				fmt.Fprintf(&b, "- %s %s/%s x%d (%s): %s\n",
					ns, ev.Reason, ev.Object, ev.Count, ev.Age, truncate(ev.Message, 160))
			}
		}
	}

	if len(e.Logs) > 0 {
		b.WriteString("\n# Logs from unhealthy pods\n\n")
		for key, logs := range e.Logs {
			fmt.Fprintf(&b, "## %s\n```\n%s\n```\n\n", key, truncate(logs, 3000))
		}
	}

	if len(e.Errors) > 0 {
		b.WriteString("\n# Collection errors\n\n")
		for _, errMsg := range e.Errors {
			fmt.Fprintf(&b, "- %s\n", errMsg)
		}
	}

	return b.String()
}

// healthy reports whether the evidence shows nothing worth investigating. Used
// by the sweep to skip the model call entirely on a quiet cluster, which is
// what keeps daily spend near zero.
func (e evidence) healthy() bool {
	for _, alert := range e.Alerts {
		// Watchdog fires forever by design; it is the pipeline's own liveness
		// check, not a problem.
		if alert.Name == "Watchdog" || alert.Name == "InfoInhibitor" {
			continue
		}
		return false
	}
	for _, pods := range e.Namespaces {
		for _, p := range pods {
			if unhealthy(p) {
				return false
			}
		}
	}
	for _, app := range e.ArgoApps {
		// opsagent itself is manual-sync and therefore expected to sit
		// OutOfSync until a human syncs it; that is not a fault.
		if app.Name == "opsagent" {
			continue
		}
		if app.Health == "Degraded" || app.Health == "Missing" {
			return false
		}
	}
	return true
}
