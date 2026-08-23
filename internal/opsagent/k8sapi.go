package opsagent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Paths the kubelet projects into every pod. The agent authenticates as its own
// ServiceAccount; there is no kubeconfig and no kubectl in the distroless image.
const (
	saTokenPath  = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saCACertPath = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// k8sClient is a minimal Kubernetes API client. The agent needs roughly six
// operations, so it talks to the REST API directly rather than pulling in
// client-go: the dependency is large and the distroless image stays small.
type k8sClient struct {
	base  string
	token string
	http  *http.Client
}

// newK8sClient builds a client from the in-pod ServiceAccount credentials. The
// token is read once at startup; projected-token rotation is handled by
// reReadToken on 401.
func newK8sClient() (*k8sClient, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not running in-cluster: KUBERNETES_SERVICE_HOST/PORT unset")
	}
	token, err := os.ReadFile(saTokenPath)
	if err != nil {
		return nil, fmt.Errorf("read service account token: %w", err)
	}
	caPEM, err := os.ReadFile(saCACertPath)
	if err != nil {
		return nil, fmt.Errorf("read service account CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("service account CA is not valid PEM")
	}
	return &k8sClient{
		base:  fmt.Sprintf("https://%s:%s", host, barePort(port)),
		token: strings.TrimSpace(string(token)),
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
	}, nil
}

// barePort strips anything the downward API may have appended to the port value.
func barePort(port string) string {
	if i := strings.IndexByte(port, ':'); i >= 0 {
		return port[:i]
	}
	return port
}

// do issues one authenticated API request. contentType is empty for GET/DELETE.
func (c *k8sClient) do(ctx context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Cap the read: pod logs from a crash-looping container can be enormous, and
	// anything past this is truncated before it ever reaches the model's context.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// Projected tokens rotate; re-read once and let the caller retry.
		if t, rerr := os.ReadFile(saTokenPath); rerr == nil {
			c.token = strings.TrimSpace(string(t))
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("kubernetes %s %s: %s: %s", method, path, resp.Status, truncate(string(payload), 400))
	}
	return payload, nil
}

const maxAPIResponseBytes = 4 << 20 // 4 MiB

// --- read-only operations ---

// podSummary is the trimmed pod view handed to the model. The full PodList is
// mostly noise (managed fields, resource versions) that would burn context
// without improving the diagnosis.
type podSummary struct {
	Name       string `json:"name"`
	Phase      string `json:"phase"`
	Ready      string `json:"ready"`
	Restarts   int64  `json:"restarts"`
	Age        string `json:"age"`
	Node       string `json:"node,omitempty"`
	Message    string `json:"message,omitempty"`
	LastReason string `json:"last_termination_reason,omitempty"`
}

func (c *k8sClient) listPods(ctx context.Context, namespace, labelSelector string) ([]podSummary, error) {
	q := url.Values{}
	if labelSelector != "" {
		q.Set("labelSelector", labelSelector)
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods", url.PathEscape(namespace))
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	raw, err := c.do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string `json:"name"`
				CreationTimestamp string `json:"creationTimestamp"`
			} `json:"metadata"`
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
			Status struct {
				Phase             string `json:"phase"`
				Message           string `json:"message"`
				ContainerStatuses []struct {
					Ready        bool  `json:"ready"`
					RestartCount int64 `json:"restartCount"`
					LastState    struct {
						Terminated struct {
							Reason string `json:"reason"`
						} `json:"terminated"`
					} `json:"lastState"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode pod list: %w", err)
	}
	out := make([]podSummary, 0, len(list.Items))
	for _, it := range list.Items {
		var ready, total, restarts int64
		var lastReason string
		for _, cs := range it.Status.ContainerStatuses {
			total++
			if cs.Ready {
				ready++
			}
			restarts += cs.RestartCount
			if cs.LastState.Terminated.Reason != "" {
				lastReason = cs.LastState.Terminated.Reason
			}
		}
		out = append(out, podSummary{
			Name:       it.Metadata.Name,
			Phase:      it.Status.Phase,
			Ready:      fmt.Sprintf("%d/%d", ready, total),
			Restarts:   restarts,
			Age:        since(it.Metadata.CreationTimestamp),
			Node:       it.Spec.NodeName,
			Message:    it.Status.Message,
			LastReason: lastReason,
		})
	}
	return out, nil
}

// describePod returns the pod's status and conditions. Spec and managed fields
// are dropped: probe failures live in status and events, not in the spec.
func (c *k8sClient) describePod(ctx context.Context, namespace, name string) (string, error) {
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", url.PathEscape(namespace), url.PathEscape(name))
	raw, err := c.do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return "", err
	}
	var pod struct {
		Metadata map[string]any `json:"metadata"`
		Status   map[string]any `json:"status"`
		Spec     struct {
			Containers []map[string]any `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &pod); err != nil {
		return "", fmt.Errorf("decode pod: %w", err)
	}
	delete(pod.Metadata, "managedFields")
	// Probe definitions matter (a liveness probe that is too aggressive is itself
	// a common root cause), but env and volume mounts are noise here.
	probes := make([]map[string]any, 0, len(pod.Spec.Containers))
	for _, ct := range pod.Spec.Containers {
		keep := map[string]any{"name": ct["name"], "image": ct["image"]}
		for _, p := range []string{"livenessProbe", "readinessProbe", "startupProbe", "resources"} {
			if v, ok := ct[p]; ok {
				keep[p] = v
			}
		}
		probes = append(probes, keep)
	}
	out, err := json.MarshalIndent(map[string]any{
		"metadata":   pod.Metadata,
		"status":     pod.Status,
		"containers": probes,
	}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (c *k8sClient) podLogs(ctx context.Context, namespace, name, container string, tailLines int, previous bool) (string, error) {
	q := url.Values{}
	q.Set("tailLines", strconv.Itoa(tailLines))
	q.Set("timestamps", "true")
	if container != "" {
		q.Set("container", container)
	}
	if previous {
		q.Set("previous", "true")
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/log?%s", url.PathEscape(namespace), url.PathEscape(name), q.Encode())
	raw, err := c.do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

type eventSummary struct {
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Object  string `json:"object"`
	Count   int64  `json:"count"`
	Age     string `json:"age"`
	Message string `json:"message"`
}

func (c *k8sClient) listEvents(ctx context.Context, namespace string) ([]eventSummary, error) {
	path := fmt.Sprintf("/api/v1/namespaces/%s/events", url.PathEscape(namespace))
	raw, err := c.do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Type           string `json:"type"`
			Reason         string `json:"reason"`
			Message        string `json:"message"`
			Count          int64  `json:"count"`
			LastTimestamp  string `json:"lastTimestamp"`
			InvolvedObject struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"involvedObject"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode event list: %w", err)
	}
	out := make([]eventSummary, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, eventSummary{
			Type:    it.Type,
			Reason:  it.Reason,
			Object:  it.InvolvedObject.Kind + "/" + it.InvolvedObject.Name,
			Count:   it.Count,
			Age:     since(it.LastTimestamp),
			Message: it.Message,
		})
	}
	return out, nil
}

type deploymentSummary struct {
	Name      string `json:"name"`
	Replicas  string `json:"replicas"`
	Image     string `json:"image"`
	Available int64  `json:"available"`
	Age       string `json:"age"`
}

func (c *k8sClient) listDeployments(ctx context.Context, namespace string) ([]deploymentSummary, error) {
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments", url.PathEscape(namespace))
	raw, err := c.do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string `json:"name"`
				CreationTimestamp string `json:"creationTimestamp"`
			} `json:"metadata"`
			Spec struct {
				Replicas int64 `json:"replicas"`
				Template struct {
					Spec struct {
						Containers []struct {
							Image string `json:"image"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
			Status struct {
				ReadyReplicas     int64 `json:"readyReplicas"`
				AvailableReplicas int64 `json:"availableReplicas"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode deployment list: %w", err)
	}
	out := make([]deploymentSummary, 0, len(list.Items))
	for _, it := range list.Items {
		image := ""
		if len(it.Spec.Template.Spec.Containers) > 0 {
			image = it.Spec.Template.Spec.Containers[0].Image
		}
		out = append(out, deploymentSummary{
			Name:      it.Metadata.Name,
			Replicas:  fmt.Sprintf("%d/%d", it.Status.ReadyReplicas, it.Spec.Replicas),
			Image:     image,
			Available: it.Status.AvailableReplicas,
			Age:       since(it.Metadata.CreationTimestamp),
		})
	}
	return out, nil
}

// --- write operations (allowlisted; see gate.go) ---

// rolloutRestart stamps the pod template with a restart annotation, which is
// exactly what `kubectl rollout restart` does. Requires only patch on
// deployments, so the Role stays narrow.
func (c *k8sClient) rolloutRestart(ctx context.Context, namespace, name string) error {
	patch := fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{"jsa.opsagent/restartedAt":%q}}}}}`,
		time.Now().UTC().Format(time.RFC3339),
	)
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", url.PathEscape(namespace), url.PathEscape(name))
	_, err := c.do(ctx, http.MethodPatch, path, "application/strategic-merge-patch+json", []byte(patch))
	return err
}

func (c *k8sClient) deletePod(ctx context.Context, namespace, name string) error {
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", url.PathEscape(namespace), url.PathEscape(name))
	_, err := c.do(ctx, http.MethodDelete, path, "", nil)
	return err
}

func (c *k8sClient) scaleDeployment(ctx context.Context, namespace, name string, replicas int) error {
	patch := fmt.Sprintf(`{"spec":{"replicas":%d}}`, replicas)
	path := fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s/scale", url.PathEscape(namespace), url.PathEscape(name))
	_, err := c.do(ctx, http.MethodPatch, path, "application/strategic-merge-patch+json", []byte(patch))
	return err
}

// --- helpers ---

// since renders an RFC3339 timestamp as an approximate age ("3h12m"), which is
// what a human reads off kubectl and what the model reasons about.
func since(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return "unknown"
	}
	return time.Since(t).Round(time.Minute).String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "... [truncated]"
}
