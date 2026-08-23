package opsagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"job-search-automation/internal/metrics"
)

// alertmanagerPayload is the subset of Alertmanager's webhook body the agent
// uses. Alertmanager groups alerts, so one delivery can carry several.
type alertmanagerPayload struct {
	Status      string            `json:"status"`
	Receiver    string            `json:"receiver"`
	GroupLabels map[string]string `json:"groupLabels"`
	Alerts      []alertEntry      `json:"alerts"`
}

// alertEntry is one alert inside a grouped Alertmanager delivery.
type alertEntry struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     string            `json:"startsAt"`
	Fingerprint  string            `json:"fingerprint"`
	GeneratorURL string            `json:"generatorURL"`
}

// Server serves the Alertmanager webhook and runs the periodic sweep. Runs are
// serialized: two investigations at once would race on the same remediation and
// double the token spend for one problem.
type Server struct {
	agent  *Agent
	cfg    Config
	mu     sync.Mutex
	inTerm bool
	// recent suppresses repeat investigations of the same fingerprint. A
	// flapping alert re-fires every repeat_interval, and re-investigating an
	// unchanged problem produces an identical write-up at full token cost.
	recent map[string]time.Time
}

// cooldown is how long a fingerprint stays suppressed after being investigated.
const cooldown = 45 * time.Minute

// claimResult reports why a delivery was or was not taken up. The caller needs
// the distinction to answer Alertmanager correctly: a cooldown skip is
// deliberate and should be acknowledged, but a busy skip drops work and must
// not be.
type claimResult int

const (
	claimOK claimResult = iota
	claimBusy
	claimCooldown
)

func NewServer(agent *Agent, cfg Config) *Server {
	return &Server{agent: agent, cfg: cfg, recent: map[string]time.Time{}}
}

// Handler builds the HTTP surface: the Alertmanager receiver, a liveness probe,
// and this workload's own metrics.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/alert", s.handleAlert)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	mux.Handle("/metrics", metrics.Handler())
	return mux
}

func (s *Server) handleAlert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var payload alertmanagerPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		s.agent.logger.Warn("opsagent: malformed alertmanager payload", "error", err)
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	// Resolved notifications need no investigation; the problem is over.
	if payload.Status == "resolved" {
		w.WriteHeader(http.StatusOK)
		return
	}

	fingerprint, task := describeAlerts(payload)
	if task == "" {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Claim before answering, because the answer depends on the outcome.
	switch s.claim(fingerprint) {
	case claimCooldown:
		// Deliberate suppression: an identical investigation just ran and would
		// produce an identical write-up. Acknowledge, so Alertmanager does not
		// retry something we are intentionally skipping.
		w.WriteHeader(http.StatusOK)
		return
	case claimBusy:
		// Runs are serialized, so this delivery is not being handled at all. Do
		// NOT acknowledge it: a 2xx tells Alertmanager it landed, and the next
		// notification for an unchanged group is repeat_interval away (12h), so
		// a 20-minute investigation of one alert could swallow a different alert
		// for half a day. A 503 makes Alertmanager retry this delivery instead.
		http.Error(w, "investigation in progress", http.StatusServiceUnavailable)
		return
	}

	// Acknowledge immediately. An investigation takes minutes and Alertmanager
	// would retry a delivery it thinks timed out, producing duplicate runs.
	w.WriteHeader(http.StatusAccepted)

	go func() {
		defer s.release()
		s.investigate("alert", fingerprint, task)
	}()
}

// StartSweep registers the periodic sweep on the given cron scheduler. The
// sweep is a backstop for problems no rule covers, and it re-checks anything
// still firing that an earlier run left escalated.
func (s *Server) StartSweep(c *cron.Cron) error {
	_, err := c.AddFunc(s.cfg.SweepSchedule, func() {
		// Unlike an alert delivery there is nobody to retry for, so a busy or
		// cooled-down sweep is simply skipped until the next tick.
		if s.claim("periodic-sweep") != claimOK {
			return
		}
		defer s.release()
		s.investigate("sweep", "periodic-sweep", sweepTask)
	})
	return err
}

const sweepTask = `Run a periodic health sweep of the cluster. Nothing specific has alerted; you are looking for problems that no rule covers.

Check pod state in jsa-prod and jsa-staging, whether any alert is firing or pending, and whether Argo CD reports every Application as Synced and Healthy. Look for pods that are not Ready, restart counts that have climbed, and workloads whose running image is older than what the branch they track has merged.

If everything is healthy, say so in one sentence and stop. Do not manufacture work, and do not take any action when there is no problem to fix.`

// investigate runs one agent pass. The caller must already hold the claim and is
// responsible for releasing it.
func (s *Server) investigate(trigger, fingerprint, task string) {
	// Bound the run well above the task budget so a slow tool cannot wedge the
	// agent indefinitely, while still leaving room for a real investigation.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	res, err := s.agent.Run(ctx, trigger, fingerprint, task)
	if err != nil {
		s.agent.logger.Error("opsagent run failed", "trigger", trigger,
			"fingerprint", fingerprint, "error", err)
		return
	}
	s.agent.logger.Info("opsagent run complete", "trigger", trigger,
		"fingerprint", fingerprint, "remediated", res.Remediated,
		"escalation", res.EscalationURL, "duration", res.Duration.Round(time.Second))
}

// claim serializes runs and applies the per-fingerprint cooldown. On claimOK the
// caller owns the claim and must call release.
func (s *Server) claim(fingerprint string) claimResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inTerm {
		s.agent.logger.Info("opsagent: investigation already in progress, skipping",
			"fingerprint", fingerprint)
		return claimBusy
	}
	if last, ok := s.recent[fingerprint]; ok && time.Since(last) < cooldown {
		s.agent.logger.Info("opsagent: fingerprint in cooldown, skipping",
			"fingerprint", fingerprint, "last_run", last.Format(time.RFC3339))
		return claimCooldown
	}
	// Prune so a long-lived process does not accumulate fingerprints forever.
	for fp, at := range s.recent {
		if time.Since(at) > 24*time.Hour {
			delete(s.recent, fp)
		}
	}
	s.recent[fingerprint] = time.Now()
	s.inTerm = true
	return claimOK
}

func (s *Server) release() {
	s.mu.Lock()
	s.inTerm = false
	s.mu.Unlock()
}

// describeAlerts turns an Alertmanager group into a stable fingerprint and a
// task description. The fingerprint is derived from the alert names rather than
// Alertmanager's own per-alert fingerprint so that a group which re-fires with
// the same alerts dedupes onto one escalation issue.
func describeAlerts(p alertmanagerPayload) (string, string) {
	if len(p.Alerts) == 0 {
		return "", ""
	}
	var names []string
	var b strings.Builder
	b.WriteString("The following alert(s) are firing. Investigate and either apply an allowlisted remediation or escalate.\n\n")
	for _, a := range p.Alerts {
		if a.Status == "resolved" {
			continue
		}
		name := a.Labels["alertname"]
		if name == "" {
			name = "unnamed"
		}
		names = append(names, name)

		fmt.Fprintf(&b, "## %s\n", name)
		if sev := a.Labels["severity"]; sev != "" {
			fmt.Fprintf(&b, "- severity: %s\n", sev)
		}
		if a.StartsAt != "" {
			fmt.Fprintf(&b, "- firing since: %s (%s ago)\n", a.StartsAt, since(a.StartsAt))
		}
		for _, k := range sortedStringKeys(a.Labels) {
			if k == "alertname" || k == "severity" {
				continue
			}
			fmt.Fprintf(&b, "- %s: %s\n", k, a.Labels[k])
		}
		// Annotations carry the remediation hint authored in alerts.yml, which
		// is often the fastest route to the right fix.
		for _, k := range sortedStringKeys(a.Annotations) {
			fmt.Fprintf(&b, "- %s: %s\n", k, a.Annotations[k])
		}
		b.WriteString("\n")
	}
	if len(names) == 0 {
		return "", ""
	}
	sort.Strings(names)
	return strings.Join(dedupe(names), "+"), b.String()
}

func sortedStringKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func dedupe(in []string) []string {
	out := in[:0:0]
	seen := map[string]bool{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
