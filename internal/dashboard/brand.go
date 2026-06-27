package dashboard

// BrandName is the single source of truth for the product's display name. It
// drives the sidebar wordmark, the browser tab title, the onboarding modal, the
// report-problem page title, and the hosted login screen. To rename the product,
// change this one constant (and rerun UPDATE_GOLDENS=1 go test ./internal/dashboard).
//
// The display name is intentionally decoupled from the domain and the repo name,
// so renaming here does not touch hosting, DNS, or git remotes.
const BrandName = "Sift"
