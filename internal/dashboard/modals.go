package dashboard

import (
	_ "embed"
	"strings"
)

// modalsHTML is the static modal markup, embedded as data. It is fully static
// (the COLORS substitutions resolve to fixed hex), so it is stored as data and
// verified in context by the full-page golden. The one dynamic point is the
// <!--SIGN-OUT--> marker, replaced by renderModals.
//
//go:embed modals.html
var modalsHTML string

// signOutButton holds the settings-modal account actions emitted only in hosted
// mode: Sign out, then Delete account. The Sign out button's margin-right:auto
// pins the group to the left so Close stays on the right. Both call functions
// defined in public/js (dashboardSignOut in auth.js, settingsDeleteAccount in
// settings.js), so they are gated to hosted auth where those scripts load and
// per-tenant deletion is meaningful.
const signOutButton = `<button class="btn" style="background:transparent;color:var(--text-muted);border:1px solid var(--border);margin-right:auto" onclick="dashboardSignOut()">Sign out</button>` +
	`<button class="btn" style="background:transparent;color:var(--red);border:1px solid var(--border)" onclick="settingsDeleteAccount()">Delete account</button>` + "\n      "

// renderModals returns the modal markup, injecting the Sign out button only in
// hosted auth mode. In self-host the <!--SIGN-OUT--> marker is stripped, leaving
// the markup byte-for-byte identical (so the page golden still holds).
func renderModals(v PageView) string {
	signOut := ""
	if v.AuthEnabled {
		signOut = signOutButton
	}
	out := strings.Replace(modalsHTML, "<!--SIGN-OUT-->", signOut, 1)
	return strings.ReplaceAll(out, "{{BRAND}}", BrandName)
}
