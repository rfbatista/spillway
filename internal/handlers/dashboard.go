package handlers

import (
	"log"
	"net/http"
	"path/filepath"
	"proxy/view"
	"text/template"
)

func DashboardPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmplPath := filepath.Join("request_dashboard.html")

	// Parse template
	tmpl, err := template.ParseFS(view.ViewTemplates, tmplPath)
	if err != nil {
		http.Error(w, "Error loading template", http.StatusInternalServerError)
		log.Println("parse error:", err)
		return
	}

	data := map[string]any{
		"Title": "Request Dashboard",
	}

	// Render template
	err = tmpl.Execute(w, data)
	if err != nil {
		http.Error(w, "Error rendering template", http.StatusInternalServerError)
		log.Println("execute error:", err)
	}
}
