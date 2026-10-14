package handlers

import (
	"log"
	"net/http"
	"path/filepath"
	"text/template"

	"github.com/rfbatista/spillway/internal/proxy"
	"github.com/rfbatista/spillway/view"
)

func QuotaStatusPage(getPool func() *proxy.BackendPool) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		tmplPath := filepath.Join("quota_status.html")

		tmpl, err := template.ParseFS(view.ViewTemplates, tmplPath)
		if err != nil {
			http.Error(w, "Error loading template", http.StatusInternalServerError)
			log.Println("parse error:", err)
			return
		}

		pool := getPool()
		var backends any
		if pool != nil {
			backends, _ = pool.GetBackends()
		}

		data := map[string]any{
			"Title":    "Quota Usage",
			"backends": backends,
		}

		err = tmpl.Execute(w, data)
		if err != nil {
			http.Error(w, "Error rendering template", http.StatusInternalServerError)
			log.Println("execute error:", err)
		}
	}
}
