// Package ui embeds the static, build-free dashboard assets.
package ui

import "embed"

// FS holds index.html, style.css and app.js at its root.
//
//go:embed index.html style.css app.js
var FS embed.FS
