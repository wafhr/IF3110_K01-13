package web

import (
	"io/fs"
	"net/http"

	"github.com/wafhr/IF3110_K01-13/internal/access"
)

// NewHandler serves browser pages and static assets. Authentication and CSRF
// checks remain in access.HTTPHandler so the UI uses the same policy as the API.
func NewHandler(service *access.Service, auth *access.HTTPHandler) http.Handler {
	mux := http.NewServeMux()
	static, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /login", loginPage)
	mux.Handle("GET /{$}", auth.RequireBrowserPage(false, false, http.HandlerFunc(homePage)))
	mux.Handle("GET /manage/users", auth.RequireBrowserPage(true, false, http.HandlerFunc(usersPage(service))))
	mux.Handle("POST /manage/users", auth.RequireBrowserPage(true, true, http.HandlerFunc(createUser(service))))
	mux.Handle("POST /manage/users/{userID}", auth.RequireBrowserPage(true, true, http.HandlerFunc(updateUser(service))))
	mux.Handle("GET /manage/groups", auth.RequireBrowserPage(true, false, http.HandlerFunc(groupsPage(service))))
	mux.Handle("GET /manage/databases", auth.RequireBrowserPage(true, false, http.HandlerFunc(databasesPage)))
	mux.Handle("GET /workspace/databases/{databaseID}", auth.RequireBrowserPage(false, false, http.HandlerFunc(databaseWorkspacePage)))
	mux.Handle("GET /workspace/databases/{databaseID}/migrations", auth.RequireBrowserPage(false, false, http.HandlerFunc(migrationWorkspacePage)))
	mux.Handle("GET /workspace/databases/{databaseID}/migrations/{version}", auth.RequireBrowserPage(false, false, http.HandlerFunc(migrationWorkspacePage)))
	mux.Handle("POST /manage/groups", auth.RequireBrowserPage(true, true, http.HandlerFunc(createGroup(service))))
	mux.Handle("POST /manage/groups/{groupID}/delete", auth.RequireBrowserPage(true, true, http.HandlerFunc(deleteGroup(service))))
	mux.Handle("POST /manage/groups/{groupID}/members", auth.RequireBrowserPage(true, true, http.HandlerFunc(addMember(service))))
	mux.Handle("POST /manage/groups/{groupID}/members/{userID}/delete", auth.RequireBrowserPage(true, true, http.HandlerFunc(removeMember(service))))
	mux.Handle("POST /manage/groups/{groupID}/databases", auth.RequireBrowserPage(true, true, http.HandlerFunc(setPermission(service))))
	mux.Handle("POST /manage/groups/{groupID}/databases/{databaseID}/delete", auth.RequireBrowserPage(true, true, http.HandlerFunc(deletePermission(service))))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	})
}
