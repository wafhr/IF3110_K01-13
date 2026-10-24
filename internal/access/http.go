package access

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type HTTPHandler struct {
	service      *Service
	secureCookie bool
	mux          *http.ServeMux
}

type principalKey struct{}

func NewHTTPHandler(service *Service, secureCookie bool) *HTTPHandler {
	h := &HTTPHandler{service: service, secureCookie: secureCookie, mux: http.NewServeMux()}
	h.routes()
	return h
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	h.mux.ServeHTTP(w, r)
}

func (h *HTTPHandler) routes() {
	h.mux.HandleFunc("POST /auth/login", h.login)
	h.mux.Handle("GET /auth/me", h.protected(false, false, http.HandlerFunc(h.me)))
	h.mux.Handle("GET /auth/csrf", h.protected(false, false, http.HandlerFunc(h.csrf)))
	h.mux.Handle("POST /auth/logout", h.protected(false, true, http.HandlerFunc(h.logout)))

	h.mux.Handle("GET /admin/users", h.protected(true, false, http.HandlerFunc(h.listUsers)))
	h.mux.Handle("POST /admin/users", h.protected(true, true, http.HandlerFunc(h.createUser)))
	h.mux.Handle("PATCH /admin/users/{userID}", h.protected(true, true, http.HandlerFunc(h.updateUser)))
	h.mux.Handle("GET /admin/groups", h.protected(true, false, http.HandlerFunc(h.listGroups)))
	h.mux.Handle("POST /admin/groups", h.protected(true, true, http.HandlerFunc(h.createGroup)))
	h.mux.Handle("DELETE /admin/groups/{groupID}", h.protected(true, true, http.HandlerFunc(h.deleteGroup)))
	h.mux.Handle("GET /admin/groups/{groupID}/members", h.protected(true, false, http.HandlerFunc(h.listGroupMembers)))
	h.mux.Handle("GET /admin/groups/{groupID}/databases", h.protected(true, false, http.HandlerFunc(h.listDatabaseGrants)))
	h.mux.Handle("PUT /admin/groups/{groupID}/members/{userID}", h.protected(true, true, http.HandlerFunc(h.addMember)))
	h.mux.Handle("DELETE /admin/groups/{groupID}/members/{userID}", h.protected(true, true, http.HandlerFunc(h.removeMember)))
	h.mux.Handle("PUT /admin/groups/{groupID}/databases/{databaseID}", h.protected(true, true, http.HandlerFunc(h.setPermission)))
	h.mux.Handle("DELETE /admin/groups/{groupID}/databases/{databaseID}", h.protected(true, true, http.HandlerFunc(h.deletePermission)))
	h.mux.Handle("GET /databases/{databaseID}/access", h.protected(false, false, http.HandlerFunc(h.databaseAccess)))
}

// RequireDatabaseAccess can protect downstream schema, data, or migration handlers.
// The handler route must include a {databaseID} path value.
func (h *HTTPHandler) RequireDatabaseAccess(minimum Permission, next http.Handler) http.Handler {
	return h.protected(false, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if minimum != ViewOnly && minimum != ViewEdit {
			writeServiceError(w, ErrInvalidInput)
			return
		}
		user := currentUser(r.Context())
		permission, err := h.service.EffectivePermission(r.Context(), user.ID, r.PathValue("databaseID"))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		if permission == "" || (minimum == ViewEdit && permission != ViewEdit) {
			writeServiceError(w, ErrForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), databasePermissionKey{}, permission)
		next.ServeHTTP(w, r.WithContext(ctx))
	}))
}

// RequireAuthenticated makes the session principal available to user-facing APIs.
func (h *HTTPHandler) RequireAuthenticated(next http.Handler) http.Handler {
	return h.protected(false, true, next)
}

// UserFromContext returns only the principal set by the authentication middleware.
func UserFromContext(ctx context.Context) (User, bool) {
	user, ok := ctx.Value(principalKey{}).(User)
	return user, ok
}

type databasePermissionKey struct{}

// DatabasePermissionFromContext reports the permission verified for the current database request.
func DatabasePermissionFromContext(ctx context.Context) Permission {
	permission, _ := ctx.Value(databasePermissionKey{}).(Permission)
	return permission
}

// RequirePlatformAdmin protects platform configuration endpoints.
func (h *HTTPHandler) RequirePlatformAdmin(csrfRequired bool, next http.Handler) http.Handler {
	return h.protected(true, csrfRequired, next)
}

func (h *HTTPHandler) protected(admin, csrfRequired bool, next http.Handler) http.Handler {
	return h.protectedWithRedirect(admin, csrfRequired, next, false)
}

// RequireBrowserPage protects server-rendered pages and their form submissions.
func (h *HTTPHandler) RequireBrowserPage(admin, csrfRequired bool, next http.Handler) http.Handler {
	return h.protectedWithRedirect(admin, csrfRequired, next, true)
}

func (h *HTTPHandler) protectedWithRedirect(admin, csrfRequired bool, next http.Handler, redirectToLogin bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			if redirectToLogin {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		session, err := h.service.Authenticate(r.Context(), cookie.Value)
		if errors.Is(err, ErrUnauthenticated) {
			h.clearCookie(w, r)
			if redirectToLogin {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if err != nil {
			slog.Error("access session lookup failed")
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if session.RotatedToken != "" {
			h.setCookie(w, r, session.RotatedToken, session.AbsoluteExpiry)
		}
		if admin && !session.User.PlatformAdmin {
			if redirectToLogin {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
		stateChanging := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
		if csrfRequired && stateChanging && !h.validRequestOrigin(r) {
			writeError(w, http.StatusForbidden, "request origin rejected")
			return
		}
		if csrfRequired && stateChanging {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			if !validCSRF(requestCSRF(r), session.CSRFToken) {
				writeError(w, http.StatusForbidden, "csrf token required")
				return
			}
		}
		ctx := context.WithValue(r.Context(), principalKey{}, session.User)
		ctx = context.WithValue(ctx, csrfContextKey{}, session.CSRFToken)
		ctx = context.WithValue(ctx, sessionTokenContextKey{}, cookie.Value)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type csrfContextKey struct{}
type sessionTokenContextKey struct{}

const sessionCookieName = "platform_session"

func (h *HTTPHandler) login(w http.ResponseWriter, r *http.Request) {
	if !h.validRequestOrigin(r) {
		if isHTMLRequest(r) {
			http.Redirect(w, r, "/login?notice=login-error", http.StatusSeeOther)
			return
		}
		writeError(w, http.StatusForbidden, "request origin rejected")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if isFormRequest(r) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if r.ParseForm() != nil {
			http.Redirect(w, r, "/login?notice=login-error", http.StatusSeeOther)
			return
		}
		input.Username, input.Password = r.PostForm.Get("username"), r.PostForm.Get("password")
	} else if !decodeJSON(w, r, &input) {
		return
	}
	if input.Username == "" || input.Password == "" {
		if isHTMLRequest(r) {
			http.Redirect(w, r, "/login?notice=login-error", http.StatusSeeOther)
			return
		}
		writeError(w, http.StatusBadRequest, "invalid login request")
		return
	}
	oldToken := ""
	if oldCookie, err := r.Cookie(sessionCookieName); err == nil {
		oldToken = oldCookie.Value
	}
	session, err := h.service.Login(r.Context(), input.Username, input.Password)
	if errors.Is(err, ErrUnauthenticated) {
		if isHTMLRequest(r) {
			http.Redirect(w, r, "/login?notice=login-error", http.StatusSeeOther)
			return
		}
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	if err != nil {
		slog.Error("access login failed")
		if isHTMLRequest(r) {
			http.Error(w, "The page could not be loaded.", http.StatusInternalServerError)
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if oldToken != "" {
		if err := h.service.Logout(r.Context(), oldToken); err != nil {
			slog.Error("revoke previous login session failed")
			if isHTMLRequest(r) {
				http.Error(w, "The page could not be loaded.", http.StatusInternalServerError)
				return
			}
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	h.setCookie(w, r, session.RotatedToken, session.AbsoluteExpiry)
	if isHTMLRequest(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": session.User})
}

func (h *HTTPHandler) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"user": currentUser(r.Context())})
}

func (h *HTTPHandler) csrf(w http.ResponseWriter, r *http.Request) {
	token, _ := r.Context().Value(csrfContextKey{}).(string)
	writeJSON(w, http.StatusOK, map[string]string{"csrf_token": token})
}

func (h *HTTPHandler) logout(w http.ResponseWriter, r *http.Request) {
	token, _ := r.Context().Value(sessionTokenContextKey{}).(string)
	if err := h.service.Logout(r.Context(), token); err != nil {
		slog.Error("access logout failed")
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.clearCookie(w, r)
	if isHTMLRequest(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func isFormRequest(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/x-www-form-urlencoded"
}

func isHTMLRequest(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

func requestCSRF(r *http.Request) string {
	if token := r.Header.Get("X-CSRF-Token"); token != "" {
		return token
	}
	if !isFormRequest(r) || r.ParseForm() != nil {
		return ""
	}
	return r.PostForm.Get("csrf_token")
}

// CurrentUser returns the authenticated user attached by the page middleware.
func CurrentUser(ctx context.Context) User { return currentUser(ctx) }

// CSRFToken returns the request token attached by the page middleware.
func CSRFToken(ctx context.Context) string {
	token, _ := ctx.Value(csrfContextKey{}).(string)
	return token
}

func (h *HTTPHandler) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.service.ListUsers(r.Context())
	if err != nil {
		internalError(w, "list users")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (h *HTTPHandler) createUser(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		PlatformAdmin bool   `json:"platform_admin"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	user, err := h.service.CreateUser(r.Context(), input.Username, input.Password, input.PlatformAdmin)
	if errors.Is(err, ErrConflict) {
		writeError(w, http.StatusConflict, "username already exists")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": user})
}

func (h *HTTPHandler) updateUser(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Active        *bool `json:"active"`
		PlatformAdmin *bool `json:"platform_admin"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Active == nil || input.PlatformAdmin == nil {
		writeError(w, http.StatusBadRequest, "active and platform_admin are required")
		return
	}
	actor := currentUser(r.Context())
	if strings.EqualFold(actor.ID, r.PathValue("userID")) && (!*input.Active || !*input.PlatformAdmin) {
		writeError(w, http.StatusConflict, "cannot disable or demote the current admin")
		return
	}
	if err := h.service.UpdateUser(r.Context(), r.PathValue("userID"), *input.Active, *input.PlatformAdmin); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) listGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := h.service.ListGroups(r.Context())
	if err != nil {
		internalError(w, "list groups")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

func (h *HTTPHandler) createGroup(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	group, err := h.service.CreateGroup(r.Context(), input.Name)
	if errors.Is(err, ErrConflict) {
		writeError(w, http.StatusConflict, "group name already exists")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"group": group})
}

func (h *HTTPHandler) deleteGroup(w http.ResponseWriter, r *http.Request) {
	if err := h.service.DeleteGroup(r.Context(), r.PathValue("groupID")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) listGroupMembers(w http.ResponseWriter, r *http.Request) {
	members, err := h.service.ListGroupMembers(r.Context(), r.PathValue("groupID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": members})
}

func (h *HTTPHandler) listDatabaseGrants(w http.ResponseWriter, r *http.Request) {
	grants, err := h.service.ListDatabaseGrants(r.Context(), r.PathValue("groupID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"databases": grants})
}

func (h *HTTPHandler) addMember(w http.ResponseWriter, r *http.Request) {
	if err := h.service.SetMembership(r.Context(), r.PathValue("groupID"), r.PathValue("userID"), true); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) removeMember(w http.ResponseWriter, r *http.Request) {
	if err := h.service.SetMembership(r.Context(), r.PathValue("groupID"), r.PathValue("userID"), false); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) setPermission(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Permission Permission `json:"permission"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := h.service.SetDatabasePermission(r.Context(), r.PathValue("groupID"), r.PathValue("databaseID"), input.Permission); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) deletePermission(w http.ResponseWriter, r *http.Request) {
	if err := h.service.DeleteDatabasePermission(r.Context(), r.PathValue("groupID"), r.PathValue("databaseID")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) databaseAccess(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r.Context())
	permission, err := h.service.EffectivePermission(r.Context(), user.ID, r.PathValue("databaseID"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if permission == "" {
		writeError(w, http.StatusForbidden, "no database access")
		return
	}
	writeJSON(w, http.StatusOK, map[string]Permission{"permission": permission})
}

func (h *HTTPHandler) setCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	maxAge := int(time.Until(expires).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: token, Path: "/", Expires: expires,
		MaxAge: maxAge, HttpOnly: true, Secure: h.secureCookie || r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *HTTPHandler) clearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: h.secureCookie || r.TLS != nil, SameSite: http.SameSiteLaxMode,
	})
}

func (h *HTTPHandler) validRequestOrigin(r *http.Request) bool {
	if !validRequestOrigin(r) {
		return false
	}
	if r.Header.Get("Origin") == "" || (!h.secureCookie && r.TLS == nil) {
		return true
	}
	u, err := url.Parse(r.Header.Get("Origin"))
	if err != nil {
		return false
	}
	return u.Scheme == "https"
}

func validCSRF(provided, expected string) bool {
	return provided != "" && len(provided) == len(expected) && subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func validRequestOrigin(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || !strings.EqualFold(u.Host, r.Host) {
		return false
	}
	return true
}

func currentUser(ctx context.Context) User {
	user, _ := UserFromContext(ctx)
	return user
}

// CurrentUser returns the authenticated principal attached by the access middleware.
func CurrentUser(ctx context.Context) User { return currentUser(ctx) }

// CSRFToken returns the session-bound token attached by the access middleware.
func CSRFToken(ctx context.Context) string {
	token, _ := ctx.Value(csrfContextKey{}).(string)
	return token
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON request")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "request must contain one JSON value")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func internalError(w http.ResponseWriter, operation string) {
	slog.Error("access operation failed", "operation", operation)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, "conflict")
	case errors.Is(err, ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid request")
	default:
		slog.Error("access operation failed")
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
