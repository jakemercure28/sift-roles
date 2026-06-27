package dashboard

import "net/url"

const brandMarkSVG = `<svg class="brand-mark sidebar-logo" width="34" height="34" viewBox="0 0 64 64" fill="none" xmlns="http://www.w3.org/2000/svg" aria-hidden="true" focusable="false" shape-rendering="crispEdges"><path d="M7 9h50v10H7zM13 21h38v8h-6v7h-7v16H26V36h-7v-7h-6z" fill="currentColor"/><path d="M17 13h7v3h-7zM29 13h7v3h-7zM41 13h7v3h-7zM24 25h16v4H24z" fill="var(--brand-cutout)"/></svg>`

const brandWordmarkSVG = `<svg class="brand-wordmark" width="212" height="58" viewBox="0 0 212 58" fill="none" xmlns="http://www.w3.org/2000/svg" aria-hidden="true" focusable="false"><g shape-rendering="crispEdges"><path d="M8 10h42v8H8zM13 20h32v7h-5v6h-6v13H24V33h-6v-6h-5z" fill="currentColor"/><path d="M17 13h6v3h-6zM28 13h6v3h-6zM39 13h6v3h-6zM22 23h14v4H22z" fill="var(--brand-cutout)"/></g><text x="58" y="45" fill="currentColor" font-family="Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif" font-size="48" font-style="italic" font-weight="800" letter-spacing="3">sift</text></svg>`

const brandFaviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" shape-rendering="crispEdges"><rect width="64" height="64" rx="12" fill="#f4f4f5"/><path d="M7 9h50v10H7zM13 21h38v8h-6v7h-7v16H26V36h-7v-7h-6z" fill="#18181b"/><path d="M17 13h7v3h-7zM29 13h7v3h-7zM41 13h7v3h-7zM24 25h16v4H24z" fill="#f4f4f5"/></svg>`

const brandFaviconPrefix = `<link rel="icon" type="image/svg+xml" href="data:image/svg+xml,`

func BrandMarkSVG() string {
	return brandMarkSVG
}

func BrandWordmarkSVG() string {
	return brandWordmarkSVG
}

func BrandFaviconLink() string {
	return brandFaviconPrefix + url.PathEscape(brandFaviconSVG) + `">`
}
