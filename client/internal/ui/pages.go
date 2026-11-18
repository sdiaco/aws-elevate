package ui

import (
	"io/fs"
	"net/http"
)

// staticFiles serves the style sheet and the script from the executable.
func staticFiles() http.Handler {
	static, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return http.StripPrefix("/static/", http.FileServerFS(static))
}
