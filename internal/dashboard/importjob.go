package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"job-search-automation/internal/model"
	"job-search-automation/internal/scorer"
)

// JobScorer extends RejectionScorer with direct 1-10 scoring capability.
// scorer.Scorer satisfies it.
type JobScorer interface {
	RejectionScorer
	ScoreJob(ctx context.Context, job scorer.Job) (scorer.Result, error)
}

type importJobReq struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Company     string `json:"company"`
	URL         string `json:"url"`
	Location    string `json:"location"`
	PostedAt    string `json:"posted_at"`
	Description string `json:"description"`
}

type importJobResp struct {
	OK        bool   `json:"ok"`
	ID        string `json:"id"`
	Inserted  bool   `json:"inserted"`
	Score     *int   `json:"score,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
	Error     string `json:"error,omitempty"`
}

// handleImportJob handles POST /api/jobs — inserts a manually-added job (e.g.
// from /add-job skill) directly into the live DB and scores it inline.
func (s *Server) handleImportJob(w http.ResponseWriter, r *http.Request) {
	var req importJobReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, importJobResp{Error: "invalid JSON: " + err.Error()})
		return
	}
	if req.ID == "" || req.Title == "" || req.Company == "" {
		writeJSON(w, http.StatusBadRequest, importJobResp{Error: "id, title, and company are required"})
		return
	}

	lead := model.Lead{
		JobLead: model.JobLead{
			Title:            req.Title,
			Company:          req.Company,
			DirectApplyURL:   req.URL,
			ATSPlatformName:  "linkedin",
			ScrapedTimestamp: time.Now().UTC().Format(time.RFC3339),
			Location:         req.Location,
			PostedAt:         req.PostedAt,
			Description:      req.Description,
		},
		ID: req.ID,
	}

	inserted, err := s.repo.InsertScrapedLead(lead)
	if err != nil {
		s.log.Error("import job insert failed", "id", req.ID, "error", err)
		writeJSON(w, http.StatusInternalServerError, importJobResp{Error: err.Error()})
		return
	}

	jobID := s.repo.JobRowID(req.ID)
	resp := importJobResp{OK: true, ID: jobID, Inserted: inserted}

	// Score inline if the tenant's scorer supports it.
	if js, ok := s.scorer().(JobScorer); ok && js != nil {
		result, err := js.ScoreJob(r.Context(), scorer.Job{
			Title:       req.Title,
			Company:     req.Company,
			Location:    req.Location,
			Description: req.Description,
		})
		if err != nil {
			s.log.Warn("import job score failed", "id", req.ID, "error", err)
		} else if result.Score != nil {
			if err := s.repo.UpdateJobScore(jobID, *result.Score, result.Reasoning); err != nil {
				s.log.Warn("import job save score failed", "id", jobID, "error", err)
			} else {
				resp.Score = result.Score
				resp.Reasoning = result.Reasoning
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
