package voice

import "testing"

func hasType(issues []Issue, t string) bool {
	for _, i := range issues {
		if i.Type == t {
			return true
		}
	}
	return false
}

func TestSplitSentences(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"One. Two! Three?", []string{"One.", "Two!", "Three?"}},
		{"No split here", []string{"No split here"}},
		{"Tight.Spacing stays", []string{"Tight.Spacing stays"}}, // no whitespace after the period
		{"A.  B", []string{"A.", "B"}},                           // collapses the whitespace run
	}
	for _, c := range cases {
		got := splitSentences(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitSentences(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitSentences(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestLocalCheckKillWordAndDash(t *testing.T) {
	issues := LocalCheck("We will leverage synergy — to elevate the realm.")
	if !hasType(issues, "kill_word") {
		t.Errorf("expected a kill_word issue, got %+v", issues)
	}
	if !hasType(issues, "dash") {
		t.Errorf("expected a dash issue, got %+v", issues)
	}
}

func TestLocalCheckBannedOpener(t *testing.T) {
	issues := LocalCheck("Throughout my career I shipped things. Then more things happened later on.")
	if !hasType(issues, "banned_opener") {
		t.Errorf("expected a banned_opener issue, got %+v", issues)
	}
}

func TestLocalCheckCleanText(t *testing.T) {
	// Varied, human-sounding text with no kill words, dashes, or banned openers.
	clean := "I built the deploy tool last spring. It broke twice. We fixed the retry logic and it has held up since then, mostly."
	for _, issue := range LocalCheck(clean) {
		if issue.Type != "low_burstiness" {
			t.Errorf("unexpected issue on clean text: %+v", issue)
		}
	}
}

func TestRenderScore(t *testing.T) {
	cases := []struct {
		score float64
		want  string
	}{
		{0.01, "1.0% AI  (looks human)"},
		{0.1, "10% AI  (borderline)"},
		{0.9, "90% AI  (flagging — rewrite)"},
	}
	for _, c := range cases {
		if got := RenderScore(c.score); got != c.want {
			t.Errorf("RenderScore(%v) = %q, want %q", c.score, got, c.want)
		}
	}
}
