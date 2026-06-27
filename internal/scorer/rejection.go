package scorer

import "context"

// maxDescriptionLength mirrors MAX_DESCRIPTION_LENGTH in config/constants.js.
const maxDescriptionLength = 15000

const rejectionMaxTokens = 400

// Ask sends an arbitrary prompt to Gemini (used by the in-app help assistant).
// It does not read the resume profile.
func (s *Scorer) Ask(ctx context.Context, prompt string, maxTokens int) (string, error) {
	return s.client.CallGemini(ctx, prompt, maxTokens)
}

// ScoreRejection returns the likely reasons this application would be rejected,
// porting scoreRejectionLikelihood in scorer.js. It is best-effort: the dashboard
// calls it in the background after a pipeline transition.
func (s *Scorer) ScoreRejection(ctx context.Context, job Job) (string, error) {
	s.once.Do(s.load)
	if s.loadErr != nil {
		return "", s.loadErr
	}

	location := job.Location
	if location == "" {
		location = "Not specified"
	}
	description := job.Description
	if len(description) > maxDescriptionLength {
		description = description[:maxDescriptionLength]
	}

	prompt := `You are a hiring manager reviewing a job application. Given the job listing and candidate resume below, identify the most likely reasons this application would be rejected.

## Candidate Resume
` + s.resume + `

## Job Listing
Title: ` + job.Title + `
Company: ` + job.Company + `
Location: ` + location + `
Description: ` + description + `

---

Identify the top 2-4 most likely reasons a recruiter or hiring manager would pass on this candidate for this specific role. Be concrete — reference actual gaps between the job requirements and the candidate's profile. Do not give generic advice.

Respond in 2-4 plain sentences. No bullet points, no headers.`

	return s.client.CallGemini(ctx, prompt, rejectionMaxTokens)
}
