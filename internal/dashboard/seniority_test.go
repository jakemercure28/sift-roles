package dashboard

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func eqIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func ptrStr(p *int) string {
	if p == nil {
		return "nil"
	}
	return strconv.Itoa(*p)
}

func TestParseYearsFromDescription(t *testing.T) {
	cases := map[string]*int{
		"Requires 5+ years of experience in Go": ptr(5),
		"minimum of 3 years":                    ptr(3),
		"10 years in distributed systems":       ptr(10),
		"2 years exp, plus 8 years experience":  ptr(8), // max wins
		"no years mentioned here":               nil,
		"":                                      nil,
		"99 years experience":                   nil, // out of 1..30 range
	}
	for desc, want := range cases {
		got := parseYearsFromDescription(desc)
		if !eqIntPtr(got, want) {
			t.Errorf("parseYears(%q) = %v, want %v", desc, ptrStr(got), ptrStr(want))
		}
	}
}

func TestLevelFromTitle(t *testing.T) {
	cases := map[string]string{
		"Senior Backend Engineer": "senior",
		"Staff Engineer":          "staff",
		"Principal SRE":           "staff",
		"Engineering Manager":     "staff",
		"Junior Developer":        "junior",
		"Software Engineer":       "mid",
	}
	for title, want := range cases {
		if got := levelFromTitle(title); got != want {
			t.Errorf("levelFromTitle(%q) = %q, want %q", title, got, want)
		}
	}
}

func TestClassifySeniorityAndAccessible(t *testing.T) {
	// JD years win over the title.
	s := classifySeniority("Senior Engineer", "needs 8 years experience")
	if s.Source != "jd" || s.Years == nil || *s.Years != 8 || s.Level != "staff" {
		t.Fatalf("classify jd = %+v", s)
	}
	// No years -> title-based.
	s2 := classifySeniority("Senior Engineer", "no year hints")
	if s2.Source != "title" || s2.Years != nil || s2.Level != "senior" {
		t.Fatalf("classify title = %+v", s2)
	}

	if isAccessible("X", "8 years experience", 6) {
		t.Fatal("8y job should not be accessible to 6y applicant")
	}
	if !isAccessible("X", "3 years experience", 6) {
		t.Fatal("3y job should be accessible to 6y applicant")
	}
	// Title-only mid role is accessible to a senior-level (6y) applicant.
	if !isAccessible("Software Engineer", "no years", 6) {
		t.Fatal("mid title should be accessible to 6y applicant")
	}
}

func TestIsAccessibleForMatchesIsAccessible(t *testing.T) {
	// isAccessibleFor takes a precomputed Seniority; it must agree with the
	// original isAccessible across the levels and the YOE bands.
	descs := []string{
		"needs 8 years experience",
		"3 years experience required",
		"minimum of 1 year",
		"no year hints at all",
		"",
	}
	titles := []string{"Software Engineer", "Senior Engineer", "Staff Engineer", "Junior Developer"}
	for _, yoe := range []int{0, 2, 4, 6, 12} {
		for _, title := range titles {
			for _, desc := range descs {
				want := isAccessible(title, desc, yoe)
				got := isAccessibleFor(classifySeniority(title, desc), yoe)
				if got != want {
					t.Errorf("isAccessibleFor(%q,%q,%d)=%v, isAccessible=%v", title, desc, yoe, got, want)
				}
			}
		}
	}
}

func TestRemainderSkewsSenior(t *testing.T) {
	yoe := ptr(3)
	// All inaccessible roles are senior/staff -> skews senior.
	senior := []Seniority{
		{Level: "staff", Years: ptr(10), Source: "jd"},
		{Level: "senior", Years: ptr(7), Source: "jd"},
		{Level: "staff", Source: "title"},
	}
	if !remainderSkewsSenior(senior, yoe) {
		t.Fatal("all-senior remainder should skew senior")
	}
	// Inaccessible roles that are not senior-ish -> does not skew.
	mixed := []Seniority{
		{Level: "mid", Years: ptr(4), Source: "jd"},
		{Level: "mid", Years: ptr(4), Source: "jd"},
	}
	if remainderSkewsSenior(mixed, yoe) {
		t.Fatal("mid remainder should not skew senior")
	}
	// No inaccessible roles -> false.
	accessible := []Seniority{{Level: "junior", Years: ptr(1), Source: "jd"}}
	if remainderSkewsSenior(accessible, yoe) {
		t.Fatal("fully accessible set should not skew senior")
	}
	// Nil YOE -> false.
	if remainderSkewsSenior(senior, nil) {
		t.Fatal("nil yoe should not skew senior")
	}
}

func TestComputeApplicantYoe(t *testing.T) {
	dir := t.TempDir()
	exp := filepath.Join(dir, "experience")
	if err := os.MkdirAll(exp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exp, "job1.md"), []byte("At Acme from January 2020 to March 2023."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(exp, "job2.md"), []byte("At Globex since June 2023."), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)
	got := computeApplicantYoe(dir, now)
	if got == nil || *got != 6 {
		t.Fatalf("computeApplicantYoe = %v, want 6 (earliest Jan 2020)", ptrStr(got))
	}

	// No experience dir -> nil.
	if computeApplicantYoe(t.TempDir(), now) != nil {
		t.Fatal("missing experience dir should yield nil")
	}
}
