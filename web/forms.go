package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/wafhr/IF3110_K01-13/internal/access"
)

func createUser(service *access.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil {
			redirectNotice(w, r, "/manage/users", "user-error")
			return
		}
		_, err := service.CreateUser(r.Context(), r.PostForm.Get("username"), r.PostForm.Get("password"), r.PostForm.Get("platform_admin") == "true")
		if err != nil {
			redirectNotice(w, r, "/manage/users", "user-error")
			return
		}
		redirectNotice(w, r, "/manage/users", "user-created")
	}
}

func updateUser(service *access.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil {
			redirectNotice(w, r, "/manage/users", "user-error")
			return
		}
		active, activeErr := strconv.ParseBool(r.PostForm.Get("active"))
		admin, adminErr := strconv.ParseBool(r.PostForm.Get("platform_admin"))
		if activeErr != nil || adminErr != nil {
			redirectNotice(w, r, "/manage/users", "user-error")
			return
		}
		if actor := access.CurrentUser(r.Context()); strings.EqualFold(actor.ID, r.PathValue("userID")) && (!active || !admin) {
			redirectNotice(w, r, "/manage/users", "self-update-error")
			return
		}
		if err := service.UpdateUser(r.Context(), r.PathValue("userID"), active, admin); err != nil {
			redirectNotice(w, r, "/manage/users", "user-error")
			return
		}
		redirectNotice(w, r, "/manage/users", "user-updated")
	}
}

func createGroup(service *access.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil {
			redirectGroups(w, r, "group-error", "")
			return
		}
		group, err := service.CreateGroup(r.Context(), r.PostForm.Get("name"))
		if err != nil {
			redirectGroups(w, r, "group-error", "")
			return
		}
		redirectGroups(w, r, "group-created", group.ID)
	}
}

func deleteGroup(service *access.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := service.DeleteGroup(r.Context(), r.PathValue("groupID")); err != nil {
			redirectGroups(w, r, "group-error", "")
			return
		}
		redirectGroups(w, r, "group-deleted", "")
	}
}

func addMember(service *access.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil || service.SetMembership(r.Context(), r.PathValue("groupID"), r.PostForm.Get("user_id"), true) != nil {
			redirectGroups(w, r, "member-error", r.PathValue("groupID"))
			return
		}
		redirectGroups(w, r, "member-updated", r.PathValue("groupID"))
	}
}

func removeMember(service *access.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := service.SetMembership(r.Context(), r.PathValue("groupID"), r.PathValue("userID"), false); err != nil {
			redirectGroups(w, r, "member-error", r.PathValue("groupID"))
			return
		}
		redirectGroups(w, r, "member-updated", r.PathValue("groupID"))
	}
}

func setPermission(service *access.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil || service.SetDatabasePermission(r.Context(), r.PathValue("groupID"), r.PostForm.Get("database_id"), access.Permission(r.PostForm.Get("permission"))) != nil {
			redirectGroups(w, r, "grant-error", r.PathValue("groupID"))
			return
		}
		redirectGroups(w, r, "grant-updated", r.PathValue("groupID"))
	}
}

func deletePermission(service *access.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := service.DeleteDatabasePermission(r.Context(), r.PathValue("groupID"), r.PathValue("databaseID")); err != nil {
			redirectGroups(w, r, "grant-error", r.PathValue("groupID"))
			return
		}
		redirectGroups(w, r, "grant-updated", r.PathValue("groupID"))
	}
}

func redirectGroups(w http.ResponseWriter, r *http.Request, notice, groupID string) {
	path := "/manage/groups?notice=" + url.QueryEscape(notice)
	if groupID != "" {
		path += "&group=" + url.QueryEscape(groupID)
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

func redirectNotice(w http.ResponseWriter, r *http.Request, path, notice string) {
	http.Redirect(w, r, path+"?notice="+url.QueryEscape(notice), http.StatusSeeOther)
}
