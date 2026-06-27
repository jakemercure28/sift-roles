package dashboard

import (
	"testing"

	"job-search-automation/internal/db"
)

// TestRenderPageParity renders the full page-all scenario and asserts byte
// equality with the committed .golden fixture (the renderer's source of truth).
func TestRenderPageParity(t *testing.T) {
	body := RenderJobTable(nil, map[string]int{}, map[string][]string{}, "all", "score", nil, SearchOptions{})
	page := RenderPage(PageView{
		Filter:     "all",
		Sort:       "score",
		Search:     SearchOptions{},
		Stats:      db.Stats{},
		Prefs:      LocationPrefs{Metros: []string{}, IncludeUnknown: true, RemoteOnly: false},
		Heartbeat:  nil,
		BodyHTML:   body,
	})
	assertGolden(t, "page-all.html.golden", page)
}
