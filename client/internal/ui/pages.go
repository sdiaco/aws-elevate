package ui

import (
	"io/fs"
	"net/http"
)

// Pages are the views of the interface.
func Pages() http.Handler {
	static, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		if err := renderPage(w, "home", Page{Title: "aws-elevate"}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	return mux
}
