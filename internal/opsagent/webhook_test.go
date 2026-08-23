package opsagent

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

const firingPayload = `{
  "status": "firing",
  "receiver": "opsagent",
  "groupLabels": {"alertname": "TenantDistributionStalled"},
  "alerts": [
    {
      "status": "firing",
      "labels": {"alertname": "TenantDistributionStalled", "severity": "critical", "namespace": "jsa-prod"},
      "annotations": {"summary": "No distribution in 6.5h", "runbook": "check the scrape cron"},
      "startsAt": "2026-07-29T10:00:00Z",
      "fingerprint": "abc123"
    }
  ]
}`

func TestDescribeAlertsBuildsTaskWithLabelsAndAnnotations(t *testing.T) {
	var p alertmanagerPayload
	if err := json.Unmarshal([]byte(firingPayload), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	fingerprint, task := describeAlerts(p)

	if fingerprint != "TenantDistributionStalled" {
		t.Errorf("fingerprint = %q, want the alert name", fingerprint)
	}
	// The annotation carries the remediation hint authored in alerts.yml, so
	// dropping it would throw away the fastest route to the right fix.
	for _, want := range []string{"TenantDistributionStalled", "critical", "jsa-prod", "check the scrape cron"} {
		if !strings.Contains(task, want) {
			t.Errorf("task should mention %q, got:\n%s", want, task)
		}
	}
}

func TestDescribeAlertsIgnoresResolvedMembersOfAGroup(t *testing.T) {
	p := alertmanagerPayload{
		Status: "firing",
		Alerts: []alertEntry{{
			Status: "resolved",
			Labels: map[string]string{"alertname": "AlreadyOver"},
		}},
	}

	fingerprint, task := describeAlerts(p)
	if fingerprint != "" || task != "" {
		t.Errorf("a group with only resolved alerts needs no investigation, got %q / %q", fingerprint, task)
	}
}

func TestDescribeAlertsFingerprintIsStableAcrossOrdering(t *testing.T) {
	// Alertmanager does not guarantee member order within a group. If the
	// fingerprint moved with it, the same problem would open a new escalation
	// issue on every firing instead of commenting on the existing one.
	mk := func(names ...string) alertmanagerPayload {
		p := alertmanagerPayload{Status: "firing"}
		for _, n := range names {
			p.Alerts = append(p.Alerts, alertEntry{
				Status: "firing",
				Labels: map[string]string{"alertname": n},
			})
		}
		return p
	}

	a, _ := describeAlerts(mk("Bravo", "Alpha"))
	b, _ := describeAlerts(mk("Alpha", "Bravo"))
	if a != b {
		t.Errorf("fingerprint must not depend on member order: %q vs %q", a, b)
	}
}

func newTestServer() *Server {
	return NewServer(&Agent{logger: discardLogger()}, Config{})
}

func TestClaimSerializesConcurrentRuns(t *testing.T) {
	// Two investigations at once would race on the same remediation and pay
	// twice for one problem.
	s := newTestServer()
	if s.claim("first") != claimOK {
		t.Fatal("the first claim should succeed")
	}
	if got := s.claim("second-and-different"); got != claimBusy {
		t.Fatalf("a second run must not start while one is in flight, got %v", got)
	}
	s.release()
	if s.claim("second-and-different") != claimOK {
		t.Fatal("a new run should be allowed once the previous one released")
	}
}

func TestClaimAppliesPerFingerprintCooldown(t *testing.T) {
	// A flapping alert re-fires every repeat_interval; re-investigating an
	// unchanged problem produces an identical write-up at full token cost.
	s := newTestServer()
	if s.claim("Flapping") != claimOK {
		t.Fatal("the first claim should succeed")
	}
	s.release()

	if got := s.claim("Flapping"); got != claimCooldown {
		t.Fatalf("the same fingerprint should be suppressed during its cooldown, got %v", got)
	}
	if s.claim("SomethingElse") != claimOK {
		t.Fatal("cooldown must be per fingerprint, not global")
	}
}

func TestClaimReleasesCooldownAfterItExpires(t *testing.T) {
	s := newTestServer()
	if s.claim("Stale") != claimOK {
		t.Fatal("the first claim should succeed")
	}
	s.release()

	// Backdate past the cooldown rather than sleeping for 45 minutes.
	s.mu.Lock()
	s.recent["Stale"] = time.Now().Add(-cooldown - time.Minute)
	s.mu.Unlock()

	if s.claim("Stale") != claimOK {
		t.Fatal("an expired cooldown should allow the fingerprint again")
	}
}

func TestHandleAlertAcknowledgesResolvedWithoutInvestigating(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodPost, "/alert",
		strings.NewReader(`{"status":"resolved","alerts":[]}`))
	rec := httptest.NewRecorder()

	s.handleAlert(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("resolved notifications should be acknowledged, got %d", rec.Code)
	}
	// No run was claimed, so a fresh claim must still succeed.
	if s.claim("anything") != claimOK {
		t.Error("a resolved notification must not start an investigation")
	}
}

func TestHandleAlertDoesNotAcknowledgeWhatItDropped(t *testing.T) {
	// Runs are serialized, so a delivery arriving mid-investigation is not
	// handled at all. Answering 2xx would tell Alertmanager it landed, and the
	// next notification for an unchanged group is repeat_interval away (12h), so
	// one long investigation could swallow a different alert for half a day. A
	// 5xx makes Alertmanager retry the delivery instead.
	s := newTestServer()
	if s.claim("in-flight") != claimOK {
		t.Fatal("failed to simulate a run in flight")
	}

	req := httptest.NewRequest(http.MethodPost, "/alert", strings.NewReader(
		`{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"SomethingElse"}}]}`))
	rec := httptest.NewRecorder()
	s.handleAlert(rec, req)

	if rec.Code < 500 {
		t.Errorf("a dropped delivery must not be acknowledged, got %d", rec.Code)
	}
}

func TestHandleAlertAcknowledgesACooldownSkip(t *testing.T) {
	// Unlike a busy skip, this one is deliberate: an identical investigation just
	// ran. Retrying it would only burn tokens on the same write-up.
	s := newTestServer()
	if s.claim("Flapper") != claimOK {
		t.Fatal("failed to seed the cooldown")
	}
	s.release()

	req := httptest.NewRequest(http.MethodPost, "/alert", strings.NewReader(
		`{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"Flapper"}}]}`))
	rec := httptest.NewRecorder()
	s.handleAlert(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("a deliberate cooldown skip should be acknowledged, got %d", rec.Code)
	}
}

func TestHandleAlertRejectsMalformedBody(t *testing.T) {
	s := newTestServer()
	req := httptest.NewRequest(http.MethodPost, "/alert", strings.NewReader(`{not json`))
	rec := httptest.NewRecorder()

	s.handleAlert(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed payload should be rejected, got %d", rec.Code)
	}
}

func TestHandleAlertRejectsNonPost(t *testing.T) {
	s := newTestServer()
	rec := httptest.NewRecorder()

	s.handleAlert(rec, httptest.NewRequest(http.MethodGet, "/alert", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /alert should be rejected, got %d", rec.Code)
	}
}
