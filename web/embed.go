// Package web embeds the single-page web UI.
package web

import (
	"embed"
	"io/fs"
)

//go:embed index.html assets
var content embed.FS

// FS returns the UI file system rooted at the web directory.
func FS() fs.FS { return content }
