package web

import (
	"net/http"

	"github.com/wafhr/IF3110_K01-13/internal/access"
)

type pageData struct {
	Title     string
	User      access.User
	CSRFToken string
	Notice    string
	Users     []access.User
	Groups    []access.Group
	Members   []access.GroupMember
	Grants    []access.DatabaseGrant
	GroupID   string
	GroupName string
	IsAdmin   bool
}

func loginPage(w http.ResponseWriter, r *http.Request) {
	render(w, "login", pageData{Title: "Sign in", Notice: noticeText(r.URL.Query().Get("notice"))})
}

func homePage(w http.ResponseWriter, r *http.Request) {
	user := access.CurrentUser(r.Context())
	render(w, "home", pageData{Title: "Workspace", User: user, CSRFToken: access.CSRFToken(r.Context()), IsAdmin: user.PlatformAdmin})
}

func usersPage(service *access.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		users, err := service.ListUsers(r.Context())
		if err != nil {
			renderError(w)
			return
		}
		render(w, "users", pageData{Title: "Users", User: access.CurrentUser(r.Context()), CSRFToken: access.CSRFToken(r.Context()),
			Users: users, Notice: noticeText(r.URL.Query().Get("notice")), IsAdmin: true})
	}
}

func groupsPage(service *access.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		groups, err := service.ListGroups(ctx)
		if err != nil {
			renderError(w)
			return
		}
		users, err := service.ListUsers(ctx)
		if err != nil {
			renderError(w)
			return
		}
		page := pageData{Title: "Groups", User: access.CurrentUser(ctx), CSRFToken: access.CSRFToken(ctx), Groups: groups, Users: users,
			Notice: noticeText(r.URL.Query().Get("notice")), IsAdmin: true, GroupID: r.URL.Query().Get("group")}
		for _, group := range groups {
			if group.ID == page.GroupID {
				page.GroupName = group.Name
				break
			}
		}
		if page.GroupID != "" {
			page.Members, err = service.ListGroupMembers(ctx, page.GroupID)
			if err == nil {
				page.Grants, err = service.ListDatabaseGrants(ctx, page.GroupID)
			}
			if err != nil {
				page.GroupID, page.GroupName = "", ""
				page.Notice = noticeText("group-error")
			}
		}
		render(w, "groups", page)
	}
}
