package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type setupStatusResponse struct {
	ResumeContent    string `json:"resumeContent"`
	ContextContent   string `json:"contextContent"`
	CompaniesContent string `json:"companiesContent"`
	HasKey           bool   `json:"hasKey"`
	// FirstRun reports whether this tenant still needs onboarding. It is the
	// authoritative, tenant-scoped signal the browser uses to decide whether to
	// show the setup wizard: GET / is public and always renders the base tenant,
	// so the server can't bake a per-user first-run flag into the page HTML.
	FirstRun bool `json:"firstRun"`
}

func readTextFileSafe(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// tenantOnboarded reports whether the tenant whose profile lives in dir has
// completed setup. Resume is the only required markdown profile input, but a
// lone uploaded resume is still not enough: the setup wizard writes resume.md
// before the final profile step. The marker is the authoritative completion
// signal for new tenants, and the companies.json fallback keeps older pre-marker
// profiles working after context.md stopped being part of the automated pipeline.
func tenantOnboarded(dir string) bool {
	if strings.TrimSpace(readTextFileSafe(filepath.Join(dir, "resume.md"))) == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, ".onboarded")); err == nil {
		return true
	}
	return setupCompaniesHasSignal(filepath.Join(dir, "companies.json"))
}

func setupCompaniesHasSignal(path string) bool {
	raw := readTextFileSafe(path)
	if strings.TrimSpace(raw) == "" {
		return false
	}
	var cfg struct {
		SearchTerms []string `json:"SEARCH_TERMS"`
		Greenhouse  []string `json:"GREENHOUSE_COMPANIES"`
		Lever       []string `json:"LEVER_COMPANIES"`
		Workable    []string `json:"WORKABLE_COMPANIES"`
		Ashby       []string `json:"ASHBY_COMPANIES"`
		Workday     []any    `json:"WORKDAY_COMPANIES"`
		Rippling    []string `json:"RIPPLING_COMPANIES"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return true
	}
	return len(cfg.SearchTerms) > 0 || len(cfg.Greenhouse) > 0 || len(cfg.Lever) > 0 ||
		len(cfg.Workable) > 0 || len(cfg.Ashby) > 0 || len(cfg.Workday) > 0 ||
		len(cfg.Rippling) > 0
}

func (s *Server) handleSetupStatus(w http.ResponseWriter, _ *http.Request) {
	out := setupStatusResponse{
		ResumeContent:    readTextFileSafe(filepath.Join(s.dataDir, "resume.md")),
		ContextContent:   readTextFileSafe(filepath.Join(s.dataDir, "context.md")),
		CompaniesContent: readTextFileSafe(filepath.Join(s.dataDir, "companies.json")),
		HasKey:           strings.TrimSpace(os.Getenv("GEMINI_API_KEY")) != "",
		FirstRun:         !tenantOnboarded(s.dataDir),
	}
	writeJSON(w, http.StatusOK, out)
}

type setupSettingSpec struct {
	Key   string
	Label string
	Type  string
	Min   *float64
	Max   *float64
	Def   any
	Group string
	Hint  string
}

// setupAllowedSettings are the only env keys the Settings UI may read or write.
// Scrape/scoring/discovery knobs are deliberately absent: they are process-wide
// (one .env value for every tenant), so they stay operator-only env config and
// run on their config defaults. Only the per-deployment Gmail integration is
// editable here.
var setupAllowedSettings = []setupSettingSpec{
	{Key: "GMAIL_EMAIL", Label: "Gmail address", Type: "string", Group: "integrations", Hint: "For rejection-email sync (optional)."},
	{Key: "GMAIL_APP_PASSWORD", Label: "Gmail app password", Type: "secret", Group: "integrations", Hint: "Google app password. Stored write-only; never shown back."},
	{Key: "REJECTION_EMAIL_SYNC_DISABLED", Label: "Pause rejection-email sync", Type: "bool", Group: "integrations", Hint: "When on, stop checking Gmail for rejection emails. Your credentials stay saved."},
}

type setupSettingsResponse struct {
	OK       bool             `json:"ok"`
	Settings []map[string]any `json:"settings"`
}

func (s *Server) handleSettingsEnvGet(w http.ResponseWriter, _ *http.Request) {
	settings := make([]map[string]any, 0, len(setupAllowedSettings))
	for _, spec := range setupAllowedSettings {
		current := os.Getenv(spec.Key)
		item := map[string]any{
			"key":   spec.Key,
			"label": spec.Label,
			"type":  spec.Type,
			"group": spec.Group,
			"hint":  spec.Hint,
		}
		switch spec.Type {
		case "secret":
			item["set"] = strings.TrimSpace(current) != ""
		case "bool":
			item["checked"] = strings.ToLower(current) == "true"
		default:
			if spec.Min != nil {
				item["min"] = *spec.Min
			}
			if spec.Max != nil {
				item["max"] = *spec.Max
			}
			if spec.Def != nil {
				item["def"] = spec.Def
			}
			item["value"] = current
		}
		settings = append(settings, item)
	}
	writeJSON(w, http.StatusOK, setupSettingsResponse{OK: true, Settings: settings})
}

type careerExperienceFile struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type careerGetResponse struct {
	CareerDetail string                 `json:"careerDetail"`
	Applicant    string                 `json:"applicant"`
	Voice        string                 `json:"voice"`
	Experience   []careerExperienceFile `json:"experience"`
}

func (s *Server) handleCareerGet(w http.ResponseWriter, _ *http.Request) {
	experienceDir := filepath.Join(s.dataDir, "experience")
	experience := []careerExperienceFile{}
	if entries, err := os.ReadDir(experienceDir); err == nil {
		names := []string{}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, ".") {
				continue
			}
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			experience = append(experience, careerExperienceFile{
				Name:    name,
				Content: readTextFileSafe(filepath.Join(experienceDir, name)),
			})
		}
	}
	peopleDir := s.peopleDir()
	out := careerGetResponse{
		CareerDetail: readTextFileSafe(filepath.Join(s.dataDir, "career-detail.md")),
		Applicant:    readTextFileSafe(filepath.Join(peopleDir, "applicant.md")),
		Voice:        readTextFileSafe(filepath.Join(peopleDir, "voice.md")),
		Experience:   experience,
	}
	writeJSON(w, http.StatusOK, out)
}
