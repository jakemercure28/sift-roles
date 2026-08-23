package opsagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// argoClient reads Application sync/health state and can trigger a sync. Argo
// reconciles to git, so a sync is one of the safer remediations available: it
// converges the cluster onto the committed desired state rather than inventing
// a new one.
type argoClient struct {
	base  string
	token string
	http  *http.Client
}

func newArgoClient(base, token string) *argoClient {
	return &argoClient{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (a *argoClient) do(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("argocd unreachable: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("argocd %s %s: %s: %s", method, path, resp.Status, truncate(string(payload), 300))
	}
	return payload, nil
}

type appStatus struct {
	Name     string `json:"name"`
	Sync     string `json:"sync"`
	Health   string `json:"health"`
	Revision string `json:"revision"`
	Message  string `json:"message,omitempty"`
}

// listApps returns every Application's sync and health state. A `Progressing`
// or `Degraded` app next to a firing alert is usually the whole story.
func (a *argoClient) listApps(ctx context.Context) ([]appStatus, error) {
	raw, err := a.do(ctx, http.MethodGet, "/api/v1/applications", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Sync struct {
					Status   string `json:"status"`
					Revision string `json:"revision"`
				} `json:"sync"`
				Health struct {
					Status  string `json:"status"`
					Message string `json:"message"`
				} `json:"health"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode applications: %w", err)
	}
	apps := make([]appStatus, 0, len(out.Items))
	for _, it := range out.Items {
		apps = append(apps, appStatus{
			Name:     it.Metadata.Name,
			Sync:     it.Status.Sync.Status,
			Health:   it.Status.Health.Status,
			Revision: shortSHA(it.Status.Sync.Revision),
			Message:  it.Status.Health.Message,
		})
	}
	return apps, nil
}

// sync triggers a sync of one Application. Prune is deliberately not exposed:
// the agent may converge the cluster onto git, but it may not delete resources
// git no longer declares.
func (a *argoClient) sync(ctx context.Context, name string) error {
	body := strings.NewReader(`{"prune":false,"dryRun":false}`)
	_, err := a.do(ctx, http.MethodPost, fmt.Sprintf("/api/v1/applications/%s/sync", name), body)
	return err
}
