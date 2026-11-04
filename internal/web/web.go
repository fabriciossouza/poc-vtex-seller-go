// Package web serves the demo panel, the API documentation page (vendored
// Swagger UI, no CDN) and the OpenAPI specification.
package web

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/fabriciossouza/poc-vtex-seller-go/api"
)

//go:embed static swagger
var files embed.FS

func Handler() http.Handler {
	static, _ := fs.Sub(files, "static")
	swagger, _ := fs.Sub(files, "swagger")

	page := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { http.ServeFileFS(w, r, static, name) }
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", page("index.html"))
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServerFS(static)))
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("GET /docs/{$}", page("docs.html"))
	mux.HandleFunc("GET /docs/swagger-initializer.js", page("swagger-initializer.js"))
	mux.HandleFunc("GET /docs/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		_, _ = w.Write(api.Spec)
	})
	mux.Handle("GET /docs/swagger/", http.StripPrefix("/docs/swagger/", http.FileServerFS(swagger)))
	return mux
}
