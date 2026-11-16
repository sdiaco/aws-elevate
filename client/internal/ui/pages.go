package ui

import (
	"io"
	"net/http"
)

// Pages are the views of the interface.
func Pages() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, "<!doctype html><title>aws-elevate</title><h1>aws-elevate</h1>")
	})
	return mux
}
