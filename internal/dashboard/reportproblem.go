package dashboard

import (
	_ "embed"
	"net/http"
	"strings"
)

// The report-problem page is static except the inlined theme. It is stored split
// at the theme block (head ends at <style id="theme-vars">, tail starts at
// </style>) so Go inserts a live RenderThemeCSS().

//go:embed report-problem-head.html
var reportProblemHead string

//go:embed report-problem-tail.html
var reportProblemTail string

// RenderReportProblemPage ports renderReportProblemPage.
func RenderReportProblemPage() string {
	page := reportProblemHead + RenderThemeCSS() + reportProblemTail
	return strings.ReplaceAll(page, "{{BRAND}}", BrandName)
}

func (s *Server) handleReportProblem(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write([]byte(RenderReportProblemPage()))
}
