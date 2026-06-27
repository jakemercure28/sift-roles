package dashboard

import (
	"sort"
	"strings"
)

// normalizeCompanyTag lowercases, trims, and collapses whitespace, mirroring
// normalizeCompanyTag in lib/company-tags.js.
func normalizeCompanyTag(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

// parseCompanyTags splits a comma-separated string into normalized, de-duplicated,
// sorted tags. Tags are already lowercased, so a byte-wise sort matches the
// case-insensitive en sort used in lib/company-tags.js.
func parseCompanyTags(value string) []string {
	seen := make(map[string]struct{})
	var tags []string
	for _, raw := range strings.Split(value, ",") {
		tag := normalizeCompanyTag(raw)
		if tag == "" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags
}

// serializeCompanyTags normalizes and rejoins tags as "a, b, c".
func serializeCompanyTags(value string) string {
	return strings.Join(parseCompanyTags(value), ", ")
}
