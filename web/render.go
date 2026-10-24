package web

import (
	"embed"
	"html/template"
	"log/slog"
	"net/http"
)

//go:embed templates static
var assets embed.FS

func render(w http.ResponseWriter, name string, page pageData) {
	t, err := template.ParseFS(assets,
		"templates/layouts/base.html",
		"templates/partials/header.html",
		"templates/access/"+name+".html",
	)
	if err != nil {
		renderError(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", page); err != nil {
		renderError(w)
	}
}

func noticeText(code string) string {
	return map[string]string{
		"login-error":       "Login gagal. Periksa username dan password.",
		"user-created":      "User berhasil dibuat.",
		"user-updated":      "User berhasil diperbarui.",
		"user-error":        "Perubahan user gagal. Periksa input dan pastikan username unik.",
		"self-update-error": "Akun admin yang sedang digunakan tidak dapat dinonaktifkan atau diturunkan perannya.",
		"group-created":     "Group berhasil dibuat.",
		"group-deleted":     "Group berhasil dihapus.",
		"group-error":       "Operasi group gagal. Periksa nama dan pilihan group.",
		"member-updated":    "Keanggotaan group berhasil diperbarui.",
		"member-error":      "Keanggotaan group gagal diperbarui.",
		"grant-updated":     "Grant database berhasil diperbarui.",
		"grant-error":       "Grant gagal diperbarui. Pastikan UUID database dan permission valid.",
	}[code]
}

func renderError(w http.ResponseWriter) {
	slog.Error("render web page operation failed")
	http.Error(w, "The page could not be loaded.", http.StatusInternalServerError)
}
