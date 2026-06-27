package dashboard

import (
	"strings"
	"text/template"
)

// This file ports lib/theme.js: the single source of truth for dashboard colors.
// RenderThemeCSS emits the exact CSS custom properties the stylesheet references,
// and ClientThemeJSON emits the window.__THEME__ subset. Both are verified
// byte-for-byte against the Node output (theme_test.go golden files).

// themeShared holds the mode-independent values (SHARED in theme.js).
type themeShared struct {
	NeutralTintRgb                                         string
	Green, GreenRgb, Red, RedRgb, Amber, AmberRgb, Blue    string
	Emerald, EmeraldRgb, Slate, SlateLight                 string
	AtsGreenhouse, AtsGreenhouseRgb, AtsAshby, AtsAshbyRgb string
	AtsLever, AtsLeverRgb, AtsWorkday, AtsWorkdayRgb       string
	ScoreGood, ScoreBorderline, ScoreWeak                  string
	Highlight, HighlightInk                                string
	Radius, RadiusSm, RadiusXs, FontMono                   string
	Space2, Space4, SpaceXs, Space, SpaceM, SpaceL         string
	SpaceXl, SpaceXxl                                      string
	TextMeta, TextLabel, TextBody, TextTiny, TextSmall     string
	TextH4, TextH3, TextH2, TextH1                         string
	FontRegular, FontMedium, FontSemibold, FontBold        string
	LeadTight, LeadBody                                    string
}

// themeMode holds the per-mode values (LIGHT / DARK in theme.js).
type themeMode struct {
	BgPrimary, BgSecondary, BgCard, BgCardHover          string
	BgPageGradient, BgGradient                           string
	Text1, Text2, Text3, Text4, Text5                    string
	Border, BorderHover                                  string
	PrimarySolid, Accent, AccentRgb, AccentStrong        string
	ScoreGood, ScoreBorderline, ScoreWeak                string
	Score10Bg, Score10Ink, Score9Bg, Score9Ink           string
	Score8Bg, Score8Ink                                  string
	TintRgb, InkRgb, BarBg, BarSubBg, OverlayBg, PanelBg string
	Inverse, ShadowRgb, GlassBg, GlassBorder             string
}

var sharedVals = themeShared{
	NeutralTintRgb: "128, 128, 128",
	Green:          "#2EA043", GreenRgb: "46, 160, 67",
	Red: "#DC4B43", RedRgb: "220, 75, 67",
	Amber: "#D08A2E", AmberRgb: "208, 138, 46",
	Blue:    "#3E7BD6",
	Emerald: "#16A06B", EmeraldRgb: "22, 160, 107",
	Slate: "#6B7280", SlateLight: "#8A8A8A",
	AtsGreenhouse: "#4F7D5E", AtsGreenhouseRgb: "79, 125, 94",
	AtsAshby: "#6E66A6", AtsAshbyRgb: "110, 102, 166",
	AtsLever: "#9C7E3A", AtsLeverRgb: "156, 126, 58",
	AtsWorkday: "#5470A0", AtsWorkdayRgb: "84, 112, 160",
	ScoreGood: "#9C9C9C", ScoreBorderline: "#8A8A8A", ScoreWeak: "#A8A8A8",
	Highlight: "#F9EA4F", HighlightInk: "#000000",
	Radius: "8px", RadiusSm: "6px", RadiusXs: "4px",
	FontMono: "'SF Mono', 'Fira Code', 'JetBrains Mono', ui-monospace, Menlo, Consolas, monospace",
	Space2:   "2px", Space4: "4px", SpaceXs: "8px", Space: "16px",
	SpaceM: "24px", SpaceL: "32px", SpaceXl: "48px", SpaceXxl: "80px",
	TextMeta: "11px", TextLabel: "12px", TextBody: "14px", TextTiny: "16px",
	TextSmall: "18px", TextH4: "20px", TextH3: "24px", TextH2: "32px", TextH1: "40px",
	FontRegular: "400", FontMedium: "500", FontSemibold: "600", FontBold: "700",
	LeadTight: "1.3", LeadBody: "1.6",
}

var lightMode = themeMode{
	BgPrimary: "#FFFFFF", BgSecondary: "#F5F5F5", BgCard: "#FFFFFF", BgCardHover: "#F5F5F5",
	BgPageGradient: "#FFFFFF", BgGradient: "#FFFFFF",
	Text1: "#000000", Text2: "#1A1A1A", Text3: "#6B6B6B", Text4: "#999999", Text5: "#B8B8B8",
	Border: "#ECECEC", BorderHover: "#B8B8B8",
	PrimarySolid: "#000000", Accent: "#000000", AccentRgb: "0, 0, 0", AccentStrong: "#000000",
	ScoreGood: "#4A4A4A", ScoreBorderline: "#6B6B6B", ScoreWeak: "#8A8A8A",
	Score10Bg: "#F9EA4F", Score10Ink: "#000000", Score9Bg: "#111111", Score9Ink: "#FFFFFF",
	Score8Bg: "#565656", Score8Ink: "#FFFFFF",
	TintRgb: "0, 0, 0", InkRgb: "26, 26, 26",
	BarBg: "rgba(255, 255, 255, 0.85)", BarSubBg: "rgba(246, 248, 250, 0.75)",
	OverlayBg: "rgba(20, 20, 20, 0.4)", PanelBg: "#FFFFFF", Inverse: "#FFFFFF", ShadowRgb: "20, 20, 20",
	GlassBg: "rgba(0, 0, 0, 0.02)", GlassBorder: "rgba(0, 0, 0, 0.07)",
}

var darkMode = themeMode{
	BgPrimary: "#161B22", BgSecondary: "#161B22", BgCard: "#161B22", BgCardHover: "#1C2128",
	BgPageGradient: "#0D1117", BgGradient: "#0D1117",
	Text1: "#E6EDF3", Text2: "#ADBAC7", Text3: "#9198A1", Text4: "#6E7681", Text5: "#484F58",
	Border: "#30363D", BorderHover: "#444C56",
	PrimarySolid: "#C8C8C8", Accent: "#E6EDF3", AccentRgb: "230, 237, 243", AccentStrong: "#E6EDF3",
	ScoreGood: "#9C9C9C", ScoreBorderline: "#8A8A8A", ScoreWeak: "#A8A8A8",
	Score10Bg: "#F9EA4F", Score10Ink: "#000000", Score9Bg: "#ECECEC", Score9Ink: "#0D1117",
	Score8Bg: "#565656", Score8Ink: "#FFFFFF",
	TintRgb: "255, 255, 255", InkRgb: "236, 236, 236",
	BarBg: "rgba(13, 17, 23, 0.92)", BarSubBg: "rgba(13, 17, 23, 0.7)",
	OverlayBg: "rgba(1, 4, 9, 0.8)", PanelBg: "#161B22", Inverse: "#0D1117", ShadowRgb: "0, 0, 0",
	GlassBg: "rgba(255, 255, 255, 0.03)", GlassBorder: "rgba(255, 255, 255, 0.10)",
}

// themeVarsTmpl is the verbatim themeVars() template from theme.js, with ${m.x}
// rewritten to {{.M.X}}, ${s.x} to {{.S.X}}, and the score-ramp fallbacks to {{or}}.
const themeVarsTmpl = `
  /* Surfaces */
  --bg-primary: {{.M.BgPrimary}};
  --bg-secondary: {{.M.BgSecondary}};
  --bg-card: {{.M.BgCard}};
  --bg-card-hover: {{.M.BgCardHover}};
  --bg-page: {{.M.BgPageGradient}};
  --bg-gradient: {{.M.BgGradient}};

  /* Text */
  --text-primary: {{.M.Text1}};
  --text-secondary: {{.M.Text2}};
  --text-muted: {{.M.Text3}};
  --text-dim: {{.M.Text4}};
  --text-faint: {{.M.Text5}};

  /* Borders */
  --border: {{.M.Border}};
  --border-hover: {{.M.BorderHover}};

  /* Primary family is NEUTRAL now (the chrome is monochrome). Solids resolve to
     a mid neutral; the rgb channel is mid-gray so all the legacy
     rgba(var(--primary-rgb), a) washes render as subtle neutral overlays in both
     modes. The old purple aliases (--lavender/--violet) point here too. */
  --primary: {{.M.PrimarySolid}};
  --primary-rgb: {{.S.NeutralTintRgb}};
  --primary-strong: {{.M.PrimarySolid}};
  --primary-strong-rgb: {{.S.NeutralTintRgb}};
  --primary-deep: {{.M.PrimarySolid}};
  --primary-deep-rgb: {{.S.NeutralTintRgb}};
  --primary-light: {{.M.Text2}};
  --primary-light-rgb: {{.S.NeutralTintRgb}};
  --primary-indigo: {{.M.PrimarySolid}};
  --lavender: {{.M.Text2}};
  --violet: {{.M.Text2}};
  --violet-rgb: {{.S.NeutralTintRgb}};

  /* Accent (GitHub green: #1F883D on light, #3FB950 on dark). Reserved for
     active states, focus, primary actions, progress, and salary. */
  --accent-lime: {{.M.Accent}};
  --accent-lime-rgb: {{.M.AccentRgb}};
  --accent-lime-strong: {{.M.AccentStrong}};
  --accent-lime-soft: rgba({{.M.AccentRgb}}, 0.12);
  --accent-lime-glow: rgba({{.M.AccentRgb}}, 0.26);

  /* Focus ring: the ink accent so interaction reads as the brand everywhere */
  --focus-ring: {{.M.Accent}};
  --focus-ring-glow: rgba({{.M.AccentRgb}}, 0.28);

  /* Status */
  --green: {{.S.Green}};
  --green-glow: rgba({{.S.GreenRgb}}, 0.12);
  --red: {{.S.Red}};
  --red-glow: rgba({{.S.RedRgb}}, 0.10);
  --amber: {{.S.Amber}};
  --amber-glow: rgba({{.S.AmberRgb}}, 0.10);
  --blue: {{.S.Blue}};
  --emerald: {{.S.Emerald}};
  --emerald-glow: rgba({{.S.EmeraldRgb}}, 0.15);
  --slate: {{.S.Slate}};
  --slate-light: {{.S.SlateLight}};

  /* ATS source — desaturated mid-tones, mode-independent (legible on both
     surfaces). Secondary workflow signal: recognizable, never loud. */
  --ats-greenhouse: {{.S.AtsGreenhouse}}; --ats-greenhouse-rgb: {{.S.AtsGreenhouseRgb}};
  --ats-ashby: {{.S.AtsAshby}}; --ats-ashby-rgb: {{.S.AtsAshbyRgb}};
  --ats-lever: {{.S.AtsLever}}; --ats-lever-rgb: {{.S.AtsLeverRgb}};
  --ats-workday: {{.S.AtsWorkday}}; --ats-workday-rgb: {{.S.AtsWorkdayRgb}};

  /* Score ramp — tinted chip for scores 7 and below (grays darker in light mode) */
  --score-good: {{or .M.ScoreGood .S.ScoreGood}};
  --score-borderline: {{or .M.ScoreBorderline .S.ScoreBorderline}};
  --score-weak: {{or .M.ScoreWeak .S.ScoreWeak}};

  /* Score-chip ladder — solid filled chips for the top rungs, per-mode so they
     read on both surfaces. 10 (yellow) is the one deliberate pop of color. */
  --highlight: {{.S.Highlight}};
  --highlight-ink: {{.S.HighlightInk}};
  --score-10-bg: {{.M.Score10Bg}}; --score-10-ink: {{.M.Score10Ink}};
  --score-9-bg: {{.M.Score9Bg}}; --score-9-ink: {{.M.Score9Ink}};
  --score-8-bg: {{.M.Score8Bg}}; --score-8-ink: {{.M.Score8Ink}};

  /* Overlay tint channel + per-mode surfaces */
  --tint-rgb: {{.M.TintRgb}};
  --ink-rgb: {{.M.InkRgb}};
  --bar-bg: {{.M.BarBg}};
  --bar-sub-bg: {{.M.BarSubBg}};
  --overlay-bg: {{.M.OverlayBg}};
  --panel-bg: {{.M.PanelBg}};
  --inverse: {{.M.Inverse}};
  --shadow-rgb: {{.M.ShadowRgb}};

  /* Glass + misc */
  --glass-bg: {{.M.GlassBg}};
  --glass-border: {{.M.GlassBorder}};
  --glass-blur: blur(12px);
  --radius: {{.S.Radius}};
  --radius-sm: {{.S.RadiusSm}};
  --radius-xs: {{.S.RadiusXs}};
  --font-mono: {{.S.FontMono}};

  /* Practical UI scales. Mode-independent (like --radius), so they read from
     SHARED. Spacing is an 8pt scale with 2/4px sub-steps for the dense table;
     --page-gutter is the single shared horizontal page margin (header + list
     align to it). Overridden per-breakpoint in dashboard.css. */
  --space-2: {{.S.Space2}};
  --space-4: {{.S.Space4}};
  --space-xs: {{.S.SpaceXs}};
  --space-s: {{.S.Space}};
  --space-m: {{.S.SpaceM}};
  --space-l: {{.S.SpaceL}};
  --space-xl: {{.S.SpaceXl}};
  --space-xxl: {{.S.SpaceXxl}};
  --page-gutter: {{.S.SpaceXl}};
  --feed-measure: 1400px;

  --text-meta: {{.S.TextMeta}};
  --text-label: {{.S.TextLabel}};
  --text-body: {{.S.TextBody}};
  --text-tiny: {{.S.TextTiny}};
  --text-small: {{.S.TextSmall}};
  --text-h4: {{.S.TextH4}};
  --text-h3: {{.S.TextH3}};
  --text-h2: {{.S.TextH2}};
  --text-h1: {{.S.TextH1}};

  --font-regular: {{.S.FontRegular}};
  --font-medium: {{.S.FontMedium}};
  --font-semibold: {{.S.FontSemibold}};
  --font-bold: {{.S.FontBold}};
  --lead-tight: {{.S.LeadTight}};
  --lead-body: {{.S.LeadBody}};

  /* Legacy aliases. --accent marks active/interactive states, so it points at
     the ink accent; the old purple aliases resolve to the neutral primary. */
  --accent: var(--accent-lime);
  --accent-glow: rgba(var(--accent-lime-rgb), 0.12);
  --purple: var(--primary);
  --standard-purple: var(--primary);
  --deep-purple: var(--primary-deep);
  --vibrant-purple: var(--primary-light);
  --muted-indigo: var(--primary-indigo);
  --lime-accent: var(--accent-lime);
  --violet-glow: rgba(var(--violet-rgb), 0.16);`

var themeTmpl = template.Must(template.New("themeVars").Parse(themeVarsTmpl))

func themeVarsFor(m themeMode) string {
	var b strings.Builder
	if err := themeTmpl.Execute(&b, struct {
		M themeMode
		S themeShared
	}{m, sharedVals}); err != nil {
		// The template is static and validated at init; an error is a programmer bug.
		panic(err)
	}
	return b.String()
}

// RenderThemeCSS emits the full theme stylesheet, mirroring renderThemeCss().
// Dark is the base; prefers-color-scheme supplies light on first load; the
// data-theme attribute overrides either once the user toggles.
func RenderThemeCSS() string {
	d := themeVarsFor(darkMode)
	l := themeVarsFor(lightMode)
	return ":root {" + d + "\n}\n" +
		"@media (prefers-color-scheme: light) {\n" +
		"  :root:not([data-theme]) {" + l + "\n  }\n}\n" +
		":root[data-theme=\"light\"] {" + l + "\n}\n" +
		":root[data-theme=\"dark\"] {" + d + "\n}"
}

// ClientThemeJSON emits the window.__THEME__ subset, mirroring clientTheme().
// Built as an ordered literal so it matches Node's JSON.stringify key order.
func ClientThemeJSON() string {
	return `{"pipeline":{` +
		`"":"` + pipelineColors[""] + `",` +
		`"applied":"` + pipelineColors["applied"] + `",` +
		`"phone_screen":"` + pipelineColors["phone_screen"] + `",` +
		`"interview":"` + pipelineColors["interview"] + `",` +
		`"onsite":"` + pipelineColors["onsite"] + `",` +
		`"offer":"` + pipelineColors["offer"] + `",` +
		`"closed":"` + pipelineColors["closed"] + `",` +
		`"rejected":"` + pipelineColors["rejected"] + `",` +
		`"ghosted":"` + pipelineColors["ghosted"] + `"},` +
		`"scores":{"good":"` + sharedVals.ScoreGood + `","borderline":"` + sharedVals.ScoreBorderline + `","weak":"` + sharedVals.ScoreWeak + `"},` +
		`"primary":"` + tokenPrimary + `",` +
		`"accent":"` + tokenAccent + `",` +
		`"success":"` + sharedVals.Green + `",` +
		`"danger":"` + sharedVals.Red + `",` +
		`"warning":"` + sharedVals.Amber + `",` +
		`"neutral":"` + colSlateDark + `"}`
}

// Color constants reused by the HTML helpers (lib/html/helpers.js COLORS, which
// map onto the theme TOKENS) and the pipeline select.
const (
	colSlate     = "#6B7280" // SHARED.slate / gray
	colSlateDark = "#565656" // SHARED.slateDark (TOKENS.neutral)
	colSlateLite = "#8A8A8A" // SHARED.slateLight (TOKENS.neutralLight)
	colBlue      = "#3E7BD6"
	colAmber     = "#D08A2E"
	colGreen     = "#2EA043"
	colRed       = "#DC4B43"
	colInk       = "#000000" // LIGHT.accent (TOKENS.primary/accent)

	tokenPrimary = colInk
	tokenAccent  = colInk
)

// pipelineColors mirrors PIPELINE_COLORS in theme.js (the SHARED-hue map used by
// pipelineColor() and clientTheme()).
var pipelineColors = map[string]string{
	"":             colSlate,
	"applied":      colBlue,
	"phone_screen": "#6366F1", // SHARED.indigo
	"interview":    "#0E9488", // SHARED.teal
	"onsite":       colAmber,
	"offer":        colGreen, // SHARED.greenLight
	"closed":       colSlateLite,
	"rejected":     colRed,
	"ghosted":      colSlate, // SHARED.gray
}
