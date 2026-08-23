package opsagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// githubClient covers the two things the agent needs from GitHub: reading what
// has landed on which branch (so it can tell "already fixed, not yet promoted"
// apart from "unknown failure"), and filing escalation issues.
type githubClient struct {
	token string
	repo  string // owner/name
	http  *http.Client
}

func newGitHubClient(token, repo string) *githubClient {
	return &githubClient{
		token: token,
		repo:  repo,
		http:  &http.Client{Timeout: 25 * time.Second},
	}
}

func (g *githubClient) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.github.com"+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github %s %s: %s: %s", method, path, resp.Status, truncate(string(payload), 300))
	}
	return payload, nil
}

// commitInfo is one commit as the model sees it.
type commitInfo struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
	Author  string `json:"author"`
	Date    string `json:"date"`
}

// compareBranches reports what is on head that is not on base. This is the
// "is the fix already merged but not promoted?" question: comparing main to dev
// answers it directly, which is exactly the situation that left the prod
// scraper broken for a day while its fix sat on dev.
func (g *githubClient) compareBranches(ctx context.Context, base, head string) ([]commitInfo, error) {
	path := fmt.Sprintf("/repos/%s/compare/%s...%s", g.repo, url.PathEscape(base), url.PathEscape(head))
	raw, err := g.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Commits []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
				Author  struct {
					Name string `json:"name"`
					Date string `json:"date"`
				} `json:"author"`
			} `json:"commit"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode compare: %w", err)
	}
	commits := make([]commitInfo, 0, len(out.Commits))
	for _, c := range out.Commits {
		commits = append(commits, commitInfo{
			SHA:     shortSHA(c.SHA),
			Message: firstLine(c.Commit.Message),
			Author:  c.Commit.Author.Name,
			Date:    c.Commit.Author.Date,
		})
	}
	return commits, nil
}

// recentCommits lists the newest commits on a branch, optionally filtered to a
// path so the agent can ask "what changed in the scraper lately?".
func (g *githubClient) recentCommits(ctx context.Context, branch, path string, limit int) ([]commitInfo, error) {
	q := url.Values{}
	q.Set("sha", branch)
	q.Set("per_page", fmt.Sprintf("%d", limit))
	if path != "" {
		q.Set("path", path)
	}
	raw, err := g.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/commits?%s", g.repo, q.Encode()), nil)
	if err != nil {
		return nil, err
	}
	var out []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
				Date string `json:"date"`
			} `json:"author"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode commits: %w", err)
	}
	commits := make([]commitInfo, 0, len(out))
	for _, c := range out {
		commits = append(commits, commitInfo{
			SHA:     shortSHA(c.SHA),
			Message: firstLine(c.Commit.Message),
			Author:  c.Commit.Author.Name,
			Date:    c.Commit.Author.Date,
		})
	}
	return commits, nil
}

// escalationLabel tags every issue the agent files so they can be filtered, and
// so findExisting only ever matches the agent's own issues.
const escalationLabel = "opsagent"

// escalate files an escalation, or comments on the open issue for the same
// alert fingerprint. Deduping on fingerprint keeps a flapping alert to one
// thread instead of a new issue every firing.
func (g *githubClient) escalate(ctx context.Context, fingerprint, title, body string) (string, error) {
	marker := fmt.Sprintf("<!-- opsagent-fingerprint: %s -->", fingerprint)
	full := body + "\n\n" + marker

	existing, err := g.findExisting(ctx, marker)
	if err != nil {
		return "", err
	}
	if existing > 0 {
		if _, err := g.do(ctx, http.MethodPost,
			fmt.Sprintf("/repos/%s/issues/%d/comments", g.repo, existing),
			map[string]string{"body": full}); err != nil {
			return "", err
		}
		return fmt.Sprintf("https://github.com/%s/issues/%d", g.repo, existing), nil
	}

	raw, err := g.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues", g.repo), map[string]any{
		"title":  title,
		"body":   full,
		"labels": []string{escalationLabel},
	})
	if err != nil {
		return "", err
	}
	var created struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		return "", fmt.Errorf("decode created issue: %w", err)
	}
	return created.HTMLURL, nil
}

// findExisting looks for an open agent-filed issue carrying the same marker.
// It returns 0 when there is none.
func (g *githubClient) findExisting(ctx context.Context, marker string) (int, error) {
	q := url.Values{}
	q.Set("state", "open")
	q.Set("labels", escalationLabel)
	q.Set("per_page", "100")
	raw, err := g.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/issues?%s", g.repo, q.Encode()), nil)
	if err != nil {
		return 0, err
	}
	var issues []struct {
		Number int    `json:"number"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal(raw, &issues); err != nil {
		return 0, fmt.Errorf("decode issue list: %w", err)
	}
	for _, is := range issues {
		if strings.Contains(is.Body, marker) {
			return is.Number, nil
		}
	}
	return 0, nil
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
