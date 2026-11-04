// Package web holds the UI, embedded into the binary.
package web

import "embed"

//go:embed index.html favicon.png
var FS embed.FS
