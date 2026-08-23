package jobfilter

import "strings"

// MatchesSearchTerms is the Go port of the worker's title filter
// (scraper-service/src/lib/search.ts) so board-mode distribution applies the
// SAME term semantics the per-tenant scrape path applies worker-side.
// Multi-word terms ("financial analyst") and longer single words (>= 6 chars,
// e.g. "investment", "marketing") match as substrings so plurals/suffixes
// still count. Short single words ("risk", "tax", "vp") must match on a word
// boundary so they don't fire inside unrelated words (e.g. "tax" in "syntax").
func MatchesSearchTerms(text string, terms []string) bool {
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	var words map[string]bool
	for _, raw := range terms {
		term := strings.TrimSpace(strings.ToLower(raw))
		if term == "" {
			continue
		}
		if strings.Contains(term, " ") || len(term) >= 6 {
			if strings.Contains(lower, term) {
				return true
			}
			continue
		}
		if words == nil {
			words = splitWords(lower)
		}
		if words[term] {
			return true
		}
	}
	return false
}

// splitWords mirrors the worker's `lower.split(/[^a-z0-9]+/)` tokenization.
func splitWords(lower string) map[string]bool {
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(lower, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		words[w] = true
	}
	return words
}
