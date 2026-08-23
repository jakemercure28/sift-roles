package scheduler

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"job-search-automation/internal/db"
)

func TestSeedKeywordsFromSearchTerms(t *testing.T) {
	dir := t.TempDir()
	repo, err := db.Open(filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()
	// A hosted tenant has no context.md; its signal lives in resume-derived
	// SEARCH_TERMS. The generic role words (manager/coordinator/specialist) drop
	// out, leaving the distinguishing field tokens.
	companies := `{"SEARCH_TERMS": ["account manager", "communications specialist",
	  "project coordinator", "marketing coordinator"]}`
	if err := os.WriteFile(filepath.Join(dir, "companies.json"), []byte(companies), 0o644); err != nil {
		t.Fatalf("write companies.json: %v", err)
	}
	got := seedKeywordsForTenant(repo, dir)
	want := []string{"account", "communications", "project", "marketing"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("seedKeywordsForTenant = %v, want %v", got, want)
	}
}

func TestSeedKeywordsNoProfileReturnsNil(t *testing.T) {
	dir := t.TempDir()
	repo, err := db.Open(filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()
	if got := seedKeywordsForTenant(repo, dir); len(got) != 0 {
		t.Fatalf("expected nil keywords with no profile, got %v", got)
	}
}

func TestSeedKeywordsMergesSearchTermsAndContext(t *testing.T) {
	dir := t.TempDir()
	repo, err := db.Open(filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()
	if err := os.WriteFile(filepath.Join(dir, "companies.json"),
		[]byte(`{"SEARCH_TERMS": ["buyer"]}`), 0o644); err != nil {
		t.Fatalf("write companies.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "context.md"),
		[]byte("## Industry\n\nretail\n"), 0o644); err != nil {
		t.Fatalf("write context.md: %v", err)
	}
	got := seedKeywordsForTenant(repo, dir)
	want := []string{"buyer", "retail"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("seedKeywordsForTenant = %v, want %v", got, want)
	}
}

func TestParseSeedKeywords(t *testing.T) {
	md := `# Working Context

## What I'm looking for

Open to remote

## Industry

tech

## Target titles

- DevOps Engineer
- Site Reliability Engineer
- Platform Engineer

## Stack I'm most productive in

- AWS
- Kubernetes
`
	got := parseSeedKeywords(md)
	want := []string{"tech", "devops", "site", "reliability", "platform"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseSeedKeywords = %v, want %v", got, want)
	}
}

func TestParseSeedKeywordsNonTech(t *testing.T) {
	md := `## Industry

food service

## Target titles

- Restaurant General Manager
- Marketing Manager
`
	got := parseSeedKeywords(md)
	want := []string{"food", "service", "restaurant", "general", "marketing"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseSeedKeywords = %v, want %v", got, want)
	}
}

func TestParseSeedKeywordsEmpty(t *testing.T) {
	if got := parseSeedKeywords("# Heading only\n\nno sections\n"); len(got) != 0 {
		t.Fatalf("expected no keywords, got %v", got)
	}
}
