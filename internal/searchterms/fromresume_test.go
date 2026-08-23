package searchterms

import (
	"strings"
	"testing"
)

func TestFromResumePrefersRoleLines(t *testing.T) {
	resume := `Jane Doe
Senior Platform Engineer | Acme Corp
Built and operated Kubernetes clusters.
Site Reliability Engineer, Globex
`
	got := FromResume(resume)
	if len(got) == 0 {
		t.Fatal("FromResume returned no terms for a resume full of role headings")
	}
	if got[0] != "senior platform engineer" {
		t.Fatalf("first term = %q, want the normalized first role heading", got[0])
	}
	// The separator split drops the employer, so the term is a title, not a title
	// plus a company the pre-filter would never match.
	for _, term := range got {
		if strings.Contains(term, "acme") || strings.Contains(term, "globex") {
			t.Fatalf("term %q carries an employer name", term)
		}
	}
}

func TestFromResumeFallsBackToLexicon(t *testing.T) {
	// No line reads as a role heading (every line ends in a period, so the
	// heading pass skips it), which is exactly when the lexicon sweep matters.
	resume := "I have spent six years doing devops work on payment systems.\n" +
		"Most of that time was hands on with platform engineer duties.\n"
	got := FromResume(resume)
	if len(got) == 0 {
		t.Fatal("lexicon fallback produced no terms")
	}
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "devops") {
		t.Fatalf("terms = %v, want the lexicon to recover \"devops\"", got)
	}
}

func TestFromResumeEmptyAndUnparseable(t *testing.T) {
	if got := FromResume("   \n\t"); got != nil {
		t.Fatalf("FromResume(blank) = %v, want nil", got)
	}
	// A resume with no role vocabulary invents nothing: callers must be able to
	// distinguish "cannot derive" from "derived something generic".
	if got := FromResume("...\n---\n"); len(got) != 0 {
		t.Fatalf("FromResume(no role vocabulary) = %v, want none", got)
	}
}

func TestFromResumeDeduplicatesAndBounds(t *testing.T) {
	line := "Data Engineer | Acme\n"
	got := FromResume(strings.Repeat(line, 20))
	if len(got) != 1 {
		t.Fatalf("FromResume(repeated heading) = %v, want 1 deduplicated term", got)
	}

	var b strings.Builder
	for _, role := range []string{"engineer", "developer", "manager", "director", "analyst",
		"associate", "coordinator", "specialist", "designer", "architect", "consultant", "technician"} {
		b.WriteString("Senior " + role + " | Acme\n")
	}
	if got := FromResume(b.String()); len(got) > maxResumeTerms {
		t.Fatalf("FromResume returned %d terms, want at most %d", len(got), maxResumeTerms)
	}
}
