// Package searchterms deterministically widens a tenant's entered target-role
// terms so the title pre-filter (internal/jobfilter.MatchesSearchTerms) catches
// the many ways employers phrase the same role.
//
// The pre-filter matches multi-word terms as exact substrings. Users enter a few
// canonical titles ("devops engineer", "platform engineer"), but real postings
// title the same work differently ("Devops - Internal Platform & Tools"), so the
// raw phrase never appears and the job is dropped before scoring. Expand derives
// extra match terms mechanically: no LLM, no I/O, pure function, so it is
// predictable, fast, and works for any field. It only ADDS terms to an OR-match,
// so it can never match fewer jobs than the raw terms alone (zero regression);
// precision stays the LLM scorer's job.
//
// It cannot invent vocabulary the user never typed (a "devops" seeker will not
// suddenly match "Cloud Infrastructure Engineer" unless they list an infra
// term). That is deliberate: coverage stays in the user's control.
package searchterms

import "strings"

// MaxTerms bounds the widened set. Each raw term yields at most a few derived
// terms, so this only guards against pathological input inflating the SQL OR.
const MaxTerms = 60

// minToken drops filler fragments; also the shortest tokens the substring path
// treats as word-boundary matches.
const minToken = 2

// minHead is the length a trailing head-noun must reach to be emitted on its
// own, so specific nouns ("reliability", "infrastructure") widen recall but tiny
// ambiguous ones do not.
const minHead = 4

// generic are the career-agnostic role-suffix, seniority, and filler words that
// carry a title but do not identify the field. Stripping them yields the
// discriminating "core" of a term ("senior platform engineer" -> "platform").
// They are only removed from within a phrase; a bare generic term the user typed
// is still honored via the always-emitted full phrase.
var generic = map[string]bool{
	// seniority / level
	"senior": true, "junior": true, "sr": true, "jr": true, "staff": true,
	"principal": true, "lead": true, "mid": true, "entry": true, "level": true,
	"associate": true, "intern": true, "trainee": true,
	"i": true, "ii": true, "iii": true, "iv": true,
	// generic role suffixes
	"engineer": true, "developer": true, "manager": true, "specialist": true,
	"analyst": true, "coordinator": true, "administrator": true, "technician": true,
	"officer": true, "director": true, "consultant": true, "representative": true,
	"rep": true, "agent": true, "assistant": true, "professional": true,
	"expert": true, "generalist": true,
	// filler
	"of": true, "the": true, "and": true, "for": true, "in": true, "to": true,
	"a": true, "an": true, "with": true, "at": true, "on": true,
}

// Expand returns the raw terms plus mechanically derived widenings, deduped and
// capped, preserving first-seen order. For each term it emits:
//   - the normalized full phrase (the precision anchor; preserves today's match)
//   - the discriminating "core" (phrase minus generic words), when it differs
//   - the trailing head noun of a multi-word core (e.g. "site reliability" ->
//     "reliability"), when specific enough
func Expand(terms []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if len(s) < minToken || seen[s] {
			return
		}
		seen[s] = true
		if len(out) < MaxTerms {
			out = append(out, s)
		}
	}
	for _, raw := range terms {
		toks := tokens(raw)
		if len(toks) == 0 {
			continue
		}
		add(strings.Join(toks, " "))

		var content []string
		for _, t := range toks {
			if !generic[t] && len(t) >= minToken {
				content = append(content, t)
			}
		}
		if len(content) == 0 || len(content) == len(toks) {
			continue // nothing generic to strip
		}
		add(strings.Join(content, " "))
		if len(content) >= 2 {
			if head := content[len(content)-1]; len(head) >= minHead {
				add(head)
			}
		}
	}
	return out
}

// tokens lowercases and splits on any non-alphanumeric run, mirroring the
// worker's tokenization so derived terms match the same way titles are scanned.
func tokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
}
