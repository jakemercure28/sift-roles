package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// askServer builds a front door with the given Gemini agent (may be nil).
func askServer(t *testing.T, agent RejectionScorer) *httptest.Server {
	t.Helper()
	repo := newRepo(t)
	srv, err := New(t.TempDir(), repo, agent, time.Second, 500, t.TempDir(), discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func decodeAsk(t *testing.T, resp *http.Response) askResp {
	t.Helper()
	defer resp.Body.Close()
	var a askResp
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return a
}

func TestAskValidation(t *testing.T) {
	ts := askServer(t, &fakeRejection{})

	if r := postJSON(t, ts.URL+"/api/ask", `{"question":"  "}`); r.StatusCode != http.StatusBadRequest {
		r.Body.Close()
		t.Fatalf("empty question status = %d, want 400", r.StatusCode)
	}
	long := `{"question":"` + strings.Repeat("a", 501) + `"}`
	if r := postJSON(t, ts.URL+"/api/ask", long); r.StatusCode != http.StatusBadRequest {
		r.Body.Close()
		t.Fatalf("long question status = %d, want 400", r.StatusCode)
	}
}

func TestAskNoAgentOrKey(t *testing.T) {
	// Nil agent -> no-key.
	a := decodeAsk(t, postJSON(t, askServer(t, nil).URL+"/api/ask", `{"question":"how do I apply?"}`))
	if a.Answer != nil || a.Error != "no-key" {
		t.Fatalf("nil agent = %+v, want no-key", a)
	}

	// Agent returns the "key not set" error -> no-key.
	noKey := &fakeRejection{askFn: func(string) (string, error) {
		return "", errors.New("GEMINI_API_KEY environment variable is not set")
	}}
	a2 := decodeAsk(t, postJSON(t, askServer(t, noKey).URL+"/api/ask", `{"question":"hi"}`))
	if a2.Answer != nil || a2.Error != "no-key" {
		t.Fatalf("no-key error = %+v", a2)
	}
}

func TestAskAnswerEmptyAndUnavailable(t *testing.T) {
	ok := &fakeRejection{askFn: func(prompt string) (string, error) {
		if !strings.Contains(prompt, "USER QUESTION: what is ghosted?") {
			t.Errorf("prompt missing question: %q", prompt)
		}
		return "Ghosted means no reply after a few weeks.", nil
	}}
	a := decodeAsk(t, postJSON(t, askServer(t, ok).URL+"/api/ask", `{"question":"what is ghosted?"}`))
	if a.Answer == nil || *a.Answer != "Ghosted means no reply after a few weeks." {
		t.Fatalf("answer = %+v", a)
	}

	empty := &fakeRejection{askFn: func(string) (string, error) { return "", nil }}
	a2 := decodeAsk(t, postJSON(t, askServer(t, empty).URL+"/api/ask", `{"question":"hi"}`))
	if a2.Answer != nil || a2.Error != "empty" {
		t.Fatalf("empty = %+v", a2)
	}

	fail := &fakeRejection{askFn: func(string) (string, error) { return "", errors.New("gemini 500") }}
	a3 := decodeAsk(t, postJSON(t, askServer(t, fail).URL+"/api/ask", `{"question":"hi"}`))
	if a3.Answer != nil || a3.Error != "unavailable" {
		t.Fatalf("unavailable = %+v", a3)
	}
}
