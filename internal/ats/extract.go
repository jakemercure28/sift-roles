package ats

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

var (
	reMailto        = regexp.MustCompile(`(?i)^mailto:`)
	reJSONLd        = regexp.MustCompile(`(?is)<script[^>]+type=["']application/ld(?:\+|&#x2B;)json["'][^>]*>(.*?)</script>`)
	reBuiltinInit   = regexp.MustCompile(`(?s)Builtin\.jobPostInit\((\{.*?\})\);\s*</script>`)
	reBuiltinLegacy = regexp.MustCompile(`Builtin\.jobPostInit\(\{"job":\{"id":\d+[^}]*"howToApply":"([^"\\]*(?:\\.[^"\\]*)*)"`)
	reMetaRefresh   = regexp.MustCompile(`(?i)http-equiv=["']refresh["'][^>]+content=["'][^"']*url=([^"']+)["']`)
	reHref          = regexp.MustCompile(`(?i)href=["']([^"']+)["']`)
	reRemoteOKID    = regexp.MustCompile(`-(\d+)(?:[/?#]|$)`)
	reCurrentJob    = regexp.MustCompile(`currentJobId=['"](\d+)['"]`)
	reRemoteOKL     = regexp.MustCompile(`(?i)href=["']([^"']*/l/\d+[^"']*)["']`)
	reRemoteOKDom   = regexp.MustCompile(`(?i)remoteok\.com`)
)

// absoluteURL resolves href against baseURL, ported from absoluteUrl in
// lib/ats-resolver.js: decode entities, strip mailto:, then resolve. A
// relative href with no usable base is returned as-is (matching the JS catch).
func absoluteURL(href, baseURL string) string {
	if href == "" {
		return ""
	}
	decoded := strings.TrimSpace(decodeEntities(href))
	if reMailto.MatchString(decoded) {
		return reMailto.ReplaceAllString(decoded, "")
	}
	ref, err := url.Parse(decoded)
	if err != nil {
		return decoded
	}
	if baseURL == "" {
		if !ref.IsAbs() {
			return decoded
		}
		return ref.String()
	}
	base, err := url.Parse(baseURL)
	if err != nil || !base.IsAbs() {
		if ref.IsAbs() {
			return ref.String()
		}
		return decoded
	}
	return base.ResolveReference(ref).String()
}

// parseJSONObject decodes entities then parses JSON, returning nil on failure.
// Ported from parseJsonObject in lib/ats-resolver.js.
func parseJSONObject(raw string) any {
	var v any
	if err := json.Unmarshal([]byte(decodeEntities(raw)), &v); err != nil {
		return nil
	}
	return v
}

// extractJSONLd returns the JSON-LD blocks in html (flattening @graph), ported
// from extractJsonLd in lib/ats-resolver.js.
func extractJSONLd(html string) []any {
	var blocks []any
	for _, m := range reJSONLd.FindAllStringSubmatch(html, -1) {
		data := parseJSONObject(m[1])
		if data == nil {
			continue
		}
		switch d := data.(type) {
		case map[string]any:
			if g, ok := d["@graph"]; ok {
				if arr, ok := g.([]any); ok {
					blocks = append(blocks, arr...)
				} else {
					blocks = append(blocks, g)
				}
			} else {
				blocks = append(blocks, d)
			}
		case []any:
			blocks = append(blocks, d...)
		default:
			blocks = append(blocks, data)
		}
	}
	return blocks
}

// findValuesDeep collects all string values under the given keys, recursively.
// Ported from findValuesDeep in lib/ats-resolver.js.
func findValuesDeep(value any, keyNames map[string]bool, results *[]string) {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			findValuesDeep(item, keyNames, results)
		}
	case map[string]any:
		for key, nested := range v {
			if keyNames[key] {
				if s, ok := nested.(string); ok {
					*results = append(*results, s)
				}
			}
			findValuesDeep(nested, keyNames, results)
		}
	}
}

// extractBuiltInApplyURL pulls the howToApply URL out of a Built In page's
// jobPostInit payload, ported from extractBuiltInApplyUrl in lib/ats-resolver.js.
func extractBuiltInApplyURL(html string) string {
	if m := reBuiltinInit.FindStringSubmatch(html); m != nil {
		if data, ok := parseJSONObject(m[1]).(map[string]any); ok {
			if job, ok := data["job"].(map[string]any); ok {
				if howTo, ok := job["howToApply"].(string); ok && strings.TrimSpace(howTo) != "" {
					return decodeEntities(howTo)
				}
			}
		}
	}
	if m := reBuiltinLegacy.FindStringSubmatch(html); m != nil {
		return decodeEntities(m[1])
	}
	return ""
}

// extractRemoteOkApplyURLs derives RemoteOK's /l/<id> redirect links, ported
// from extractRemoteOkApplyUrls in lib/ats-resolver.js.
func extractRemoteOkApplyURLs(html, sourceURL string) []string {
	var urls []string
	var id string
	if m := reRemoteOKID.FindStringSubmatch(sourceURL); m != nil {
		id = m[1]
	} else if m := reCurrentJob.FindStringSubmatch(html); m != nil {
		id = m[1]
	}
	if id != "" {
		urls = append(urls, absoluteURL("/l/"+id, sourceURL))
	}
	for _, m := range reRemoteOKL.FindAllStringSubmatch(html, -1) {
		urls = append(urls, absoluteURL(m[1], sourceURL))
	}
	return urls
}

// extractCandidateURLs gathers every plausible apply/canonical URL from a page
// (Built In payload, meta-refresh, JSON-LD, anchors, RemoteOK links), resolves
// them against sourceURL, and de-duplicates preserving order. Ported from
// extractCandidateUrls in lib/ats-resolver.js.
func extractCandidateURLs(html, sourceURL string) []string {
	var urls []string
	if builtIn := extractBuiltInApplyURL(html); builtIn != "" {
		urls = append(urls, absoluteURL(builtIn, sourceURL))
	}
	if m := reMetaRefresh.FindStringSubmatch(html); m != nil {
		urls = append(urls, absoluteURL(m[1], sourceURL))
	}
	jsonLdKeys := map[string]bool{"url": true, "sameAs": true, "applyUrl": true, "applicationUrl": true}
	for _, item := range extractJSONLd(html) {
		var found []string
		findValuesDeep(item, jsonLdKeys, &found)
		urls = append(urls, found...)
	}
	for _, m := range reHref.FindAllStringSubmatch(html, -1) {
		urls = append(urls, m[1])
	}
	if reRemoteOKDom.MatchString(sourceURL) {
		urls = append(urls, extractRemoteOkApplyURLs(html, sourceURL)...)
	}

	seen := map[string]bool{}
	var out []string
	for _, u := range urls {
		abs := absoluteURL(u, sourceURL)
		if abs == "" || seen[abs] {
			continue
		}
		seen[abs] = true
		out = append(out, abs)
	}
	return out
}
