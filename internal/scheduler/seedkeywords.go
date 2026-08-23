package scheduler

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"job-search-automation/internal/db"
)

// seedKeywordsForTenant collects the tenant's target-role tokens so the first-run
// global seed can prefer relevant listings. The reliable signal for a hosted
// tenant is companies.json SEARCH_TERMS: those are derived from the submitted
// resume during onboarding and are always present. context.md is a legacy
// personal-app artifact that is not built for hosted tenants, so it is only an
// optional augmentation here. Best-effort: no readable profile returns nil and the
// seed falls back to recency ordering.
func seedKeywordsForTenant(repo *db.Repository, dataDir string) []string {
	dir := repo.ProfileDir(dataDir)
	seen := make(map[string]bool)
	var out []string
	add := func(tokens []string) {
		for _, tok := range tokens {
			if seen[tok] {
				continue
			}
			seen[tok] = true
			out = append(out, tok)
		}
	}

	// Primary: resume-derived search terms.
	if data, err := os.ReadFile(filepath.Join(dir, "companies.json")); err == nil {
		var cfg struct {
			SearchTerms []string `json:"SEARCH_TERMS"`
		}
		if json.Unmarshal(data, &cfg) == nil {
			for _, term := range cfg.SearchTerms {
				add(tokenizeSeedLine(term))
			}
		}
	}
	// Augment with a legacy context.md profile when one exists.
	if data, err := os.ReadFile(filepath.Join(dir, "context.md")); err == nil {
		add(parseSeedKeywords(string(data)))
	}
	return out
}

// seedStopwords are generic role/structure words that would over-match titles
// across every field (e.g. every infra title contains "engineer"). Dropping them
// keeps the distinguishing tokens (devops, marketing, sales) that actually steer
// the seed toward the tenant's field.
var seedStopwords = map[string]bool{
	"engineer": true, "engineering": true, "manager": true, "management": true,
	"senior": true, "junior": true, "lead": true, "staff": true, "principal": true,
	"associate": true, "specialist": true, "coordinator": true, "director": true,
	"head": true, "remote": true, "open": true, "the": true, "and": true, "for": true,
	"assistant": true,
}

// tokenizeSeedLine lowercases a phrase, splits it on non-alphanumerics, and drops
// generic role words and short tokens. Shared by the SEARCH_TERMS and context.md
// paths so both derive the same distinguishing tokens.
func tokenizeSeedLine(line string) []string {
	var out []string
	for _, tok := range strings.FieldsFunc(strings.ToLower(line), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(tok) < 3 || seedStopwords[tok] {
			continue
		}
		out = append(out, tok)
	}
	return out
}

// parseSeedKeywords pulls distinguishing tokens from a legacy context.md. It reads
// the "## Target titles" bullets and the "## Industry" value, tokenizes them, and
// dedupes.
func parseSeedKeywords(md string) []string {
	keep := false
	seen := make(map[string]bool)
	var out []string
	for _, raw := range strings.Split(md, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#") {
			h := strings.ToLower(strings.Trim(line, "# "))
			keep = strings.Contains(h, "target titles") || strings.Contains(h, "industry")
			continue
		}
		if !keep || line == "" {
			continue
		}
		line = strings.TrimLeft(line, "-*0123456789. ")
		for _, tok := range tokenizeSeedLine(line) {
			if seen[tok] {
				continue
			}
			seen[tok] = true
			out = append(out, tok)
		}
	}
	return out
}
