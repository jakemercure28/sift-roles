// Package ats ports the ATS-resolution pipeline from lib/ats-resolver.js: it
// turns an aggregator ("alternate") job listing into a canonical primary ATS
// posting (Ashby/Greenhouse/Lever/Workday) by parsing URLs, fetching ATS APIs,
// searching boards, and (as a fallback) proposing candidates via Gemini. This
// file holds the text helpers ported from lib/utils.js.
package ats

import (
	"regexp"
	"strconv"
	"strings"
)

// htmlEntityMap mirrors HTML_ENTITY_MAP in lib/utils.js.
var htmlEntityMap = map[string]string{
	"amp": "&", "lt": "<", "gt": ">", "quot": "\"", "apos": "'",
	"nbsp": " ", "mdash": "—", "ndash": "–",
	"ldquo": "“", "rdquo": "”", "lsquo": "‘", "rsquo": "’",
	"hellip": "…", "bull": "•", "middot": "·",
	"copy": "©", "reg": "®", "trade": "™",
}

var (
	reUnicodeEscape = regexp.MustCompile(`(?i)\\u([0-9a-f]{4})`)
	reNamedEntity   = regexp.MustCompile(`(?i)&([a-z]+);`)
	reDecEntity     = regexp.MustCompile(`&#(\d+);`)
	reHexEntity     = regexp.MustCompile(`(?i)&#x([0-9a-f]+);`)
)

// decodeEntities decodes \uXXXX escapes and HTML entities, ported verbatim from
// decodeEntities in lib/utils.js. It runs up to three passes so inputs like
// "&amp;amp;" collapse fully, stopping early once stable.
func decodeEntities(s string) string {
	decoded := s
	for i := 0; i < 3; i++ {
		next := reUnicodeEscape.ReplaceAllStringFunc(decoded, func(m string) string {
			h := reUnicodeEscape.FindStringSubmatch(m)[1]
			n, err := strconv.ParseInt(h, 16, 32)
			if err != nil {
				return m
			}
			return string(rune(n))
		})
		next = reNamedEntity.ReplaceAllStringFunc(next, func(m string) string {
			entity := reNamedEntity.FindStringSubmatch(m)[1]
			if repl, ok := htmlEntityMap[strings.ToLower(entity)]; ok {
				return repl
			}
			return m
		})
		next = reDecEntity.ReplaceAllStringFunc(next, func(m string) string {
			d := reDecEntity.FindStringSubmatch(m)[1]
			n, err := strconv.Atoi(d)
			if err != nil {
				return m
			}
			return string(rune(n))
		})
		next = reHexEntity.ReplaceAllStringFunc(next, func(m string) string {
			h := reHexEntity.FindStringSubmatch(m)[1]
			n, err := strconv.ParseInt(h, 16, 32)
			if err != nil {
				return m
			}
			return string(rune(n))
		})
		if next == decoded {
			break
		}
		decoded = next
	}
	return decoded
}

var reHTMLTag = regexp.MustCompile(`<[^>]+>`)
var reMultiSpace = regexp.MustCompile(`\s{2,}`)

// maxDescriptionLength mirrors MAX_DESCRIPTION_LENGTH in config/constants.js.
const maxDescriptionLength = 15000

// stripHTML decodes entities, strips tags, collapses whitespace, and truncates,
// ported from stripHtml in lib/utils.js. The mojibake-repair fast path in the
// JS version is skipped: ATS API payloads are already valid UTF-8. Truncation is
// rune-based so a multibyte character is never split.
func stripHTML(text string) string {
	decoded := decodeEntities(text)
	decoded = reHTMLTag.ReplaceAllString(decoded, " ")
	decoded = reMultiSpace.ReplaceAllString(decoded, " ")
	decoded = strings.TrimSpace(decoded)
	if runes := []rune(decoded); len(runes) > maxDescriptionLength {
		decoded = string(runes[:maxDescriptionLength])
	}
	return decoded
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
