package opsagent

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Action names. These are the complete set of state-changing operations the
// agent can perform. A capability that is not in this list has no handler and
// no tool definition, so the model cannot invoke it however it is prompted.
const (
	ActionRolloutRestart = "rollout_restart"
	ActionDeletePod      = "delete_pod"
	ActionScaleDeploy    = "scale_deployment"
	ActionArgoSync       = "argocd_sync"
)

// Deliberately absent: evicting a wedged scoring batch. It needs the database
// credentials and the Gemini key, which is more blast radius than a remediation
// that fires this rarely is worth. ScoreBatchStuckOpen is escalated instead,
// with the exact `server batch evict` command; see the system prompt in
// agent.go. Keeping it out of this list means arming it fails loudly at startup
// as an unknown action rather than being accepted and silently doing nothing.

// allActions is the ordering used in logs, metrics and the system prompt.
var allActions = []string{
	ActionRolloutRestart,
	ActionDeletePod,
	ActionScaleDeploy,
	ActionArgoSync,
}

// Config is the agent's runtime configuration, read from the environment. The
// opsagent runs as its own workload and does not share internal/config, which
// is shaped around the pipeline's needs.
type Config struct {
	// GeminiAPIKey authenticates to the Gemini API. The same key the scorer
	// uses: quotas are per-model, so the model below does not compete with
	// scoring for its free-tier budget.
	GeminiAPIKey string
	// Model is the Gemini model used for diagnosis.
	Model string
	// FallbackModel is tried once when Model returns 429. It should sit on a
	// larger per-model quota than Model, since it exists for the case where
	// Model's daily allowance is gone.
	FallbackModel string
	// DailyCallBudget caps model calls per day, enforced locally so we notice
	// the ceiling as a logged decision rather than as a 429 at 3am. Evidence is
	// still collected and escalated once it is spent.
	DailyCallBudget int

	// DryRun makes every action tool report what it would have done and change
	// nothing. This is the default: writes are opt-in, per action.
	DryRun bool
	// EnabledActions is the set of actions permitted to actually execute. An
	// action absent here still runs in dry-run mode, so the transcript shows
	// what the agent wanted to do without it happening.
	EnabledActions map[string]bool
	// WritableNamespaces bounds every action to the app namespaces. monitoring,
	// argocd and kube-system are readable but never writable.
	WritableNamespaces map[string]bool
	// MaxReplicas bounds scale_deployment so a scale-up cannot exhaust the node.
	MaxReplicas int
	// SyncableApps bounds argocd_sync by Application name. WritableNamespaces
	// cannot do this job: Argo Applications all live in the argocd namespace, so
	// a sync is not scoped by the namespace it ends up affecting.
	//
	// This must exclude "opsagent" and "root". The opsagent Application is
	// manual-sync precisely so a human decides when a change to the arming env
	// goes in; letting the agent sync itself would hand it the ability to arm
	// itself. "root" is the app-of-apps and syncing it reaches everything.
	SyncableApps map[string]bool

	// PromInCluster and PromCompose are the two Prometheus endpoints.
	PromInCluster string
	PromCompose   string
	// ArgoCDServer is the in-cluster Argo CD API base URL.
	ArgoCDServer string
	// ArgoCDToken authenticates to the Argo CD API. Empty disables Argo tools.
	ArgoCDToken string

	// GitHubToken authorizes issue creation for escalations.
	GitHubToken string
	// GitHubRepo is "owner/name" for escalation issues.
	GitHubRepo string

	// ListenAddr is where the Alertmanager webhook and /metrics are served.
	ListenAddr string
	// SweepSchedule is the cron spec for the periodic sweep.
	SweepSchedule string
}

// LoadConfig reads configuration from the environment, applying safe defaults.
// The defaults are deliberately conservative: dry-run on, no actions enabled.
func LoadConfig() (Config, error) {
	cfg := Config{
		GeminiAPIKey: os.Getenv("GEMINI_API_KEY"),
		// gemini-3.6-flash reasons noticeably better than the lite models and,
		// because one investigation costs exactly one request, its 20/day free
		// allowance is ample. The fallback has 500/day and catches the case
		// where it is not.
		Model:              getenv("OPSAGENT_MODEL", "gemini-3.6-flash"),
		FallbackModel:      getenv("OPSAGENT_FALLBACK_MODEL", "gemini-3.5-flash-lite"),
		DailyCallBudget:    getenvInt("OPSAGENT_DAILY_CALL_BUDGET", 15),
		DryRun:             getenvBool("OPSAGENT_DRY_RUN", true),
		EnabledActions:     parseSet(os.Getenv("OPSAGENT_ENABLED_ACTIONS")),
		WritableNamespaces: parseSet(getenv("OPSAGENT_WRITABLE_NAMESPACES", "jsa-prod,jsa-staging")),
		MaxReplicas:        getenvInt("OPSAGENT_MAX_REPLICAS", 3),
		SyncableApps:       parseSet(getenv("OPSAGENT_SYNCABLE_APPS", "jsa-prod,jsa-staging")),
		PromInCluster:      getenv("OPSAGENT_PROM_IN_CLUSTER", "http://kps-prometheus.monitoring.svc:9090"),
		PromCompose:        getenv("OPSAGENT_PROM_COMPOSE", "http://compose-prometheus.monitoring.svc:9090"),
		ArgoCDServer:       getenv("OPSAGENT_ARGOCD_SERVER", "http://argocd-server.argocd.svc"),
		ArgoCDToken:        os.Getenv("OPSAGENT_ARGOCD_TOKEN"),
		GitHubToken:        os.Getenv("OPSAGENT_GITHUB_TOKEN"),
		GitHubRepo:         getenv("OPSAGENT_GITHUB_REPO", "jakemercure28/job-search-automation-private"),
		ListenAddr:         getenv("OPSAGENT_LISTEN_ADDR", ":8091"),
		// Daily rather than the original 6-hourly. A clean sweep costs no model
		// call at all (see evidence.healthy), but a daily cadence keeps even the
		// worst case comfortably inside the free tier.
		SweepSchedule: getenv("OPSAGENT_SWEEP_SCHEDULE", "17 7 * * *"),
	}
	if cfg.GeminiAPIKey == "" {
		return cfg, fmt.Errorf("GEMINI_API_KEY is required")
	}
	if cfg.DailyCallBudget < 1 {
		return cfg, fmt.Errorf("OPSAGENT_DAILY_CALL_BUDGET must be at least 1, got %d", cfg.DailyCallBudget)
	}
	for name := range cfg.EnabledActions {
		if !isKnownAction(name) {
			return cfg, fmt.Errorf("OPSAGENT_ENABLED_ACTIONS contains unknown action %q (known: %s)",
				name, strings.Join(allActions, ", "))
		}
	}
	// Refuse to start rather than quietly accept a config that would let the
	// agent sync its own Application (self-arming) or the app-of-apps.
	for _, forbidden := range []string{"opsagent", "root"} {
		if cfg.SyncableApps[forbidden] {
			return cfg, fmt.Errorf("OPSAGENT_SYNCABLE_APPS must not contain %q", forbidden)
		}
	}
	return cfg, nil
}

// EnabledActionNames lists the actions permitted to actually execute, for
// startup logging. Everything else runs in dry-run mode.
func (c Config) EnabledActionNames() []string {
	return sortedKeys(c.EnabledActions)
}

// WritableNamespaceNames lists the namespaces actions may target.
func (c Config) WritableNamespaceNames() []string {
	return sortedKeys(c.WritableNamespaces)
}

func isKnownAction(name string) bool {
	for _, a := range allActions {
		if a == name {
			return true
		}
	}
	return false
}

// gateDecision is the outcome of checking one proposed action.
type gateDecision struct {
	// Allowed reports whether the action may actually execute.
	Allowed bool
	// Reason explains a denial, and is returned to the model as the tool result
	// so it can adapt (usually by escalating instead of retrying).
	Reason string
	// DryRun reports that the action was understood and permitted in principle
	// but must not touch the cluster.
	DryRun bool
}

// checkAction is the single decision point for every state-changing tool. It is
// enforced in Go rather than by prompting: the model cannot argue its way past
// a boolean. RBAC on the ServiceAccount is the independent second layer, so a
// bug here still cannot exceed the Role.
func (c Config) checkAction(action, namespace string) gateDecision {
	if !isKnownAction(action) {
		return gateDecision{Reason: fmt.Sprintf("%q is not an allowlisted action", action)}
	}
	// argocd_sync is the one action with no namespace, because Argo Applications
	// are not scoped by the namespace they deploy into; checkArgoApp bounds it
	// instead. For everything else an empty namespace must be rejected rather
	// than waved through, or passing "" would skip the allowlist below. Nothing
	// currently does that, and the k8s paths url.PathEscape their inputs, but a
	// gate should not depend on the API server's URL parsing to stay closed.
	if namespace == "" && action != ActionArgoSync {
		return gateDecision{Reason: fmt.Sprintf(
			"%s requires an explicit namespace (writable: %s)",
			action, strings.Join(sortedKeys(c.WritableNamespaces), ", "))}
	}
	if namespace != "" && !c.WritableNamespaces[namespace] {
		return gateDecision{Reason: fmt.Sprintf(
			"namespace %q is read-only for this agent (writable: %s)",
			namespace, strings.Join(sortedKeys(c.WritableNamespaces), ", "))}
	}
	if c.DryRun {
		return gateDecision{Allowed: true, DryRun: true, Reason: "OPSAGENT_DRY_RUN is set"}
	}
	if !c.EnabledActions[action] {
		return gateDecision{Allowed: true, DryRun: true, Reason: fmt.Sprintf(
			"action %q is not in OPSAGENT_ENABLED_ACTIONS", action)}
	}
	return gateDecision{Allowed: true}
}

// checkArgoApp bounds argocd_sync by Application name. Argo syncs are not
// covered by checkAction's namespace scope, because every Application object
// lives in the argocd namespace regardless of what it deploys, and they are not
// covered by RBAC either, since the sync goes through the Argo API with a token
// rather than through the ServiceAccount. This is the only thing bounding them.
func (c Config) checkArgoApp(app string) error {
	if !c.SyncableApps[app] {
		return fmt.Errorf("application %q is not syncable by this agent (syncable: %s)",
			app, strings.Join(sortedKeys(c.SyncableApps), ", "))
	}
	return nil
}

// checkReplicas bounds scale_deployment. Scaling to zero is permitted (it is a
// legitimate way to stop a pod that is thrashing) but the ceiling is fixed.
func (c Config) checkReplicas(n int) error {
	if n < 0 {
		return fmt.Errorf("replicas must not be negative")
	}
	if n > c.MaxReplicas {
		return fmt.Errorf("replicas %d exceeds OPSAGENT_MAX_REPLICAS (%d)", n, c.MaxReplicas)
	}
	return nil
}

// --- env helpers ---

func getenv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func getenvBool(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

// parseSet splits a comma-separated env value into a lookup set.
func parseSet(v string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out[p] = true
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
