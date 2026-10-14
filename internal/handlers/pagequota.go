package handlers

import (
	"log"
	"net/http"
	"path/filepath"
	"text/template"

	"github.com/rfbatista/spillway/view"
)

func QuotaPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmplPath := filepath.Join("quota.html")

	// Parse template
	tmpl, err := template.ParseFS(view.ViewTemplates, tmplPath)
	if err != nil {
		http.Error(w, "Error loading template", http.StatusInternalServerError)
		log.Println("parse error:", err)
		return
	}

	// Example data (can be struct/map)
	data := map[string]any{
		"Title": "Quota Usage",
		"Value": 42,
	}

	// Render template
	err = tmpl.Execute(w, data)
	if err != nil {
		http.Error(w, "Error rendering template", http.StatusInternalServerError)
		log.Println("execute error:", err)
	}
}
