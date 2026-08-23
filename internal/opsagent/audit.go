package opsagent

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// auditEntry is one action the agent considered during a run, whether or not it
// happened. Denials and dry runs are recorded as deliberately as executions:
// what the agent wanted to do is as informative as what it did.
type auditEntry struct {
	At        time.Time
	Action    string
	Namespace string
	Target    string
	// Reason is the model's own justification, captured from the tool input.
	Reason string
	// Outcome is one of executed, dry_run, denied, failed.
	Outcome string
	// Detail carries the denial reason or the execution error.
	Detail string
}

// auditLog collects entries for one run. Tool handlers are invoked concurrently
// when the model calls several tools in a turn, so it is mutex-guarded.
type auditLog struct {
	mu      sync.Mutex
	entries []auditEntry
}

func newAuditLog() *auditLog {
	return &auditLog{}
}

func (a *auditLog) record(action, namespace, target, reason, outcome, detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, auditEntry{
		At:        time.Now().UTC(),
		Action:    action,
		Namespace: namespace,
		Target:    target,
		Reason:    reason,
		Outcome:   outcome,
		Detail:    detail,
	})
}

func (a *auditLog) all() []auditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]auditEntry, len(a.entries))
	copy(out, a.entries)
	return out
}

// executed reports whether anything actually changed the cluster. This drives
// the run verdict: an agent that only proposed changes has not healed anything.
func (a *auditLog) executed() bool {
	for _, e := range a.all() {
		if e.Outcome == "executed" {
			return true
		}
	}
	return false
}

// markdown renders the audit trail for an escalation issue.
func (a *auditLog) markdown() string {
	entries := a.all()
	if len(entries) == 0 {
		return "_No actions were attempted._"
	}
	var b strings.Builder
	b.WriteString("| Time (UTC) | Action | Target | Outcome | Detail |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, e := range entries {
		target := e.Target
		if e.Namespace != "" {
			target = e.Namespace + "/" + e.Target
		}
		detail := e.Detail
		if detail == "" {
			detail = e.Reason
		}
		fmt.Fprintf(&b, "| %s | `%s` | `%s` | %s | %s |\n",
			e.At.Format("15:04:05"), e.Action, target, e.Outcome, sanitizeCell(detail))
	}
	return b.String()
}

// sanitizeCell keeps a free-text value from breaking the markdown table.
func sanitizeCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return truncate(s, 200)
}
