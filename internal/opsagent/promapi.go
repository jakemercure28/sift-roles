package opsagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// promClient queries a Prometheus HTTP API. There are two instances in play:
// the in-cluster kps-prometheus (k8s infra rules) and the compose
// jsa-observability Prometheus on the host (the jsa_* app rules). Both are
// queried so the model sees one picture instead of half of one.
type promClient struct {
	name string
	base string
	http *http.Client
}

func newPromClient(name, base string) *promClient {
	return &promClient{
		name: name,
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: 20 * time.Second},
	}
}

func (p *promClient) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	u := p.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus %s unreachable: %w", p.name, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("prometheus %s %s: %s: %s", p.name, path, resp.Status, truncate(string(body), 300))
	}
	return body, nil
}

// instantSample is one flattened result row. The raw Prometheus response nests
// value as a [timestamp, "string"] tuple, which is awkward for a model to read.
type instantSample struct {
	Metric map[string]string `json:"metric"`
	Value  string            `json:"value"`
}

func (p *promClient) query(ctx context.Context, expr string) ([]instantSample, error) {
	body, err := p.get(ctx, "/api/v1/query", url.Values{"query": {expr}})
	if err != nil {
		return nil, err
	}
	var out struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode query response: %w", err)
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("prometheus %s query failed: %s", p.name, out.Error)
	}
	samples := make([]instantSample, 0, len(out.Data.Result))
	for _, r := range out.Data.Result {
		v := ""
		if len(r.Value) == 2 {
			if s, ok := r.Value[1].(string); ok {
				v = s
			}
		}
		samples = append(samples, instantSample{Metric: r.Metric, Value: v})
	}
	return samples, nil
}

// firingAlert is the trimmed alert view. Annotations carry the remediation hint
// authored in alerts.yml, so they are deliberately kept.
type firingAlert struct {
	Source      string            `json:"source"`
	Name        string            `json:"name"`
	State       string            `json:"state"`
	Severity    string            `json:"severity"`
	ActiveSince string            `json:"active_since"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

func (p *promClient) alerts(ctx context.Context) ([]firingAlert, error) {
	body, err := p.get(ctx, "/api/v1/alerts", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Status string `json:"status"`
		Data   struct {
			Alerts []struct {
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
				State       string            `json:"state"`
				ActiveAt    string            `json:"activeAt"`
			} `json:"alerts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode alerts response: %w", err)
	}
	alerts := make([]firingAlert, 0, len(out.Data.Alerts))
	for _, a := range out.Data.Alerts {
		alerts = append(alerts, firingAlert{
			Source:      p.name,
			Name:        a.Labels["alertname"],
			State:       a.State,
			Severity:    a.Labels["severity"],
			ActiveSince: since(a.ActiveAt),
			Labels:      a.Labels,
			Annotations: a.Annotations,
		})
	}
	return alerts, nil
}
