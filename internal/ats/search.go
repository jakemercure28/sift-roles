package ats

import (
	"context"
	"encoding/json"
	"fmt"
)

// searchGreenhouseBoard finds a job on a Greenhouse board by title match, ported
// from searchGreenhouseBoard in lib/ats-resolver.js.
func searchGreenhouseBoard(ctx context.Context, fetch Fetcher, job Job, board string) (*CanonicalJob, error) {
	url := fmt.Sprintf("https://boards-api.greenhouse.io/v1/boards/%s/jobs?content=true", board)
	res, err := fetch(ctx, url, "greenhouse-search/"+board)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	var data ghBoard
	if jsonErr := json.Unmarshal(res.Body, &data); jsonErr != nil {
		return nil, nil
	}
	for i := range data.Jobs {
		m := &data.Jobs[i]
		if titleMatches(m.Title, job.Title) {
			return normalizeCanonicalJob(CanonicalJob{
				ID:          "greenhouse-" + m.ID.String(),
				Platform:    "Greenhouse",
				Title:       m.Title,
				Company:     board,
				URL:         m.AbsoluteURL,
				PostedAt:    m.UpdatedAt,
				Description: stripHTML(m.Content),
				Location:    m.Location.Name,
			}, job), nil
		}
	}
	return nil, nil
}

// searchAshbyBoard finds a job on an Ashby board by title match, ported from
// searchAshbyBoard in lib/ats-resolver.js.
func searchAshbyBoard(ctx context.Context, fetch Fetcher, job Job, board string) (*CanonicalJob, error) {
	url := fmt.Sprintf("https://api.ashbyhq.com/posting-api/job-board/%s?includeCompensation=true", board)
	res, err := fetch(ctx, url, "ashby-search/"+board)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	var data ashbyBoard
	if jsonErr := json.Unmarshal(res.Body, &data); jsonErr != nil {
		return nil, nil
	}
	for i := range data.Jobs {
		m := &data.Jobs[i]
		if titleMatches(m.Title, job.Title) {
			if m.ID == "" {
				return nil, nil
			}
			return normalizeCanonicalJob(CanonicalJob{
				ID:          "ashby-" + m.ID,
				Platform:    "Ashby",
				Title:       m.Title,
				Company:     firstNonEmpty(m.CompanyName, board),
				URL:         m.JobURL,
				PostedAt:    firstNonEmpty(m.PublishedDate, m.PublishedAt, m.UpdatedAt),
				Description: stripHTML(firstNonEmpty(m.DescriptionHTML, m.DescriptionPlain, m.Description)),
				Location:    firstNonEmpty(m.Location, m.LocationName),
			}, job), nil
		}
	}
	return nil, nil
}

// searchLeverBoard finds a job on a Lever board by title match, ported from
// searchLeverBoard in lib/ats-resolver.js.
func searchLeverBoard(ctx context.Context, fetch Fetcher, job Job, board string) (*CanonicalJob, error) {
	url := fmt.Sprintf("https://api.lever.co/v0/postings/%s?mode=json", board)
	res, err := fetch(ctx, url, "lever-search/"+board)
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, nil
	}
	var data []leverJob
	if jsonErr := json.Unmarshal(res.Body, &data); jsonErr != nil {
		return nil, nil
	}
	for i := range data {
		m := &data[i]
		if titleMatches(m.Text, job.Title) {
			if m.ID == "" {
				return nil, nil
			}
			return normalizeCanonicalJob(CanonicalJob{
				ID:          "lever-" + m.ID,
				Platform:    "Lever",
				Title:       m.Text,
				Company:     board,
				URL:         m.HostedURL,
				PostedAt:    leverISOTime(m.CreatedAt),
				Description: stripHTML(leverDescription(m.DescriptionPlain, m.Lists)),
				Location:    m.Categories.Location,
			}, job), nil
		}
	}
	return nil, nil
}

// searchPrimaryBoards probes the candidate Greenhouse, then Ashby/Lever boards
// for a title match, ported from searchPrimaryBoards in lib/ats-resolver.js.
func searchPrimaryBoards(ctx context.Context, fetch Fetcher, job Job) (*CanonicalJob, error) {
	for _, board := range greenhouseBoardCandidates(job.Company) {
		match, err := searchGreenhouseBoard(ctx, fetch, job, board)
		if err != nil {
			return nil, err
		}
		if match != nil {
			return match, nil
		}
	}
	for _, board := range slugCandidates(job.Company) {
		ashby, err := searchAshbyBoard(ctx, fetch, job, board)
		if err != nil {
			return nil, err
		}
		if ashby != nil {
			return ashby, nil
		}
		lever, err := searchLeverBoard(ctx, fetch, job, board)
		if err != nil {
			return nil, err
		}
		if lever != nil {
			return lever, nil
		}
	}
	return nil, nil
}
