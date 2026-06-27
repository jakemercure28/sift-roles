package dashboard

import (
	"net/http"
	"strings"
)

// This file ports lib/routes/ask.js: the in-app help assistant. It answers
// questions about the app, grounded only on APP_KNOWLEDGE, via Gemini.

const askMaxQuestionLen = 500
const askMaxTokens = 400

// appKnowledge is the verbatim grounding text from ask.js (APP_KNOWLEDGE).
const appKnowledge = `This is a private, local job search dashboard. It runs on your own machine. It scrapes job boards, scores each listing from 1 to 10 with Google Gemini against your resume and preferences, and gives you a dashboard to review and track jobs. Nothing is sent anywhere except the Gemini scoring calls.

Applying is always manual. The app never submits applications for you and never marks a job as applied on its own. A job becomes "applied" only when you set it yourself in the dashboard after you submit on the company site.

Daily flow:
1. Open the dashboard and review the pending jobs, highest scores first.
2. Open a job you like and apply on the company's own site.
3. Come back and set the job's status to Applied using the dropdown on the job.
4. As things progress, move it to Interviewing, then Rejected, Ghosted, or Offer.

What the scores mean: each job gets a 1 to 10 match score from Gemini. Higher is a better fit for you. Green is a strong match, blue is good, yellow is moderate, orange is borderline, and very low scores are hidden by default. You can change the minimum score filter to show more or fewer jobs.

Statuses you can set on a job: Pending (found, not applied yet), Applied (you submitted it), Interviewing (a round is in progress), Rejected, Ghosted (applied but no reply after a few weeks), and Offer. There is also Archived (you hid it) and Closed (the job posting went dead).

Where jobs come from: the app pulls from company career pages and from several remote job boards. You tell it which companies and search terms to track during setup.

Setup basics: you need a free Google Gemini API key from aistudio.google.com/apikey. Put it in your settings, then the app can score jobs. It is tuned to stay inside Gemini's free tier.

Common questions:
- "Why won't a job mark itself applied?" Because applying is always manual on purpose. You set the status after you apply.
- "Why did a job disappear?" It may be below your minimum score filter, archived, or its posting closed.
- "What is ghosted?" A job you applied to that got no response after a few weeks. The app can mark these automatically.
- "Does it apply for me?" No. It only finds, scores, and tracks. You apply yourself.`

func buildAskPrompt(question string) string {
	return `You are a friendly help assistant built into a personal job-search dashboard app. Answer the user's question using only the app knowledge below. Keep the answer short, plain, and easy for a non-technical person to follow. If the question is not about this app, say politely that you can only help with questions about the job-search dashboard. Never use dashes as sentence connectors; use commas or periods instead.

APP KNOWLEDGE:
` + appKnowledge + `

USER QUESTION: ` + question + `

ANSWER:`
}

// askResp mirrors the JSON payload of /api/ask: { answer, error? }.
type askResp struct {
	Answer *string `json:"answer"`
	Error  string  `json:"error,omitempty"`
}

// handleAsk ports resolveAsk + handleAsk.
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Question string `json:"question"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	question := strings.TrimSpace(body.Question)
	if question == "" {
		jsonError(w, http.StatusBadRequest, "A question is required.")
		return
	}
	if len(question) > askMaxQuestionLen {
		jsonError(w, http.StatusBadRequest, "That question is too long. Keep it under 500 characters.")
		return
	}

	// No Gemini agent (or no key) -> the client shows a "set your key" hint.
	sc := s.scorer()
	if sc == nil {
		writeJSON(w, http.StatusOK, askResp{Error: "no-key"})
		return
	}

	answer, err := sc.Ask(r.Context(), buildAskPrompt(question), askMaxTokens)
	if err != nil {
		if strings.Contains(err.Error(), "GEMINI_API_KEY") {
			writeJSON(w, http.StatusOK, askResp{Error: "no-key"})
			return
		}
		writeJSON(w, http.StatusOK, askResp{Error: "unavailable"})
		return
	}
	if answer == "" {
		writeJSON(w, http.StatusOK, askResp{Error: "empty"})
		return
	}
	writeJSON(w, http.StatusOK, askResp{Answer: &answer})
}
