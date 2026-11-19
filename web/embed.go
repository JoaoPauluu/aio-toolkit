// Package web holds the embedded single-page UI.
package web

import "embed"

// FS contains static/ (index.html, app.js, style.css, aio-sdk.js, vendor/).
//
//go:embed static
var FS embed.FS
