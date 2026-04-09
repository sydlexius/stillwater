// Package static embeds static web assets (CSS, JavaScript, fonts, images)
// into the binary at build time so the application can run as a standalone
// binary without requiring the web/static directory on disk.
package static

import "embed"

// FS contains all static assets embedded at compile time.
// The directory directive includes everything under this directory except
// Go source files.
//
//go:embed all:css all:fonts all:img all:js site.webmanifest
var FS embed.FS
