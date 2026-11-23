// Package web holds the files of the web interface: one page, its script, its style
// sheet and the dictionary of its two languages. They are plain files, written by hand,
// compiled into the program; the page loads nothing from anywhere else.
package web

import "embed"

// Files are the files of the web interface; index.html is the page.
//
//go:embed index.html app.js style.css i18n.json
var Files embed.FS
