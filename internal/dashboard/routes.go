package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"job-search-automation/internal/scorer"
)

// maxBodyBytes caps request bodies, matching the 1 MiB limit in
// lib/routes/_helpers.js parseBody.
const maxBodyBytes = 1 << 20

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// errorEnvelope is the canonical shape every JSON endpoint uses to report a
// failure. ok is always false; the other fields are populated as needed. A
// struct (not a map) keeps the JSON field order deterministic: ok first.
type errorEnvelope struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
	Busy  bool   `json:"busy,omitempty"`
}

// jsonError writes the canonical error envelope: {"ok": false, "error": msg}.
// Every JSON endpoint reports failures through this (or its siblings) so the
// frontend can rely on a single shape.
func jsonError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorEnvelope{Error: msg})
}

// jsonErrorCode is jsonError plus a machine-readable code the frontend can
// branch on (e.g. "no_key").
func jsonErrorCode(w http.ResponseWriter, status int, msg, code string) {
	writeJSON(w, status, errorEnvelope{Error: msg, Code: code})
}

// jsonBusy reports that the requested operation is already in flight (409).
func jsonBusy(w http.ResponseWriter) {
	writeJSON(w, http.StatusConflict, errorEnvelope{Busy: true})
}

// decodeBody reads and JSON-decodes a request body into dst (1 MiB cap).
func decodeBody(r *http.Request, dst any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, dst)
}

// GET /api/scraper-heartbeat -> { heartbeat: <stored json or null> }
func (s *Server) handleScraperHeartbeat(w http.ResponseWriter, _ *http.Request) {
	raw, err := s.repo.ScraperHeartbeatRaw()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	hb := json.RawMessage("null")
	if raw != "" {
		hb = json.RawMessage(raw)
	}
	writeJSON(w, http.StatusOK, map[string]any{"heartbeat": hb})
}

// GET /api/scoring-progress
func (s *Server) handleScoringProgress(w http.ResponseWriter, _ *http.Request) {
	st, err := s.repo.ScoringStats()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	active := false
	if st.LatestScoreAt != "" {
		// Stored timestamps are UTC without a zone; append Z to parse as UTC.
		if t, perr := time.Parse(time.RFC3339, strings.Replace(st.LatestScoreAt, " ", "T", 1)+"Z"); perr == nil {
			active = time.Since(t) < 120*time.Second
		}
	}

	etaSeconds := 0
	if st.Unscored > 0 {
		etaSeconds = int(math.Ceil(float64(st.Unscored) * s.rateDelay.Seconds()))
	}

	payload := map[string]any{
		"active":          active,
		"scored":          st.Scored,
		"unscored":        st.Unscored,
		"total":           st.Scored + st.Unscored,
		"etaSeconds":      etaSeconds,
		"latestScoreAt":   nullableString(st.LatestScoreAt),
		"newJobs24h":      st.NewJobs24h,
		"newCompanies24h": st.NewCompanies24h,
		"lastScrapeAt":    nil,
	}
	// When fresh jobs arrive is governed by the scrape cron; surface the next run
	// so the browser can show "next scrape <time>" instead of provider internals.
	if s.scrapeSchedule != "" {
		if sched, perr := cron.ParseStandard(s.scrapeSchedule); perr == nil {
			payload["nextScrapeAt"] = sched.Next(time.Now()).Format(time.RFC3339)
		}
	}
	if hb, ok, _ := s.repo.ScraperHeartbeat(); ok {
		payload["lastScrapeAt"] = nullableString(hb.LastSuccessAt)
		payload["lastScrapeStatus"] = hb.Status
		payload["lastScrapeInserted"] = hb.Inserted
	}
	if dr, ok, _ := s.repo.DiscoveryReport(); ok {
		payload["discoveryAt"] = dr.At
		payload["discoveryAdded"] = dr.Added
	}
	writeJSON(w, http.StatusOK, payload)
}

// GET /job-description?id=
func (s *Server) handleJobDescription(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		jsonError(w, http.StatusBadRequest, "id required")
		return
	}
	job, found, err := s.repo.JobDescription(id)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		jsonError(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// GET /company-notes?company=
func (s *Server) handleGetCompanyNotes(w http.ResponseWriter, r *http.Request) {
	company := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("company")))
	if company == "" {
		writeJSON(w, http.StatusOK, map[string]string{"tags": "", "notes": ""})
		return
	}
	tags, notes, found, err := s.repo.CompanyNotes(company)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, map[string]string{"tags": "", "notes": ""})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"tags":  serializeCompanyTags(tags),
		"notes": notes,
	})
}

// POST /company-notes { company, tags, notes }
func (s *Server) handleSaveCompanyNotes(w http.ResponseWriter, r *http.Request) {
	var body struct{ Company, Tags, Notes string }
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	key := strings.ToLower(strings.TrimSpace(body.Company))
	if key == "" {
		jsonError(w, http.StatusBadRequest, "company required")
		return
	}
	if err := s.repo.SaveCompanyNotes(key, serializeCompanyTags(body.Tags), body.Notes); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// POST /archive { id }
func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	var body struct{ ID string }
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.repo.ArchiveJob(body.ID); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

var validPipelineValues = map[string]struct{}{
	"": {}, "applied": {}, "phone_screen": {}, "interview": {},
	"onsite": {}, "offer": {}, "closed": {}, "rejected": {}, "ghosted": {},
}

// POST /pipeline { id, value }
func (s *Server) handlePipeline(w http.ResponseWriter, r *http.Request) {
	var body struct{ ID, Value string }
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, ok := validPipelineValues[body.Value]; !ok {
		jsonError(w, http.StatusBadRequest, "bad pipeline value")
		return
	}
	if err := s.repo.SetPipelineStage(body.ID, body.Value); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// On apply/reject, fill rejection reasoning in the background (best effort),
	// mirroring handlePipeline's fire-and-forget scoreRejectionLikelihood.
	// scoreRejectionAsync resolves the tenant's scorer and no-ops if none.
	if body.Value == "applied" || body.Value == "rejected" {
		s.scoreRejectionAsync(body.ID)
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// scoreRejectionAsync runs the rejection-likelihood call detached from the
// request, with its own timeout. Failures are swallowed (non-critical).
func (s *Server) scoreRejectionAsync(id string) {
	sc := s.scorer()
	if sc == nil {
		return
	}
	job, found, err := s.repo.JobBrief(id)
	if err != nil || !found {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		text, err := sc.ScoreRejection(ctx, scorer.Job{
			Title:       job.Title,
			Company:     job.Company,
			Location:    job.Location,
			Description: job.Description,
		})
		if err != nil {
			s.log.Warn("rejection scoring failed", "id", id, "error", err)
			return
		}
		if err := s.repo.SetRejectionReasoning(id, text); err != nil {
			s.log.Warn("store rejection reasoning failed", "id", id, "error", err)
		}
	}()
}

// GET /api/location-prefs -> { prefs, metros }
func (s *Server) handleGetLocationPrefs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Prefs  LocationPrefs   `json:"prefs"`
		Metros json.RawMessage `json:"metros"`
	}{loadPrefs(s.dataDir), MetrosRaw()})
}

// POST /api/location-prefs { metros, includeUnknown, remoteOnly } -> { ok, prefs }
func (s *Server) handlePostLocationPrefs(w http.ResponseWriter, r *http.Request) {
	var raw rawPrefs
	if err := decodeBody(r, &raw); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := savePrefs(s.dataDir, raw)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, struct {
		OK    bool          `json:"ok"`
		Prefs LocationPrefs `json:"prefs"`
	}{true, saved})
}

// nullableString returns nil for an empty string so the JSON field is null,
// matching the Node handlers that emit null for absent timestamps.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
