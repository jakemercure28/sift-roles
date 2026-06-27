package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"job-search-automation/internal/db"
	"job-search-automation/internal/scorer"
)

func readJSONBody(r *http.Request) (map[string]any, error) {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		return map[string]any{}, nil
	}
	return body, nil
}

func updateEnvLine(key, value, envPath string) error {
	content := ""
	if data, err := os.ReadFile(envPath); err == nil {
		content = string(data)
	}
	line := key + "=" + strings.ReplaceAll(value, "\\", "\\\\")
	re := regexp.MustCompile("(?m)^" + regexp.QuoteMeta(key) + "=.*$")
	if re.MatchString(content) {
		content = re.ReplaceAllString(content, line)
	} else {
		if content != "" && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		content += line + "\n"
	}
	return os.WriteFile(envPath, []byte(content), 0o644)
}

func stringField(body map[string]any, key string) string {
	if v, ok := body[key]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

func (s *Server) envPath() string {
	root, err := os.Getwd()
	if err != nil {
		root = "."
	}
	return filepath.Join(root, ".env")
}

// buildSetupCompaniesJSON renders the active profile's companies.json. Company
// lists are always written empty here (discovery fills suggested-companies.json
// separately); the wizard only sets MAX_AGE_DAYS + SEARCH_TERMS.
func buildSetupCompaniesJSON(searchTerms string, maxAgeDays *int) string {
	maxAge := 20
	if maxAgeDays != nil {
		maxAge = *maxAgeDays
	}
	terms := []string{}
	for _, t := range strings.Split(searchTerms, "\n") {
		t = strings.TrimSpace(t)
		if t != "" {
			terms = append(terms, t)
		}
	}
	profile := struct {
		MaxAgeDays  int      `json:"MAX_AGE_DAYS"`
		SearchTerms []string `json:"SEARCH_TERMS"`
		Greenhouse  []string `json:"GREENHOUSE_COMPANIES"`
		Lever       []string `json:"LEVER_COMPANIES"`
		Workable    []string `json:"WORKABLE_COMPANIES"`
		Ashby       []string `json:"ASHBY_COMPANIES"`
		Workday     []any    `json:"WORKDAY_COMPANIES"`
		Wellfound   []string `json:"WELLFOUND_ROLES"`
		Rippling    []string `json:"RIPPLING_COMPANIES"`
	}{
		MaxAgeDays:  maxAge,
		SearchTerms: terms,
		Greenhouse:  []string{},
		Lever:       []string{},
		Workable:    []string{},
		Ashby:       []string{},
		Workday:     []any{},
		Wellfound:   []string{},
		Rippling:    []string{},
	}
	raw, _ := json.MarshalIndent(profile, "", "  ")
	return string(raw) + "\n"
}

var setupResumeTermLexicon = []string{
	"software engineer", "devops", "sre", "platform engineer", "data engineer", "data analyst",
	"product manager", "project manager", "program manager", "marketing", "sales", "customer success",
	"operations", "recruiter", "human resources", "accountant", "financial analyst", "nurse", "teacher",
	"merchandising", "merchandiser", "buyer", "planner", "retail", "store manager", "visual merchandising",
}

var setupRoleLineRe = regexp.MustCompile(`(?i)\b(engineer|developer|manager|director|analyst|associate|coordinator|specialist|designer|architect|consultant|administrator|operator|technician|nurse|teacher|accountant|recruiter|merchandis(?:er|ing)|buyer|planner|retail|sales|marketing|operations)\b`)

func deriveSetupSearchTermsFromResume(resume string) []string {
	resume = strings.TrimSpace(resume)
	if resume == "" {
		return nil
	}
	seen := map[string]bool{}
	terms := []string{}
	add := func(term string) {
		term = normalizeSetupSearchTerm(term)
		if len(term) < 3 || seen[term] {
			return
		}
		seen[term] = true
		terms = append(terms, term)
	}
	for _, line := range strings.Split(resume, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "-* "))
		if line == "" || len(line) > 90 || strings.HasSuffix(line, ":") || strings.HasSuffix(line, ".") {
			continue
		}
		if !setupRoleLineRe.MatchString(line) {
			continue
		}
		for _, sep := range []string{" | ", " - ", " -- ", " at ", " @ ", ","} {
			if idx := strings.Index(strings.ToLower(line), sep); idx > 0 {
				line = line[:idx]
				break
			}
		}
		add(line)
		if len(terms) >= 8 {
			return terms
		}
	}
	lowerResume := strings.ToLower(resume)
	for _, term := range setupResumeTermLexicon {
		if strings.Contains(lowerResume, term) {
			add(term)
			if len(terms) >= 8 {
				break
			}
		}
	}
	return terms
}

func normalizeSetupSearchTerm(term string) string {
	term = strings.ToLower(strings.TrimSpace(term))
	term = regexp.MustCompile(`\([^)]*\)`).ReplaceAllString(term, "")
	term = regexp.MustCompile(`[^a-z0-9+.#/ -]+`).ReplaceAllString(term, " ")
	term = regexp.MustCompile(`\s+`).ReplaceAllString(term, " ")
	return strings.TrimSpace(term)
}

// extractPDFText sends a base64-encoded PDF to Gemini using the multimodal
// inlineData API and returns the extracted plain-text / markdown content.
func extractPDFText(ctx context.Context, apiKey, base64PDF string) (string, error) {
	type inlineData struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	}
	type part struct {
		Text       string      `json:"text,omitempty"`
		InlineData *inlineData `json:"inlineData,omitempty"`
	}
	type content struct {
		Parts []part `json:"parts"`
	}
	type genConfig struct {
		MaxOutputTokens int `json:"maxOutputTokens"`
	}
	type request struct {
		Contents         []content `json:"contents"`
		GenerationConfig genConfig `json:"generationConfig"`
	}
	reqBody, err := json.Marshal(request{
		Contents: []content{{Parts: []part{
			{InlineData: &inlineData{MimeType: "application/pdf", Data: base64PDF}},
			{Text: "Extract the full text of this resume as clean markdown. Include all sections: contact info, summary, experience, education, skills. Preserve structure. Return only the extracted text, no commentary."},
		}}},
		GenerationConfig: genConfig{MaxOutputTokens: 4096},
	})
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent", scorer.Model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("gemini decode: %w", err)
	}
	if result.Error.Message != "" {
		return "", errors.New(result.Error.Message)
	}
	if len(result.Candidates) == 0 || len(result.Candidates[0].Content.Parts) == 0 {
		return "", errors.New("empty response from Gemini")
	}
	return result.Candidates[0].Content.Parts[0].Text, nil
}

func (s *Server) handleSetupResume(w http.ResponseWriter, r *http.Request) {
	body, _ := readJSONBody(r)
	content := stringField(body, "content")
	format := stringField(body, "format")

	if format == "pdf" {
		apiKey := strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))
		if apiKey == "" {
			jsonErrorCode(w, http.StatusBadRequest, "A Gemini API key is required to read a PDF resume.", "no_key")
			return
		}
		extracted, err := extractPDFText(r.Context(), apiKey, content)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "PDF text extraction failed: "+err.Error())
			return
		}
		content = extracted
	}

	if strings.TrimSpace(content) != "" {
		if err := os.WriteFile(filepath.Join(s.dataDir, "resume.md"), []byte(content), 0o644); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSetupProfile(w http.ResponseWriter, r *http.Request) {
	body, _ := readJSONBody(r)
	resume := readTextFileSafe(filepath.Join(s.dataDir, "resume.md"))
	if strings.TrimSpace(resume) == "" {
		jsonError(w, http.StatusBadRequest, "Upload a resume before completing setup.")
		return
	}
	termSource := stringField(body, "searchTerms")
	if strings.TrimSpace(termSource) == "" {
		termSource = stringField(body, "titles")
	}
	seen := map[string]bool{}
	terms := []string{}
	for _, t := range strings.Split(termSource, "\n") {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" && !seen[t] {
			seen[t] = true
			terms = append(terms, t)
		}
	}
	if len(terms) == 0 {
		terms = deriveSetupSearchTermsFromResume(resume)
	}
	if len(terms) == 0 {
		jsonError(w, http.StatusBadRequest, "Could not determine search terms from the resume. Add at least one target title or search term.")
		return
	}
	if err := os.WriteFile(filepath.Join(s.dataDir, "companies.json"), []byte(buildSetupCompaniesJSON(strings.Join(terms, "\n"), nil)), 0o644); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.WriteFile(filepath.Join(s.dataDir, ".onboarded"), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSetupCompanies(w http.ResponseWriter, r *http.Request) {
	body, _ := readJSONBody(r)
	var maxAge *int
	if raw := strings.TrimSpace(stringField(body, "maxAgeDays")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 365 {
			jsonError(w, http.StatusBadRequest, "Job freshness must be a whole number of days between 1 and 365")
			return
		}
		maxAge = &n
	}
	if err := os.WriteFile(filepath.Join(s.dataDir, "companies.json"), []byte(buildSetupCompaniesJSON(stringField(body, "searchTerms"), maxAge)), 0o644); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleDevResetOnboarding deletes the .onboarded marker (and optionally the
// full profile) so the wizard reappears on next load. Only registered when
// APP_ENV=development or DEBUG=true.
func (s *Server) handleDevResetOnboarding(w http.ResponseWriter, r *http.Request) {
	files := []string{".onboarded"}
	if r.URL.Query().Get("full") == "true" {
		files = append(files, "resume.md", "context.md", "companies.json")
	}
	for _, f := range files {
		_ = os.Remove(filepath.Join(s.dataDir, f))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reset": files})
}

// handleSetupAPIKey persists the Gemini key from the setup wizard.
//
// Self-host (SQLite): single tenant, the key lives in .env and process env.
//
// Hosted (Postgres): every tenant scores on the shared host key configured at
// deploy via .env. The wizard must never write the shared root .env or
// process-global os.Setenv, since a single signup could overwrite or wipe the
// host key for everyone, so this is a no-op in hosted mode.
func (s *Server) handleSetupAPIKey(w http.ResponseWriter, r *http.Request) {
	body, _ := readJSONBody(r)
	key := strings.TrimSpace(stringField(body, "key"))

	if s.repo.DBType() == db.Postgres {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "usageReset": false, "rescoreStarted": false})
		return
	}

	usageReset, rescoreStarted := false, false
	if key != "" {
		changed := key != os.Getenv("GEMINI_API_KEY")
		if err := updateEnvLine("GEMINI_API_KEY", key, s.envPath()); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		os.Setenv("GEMINI_API_KEY", key)
		if changed {
			usageReset = s.repo.ResetAPIUsageToday() == nil
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "usageReset": usageReset, "rescoreStarted": rescoreStarted})
}

func (s *Server) handleSetupTestKey(w http.ResponseWriter, r *http.Request) {
	body, _ := readJSONBody(r)
	key := strings.TrimSpace(stringField(body, "key"))
	if key == "" {
		jsonError(w, http.StatusBadRequest, "No key provided")
		return
	}
	if err := validateGeminiKey(r.Context(), key); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func validateGeminiKey(ctx context.Context, key string) error {
	body := "{\"contents\":[{\"parts\":[{\"text\":\"hi\"}]}]}"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://generativelanguage.googleapis.com/v1beta/models/"+scorer.Model+":generateContent?key="+key, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 || resp.StatusCode == 429 {
		return nil
	}
	if resp.StatusCode == 400 || resp.StatusCode == 401 || resp.StatusCode == 403 {
		return errors.New("Key rejected by Gemini (invalid or expired)")
	}
	return nil
}

func (s *Server) handleExtractProfile(w http.ResponseWriter, r *http.Request) {
	resume := readTextFileSafe(filepath.Join(s.dataDir, "resume.md"))
	sc := s.scorer()
	// No resume yet is an expected, non-error state (the wizard simply has nothing
	// to auto-fill from); return empty without flagging an error.
	if strings.TrimSpace(resume) == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "titles": "", "stack": ""})
		return
	}
	// No scorer means no Gemini key is resolvable (no host key and no tenant key).
	// Surface it instead of silently returning empty, which read to the user as
	// "couldn't determine anything in the resume" with no way to tell why.
	if sc == nil {
		s.log.Warn("extract-profile: no scorer available (missing Gemini key)")
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "titles": "", "stack": "",
			"error": "AI scoring is not configured yet (no Gemini key). Enter your targets manually below to continue."})
		return
	}
	prompt := "Extract job search preferences from this resume and return ONLY a JSON object with exactly these fields (no markdown, no explanation):\n" +
		`{"titles":["list of target job titles"],"stack":["list of key technologies"],"industry":"primary industry","salary":null}` +
		"\n\nResume:\n" + resume
	raw, err := sc.Ask(r.Context(), prompt, 512)
	if err != nil {
		s.log.Warn("extract-profile: Gemini call failed", "error", err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "titles": "", "stack": "",
			"error": "Could not read your resume automatically (the AI service errored). Enter your targets manually below to continue."})
		return
	}
	var parsed struct {
		Titles      []string `json:"titles"`
		SearchTerms []string `json:"searchTerms"`
		Stack       []string `json:"stack"`
		Salary      any      `json:"salary"`
		Industry    string   `json:"industry"`
	}
	_ = json.Unmarshal([]byte(strings.TrimSpace(codeFenceJSONRe.ReplaceAllString(raw, ""))), &parsed)
	terms := []string{}
	seen := map[string]bool{}
	source := parsed.SearchTerms
	if len(source) == 0 {
		source = parsed.Titles
	}
	for _, t := range source {
		t = strings.ToLower(strings.TrimSpace(t))
		if len(t) >= 3 && !seen[t] {
			seen[t] = true
			terms = append(terms, t)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "titles": strings.Join(parsed.Titles, "\n"), "searchTerms": strings.Join(terms, "\n"), "stack": strings.Join(parsed.Stack, "\n"), "salary": parsed.Salary, "industry": parsed.Industry})
}

func (s *Server) handleSetupRunRefresh(w http.ResponseWriter, r *http.Request) {
	missing := []string{}
	for _, name := range []string{"resume.md", "companies.json"} {
		if strings.TrimSpace(readTextFileSafe(filepath.Join(s.dataDir, name))) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		jsonError(w, http.StatusBadRequest, "Profile incomplete, missing: "+strings.Join(missing, ", "))
		return
	}
	// Actually kick off the first scrape through the same trigger the "Scrape
	// now" button uses. Previously this returned a fabricated runId without
	// starting anything, so the wizard claimed a search was running while the
	// user sat on an empty Pending tab.
	busy, err := s.triggerScrape(r.Context())
	if err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	runID := "wizard-" + strconv.FormatInt(time.Now().UnixMilli(), 36)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "runId": runID, "busy": busy})
}

func (s *Server) handleSettingsEnvPost(w http.ResponseWriter, r *http.Request) {
	body, _ := readJSONBody(r)
	raw, ok := body["settings"].(map[string]any)
	if !ok {
		raw = map[string]any{}
	}
	allowed := map[string]setupSettingSpec{}
	for _, spec := range setupAllowedSettings {
		allowed[spec.Key] = spec
	}
	writes := map[string]string{}
	for key, value := range raw {
		spec, ok := allowed[key]
		if !ok {
			jsonError(w, http.StatusBadRequest, "Not allowed: "+key)
			return
		}
		val, skip, err := validateSetupSetting(spec, value)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !skip {
			writes[key] = val
		}
	}
	keys := []string{}
	for key, value := range writes {
		if err := updateEnvLine(key, value, s.envPath()); err != nil {
			jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		os.Setenv(key, value)
		keys = append(keys, key)
	}
	sort.Strings(keys)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved": keys})
}

func validateSetupSetting(spec setupSettingSpec, raw any) (string, bool, error) {
	if spec.Type == "secret" {
		str := strings.TrimSpace(fmt.Sprint(raw))
		return str, str == "", nil
	}
	if spec.Type == "bool" {
		s := strings.ToLower(strings.TrimSpace(fmt.Sprint(raw)))
		if raw == true || s == "true" || s == "1" || s == "on" || s == "yes" {
			return "true", false, nil
		}
		return "", false, nil
	}
	if spec.Type == "string" {
		return strings.TrimSpace(fmt.Sprint(raw)), false, nil
	}
	str := strings.TrimSpace(fmt.Sprint(raw))
	if str == "" {
		return "", false, fmt.Errorf("%s cannot be empty", spec.Key)
	}
	num, err := strconv.ParseFloat(str, 64)
	if err != nil {
		return "", false, fmt.Errorf("%s must be a number", spec.Key)
	}
	if spec.Type == "int" && num != float64(int(num)) {
		return "", false, fmt.Errorf("%s must be a whole number", spec.Key)
	}
	if spec.Min != nil && num < *spec.Min {
		return "", false, fmt.Errorf("%s must be >= %v", spec.Key, *spec.Min)
	}
	if spec.Max != nil && num > *spec.Max {
		return "", false, fmt.Errorf("%s must be <= %v", spec.Key, *spec.Max)
	}
	if spec.Type == "int" {
		return strconv.Itoa(int(num)), false, nil
	}
	return strconv.FormatFloat(num, 'f', -1, 64), false, nil
}

func (s *Server) handleCareerSave(w http.ResponseWriter, r *http.Request) {
	body, _ := readJSONBody(r)
	peopleDir := s.peopleDir()
	targets := map[string]string{"careerDetail": filepath.Join(s.dataDir, "career-detail.md"), "applicant": filepath.Join(peopleDir, "applicant.md"), "voice": filepath.Join(peopleDir, "voice.md")}
	written := []string{}
	for field, target := range targets {
		if val, ok := body[field].(string); ok {
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			if err := os.WriteFile(target, []byte(val), 0o644); err != nil {
				jsonError(w, http.StatusInternalServerError, err.Error())
				return
			}
			written = append(written, field)
		}
	}
	sort.Strings(written)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "written": written})
}

var safeExperienceRe = regexp.MustCompile("^[a-z0-9][a-z0-9-_]*\\.md$")

func safeExperienceNameGo(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !safeExperienceRe.MatchString(name) {
		return "", errors.New("Invalid experience file name (use lowercase letters, numbers, - or _, ending in .md)")
	}
	return name, nil
}

func (s *Server) handleExperienceSave(w http.ResponseWriter, r *http.Request) {
	body, _ := readJSONBody(r)
	name, err := safeExperienceNameGo(stringField(body, "name"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	dir := filepath.Join(s.dataDir, "experience")
	_ = os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(stringField(body, "content")), 0o644); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

func (s *Server) handleExperienceDelete(w http.ResponseWriter, r *http.Request) {
	body, _ := readJSONBody(r)
	name, err := safeExperienceNameGo(stringField(body, "name"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	_ = os.Remove(filepath.Join(s.dataDir, "experience", name))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

func (s *Server) handleCareerStructure(w http.ResponseWriter, r *http.Request) {
	body, _ := readJSONBody(r)
	notes := strings.TrimSpace(stringField(body, "raw"))
	if notes == "" {
		jsonError(w, http.StatusBadRequest, "Paste some notes to structure first.")
		return
	}
	sc := s.scorer()
	if sc == nil {
		jsonError(w, http.StatusBadGateway, "AI structuring failed. Try again.")
		return
	}
	raw, err := sc.Ask(r.Context(), "You are organizing a job seeker's raw career notes into a clean career detail markdown document.\n\nRaw notes:\n\n"+notes, 4096)
	if err != nil {
		jsonError(w, http.StatusBadGateway, "AI structuring failed. Try again.")
		return
	}
	text := strings.TrimSpace(regexp.MustCompile("(?is)^```(?:markdown|md)?\\s*|\\s*```$").ReplaceAllString(raw, ""))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "text": text})
}
