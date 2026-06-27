package dashboard

import (
	"strings"
	"testing"
)

// The sign-out action is hosted-only: it calls window.dashboardSignOut, which is
// defined solely when auth.js loads (AuthEnabled). In self-host it must not
// appear so the page stays identical to the pre-auth golden.
func TestSidebarSignOutGating(t *testing.T) {
	opts := SearchOptions{}

	hosted := renderSidebar("not-applied", "score", opts, true)
	if !strings.Contains(hosted, "dashboardSignOut()") {
		t.Error("hosted sidebar should include the sign-out action")
	}
	if !strings.Contains(hosted, "nav-item--signout") {
		t.Error("hosted sign-out should carry the nav-item--signout class")
	}
	if strings.Contains(hosted, "API Usage") {
		t.Error("hosted sidebar should not include the API counter")
	}
	if !strings.Contains(hosted, "data-filter=\"archived\"") {
		t.Error("hosted sidebar should keep Archive in the More menu")
	}

	selfHost := renderSidebar("not-applied", "score", opts, false)
	if strings.Contains(selfHost, "dashboardSignOut") {
		t.Error("self-host sidebar must not include the sign-out action")
	}
}
