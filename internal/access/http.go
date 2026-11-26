package access

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/wafhr/IF3110_K01-13/internal/httpjson"
	"github.com/wafhr/IF3110_K01-13/internal/telemetry"
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
	h.mux.Handle("PUT /admin/groups/{groupID}/leader", h.protected(true, true, http.HandlerFunc(h.setGroupLeader)))
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
		if !minimum.valid() {
			writeServiceError(w, r.Context(), ErrInvalidInput)
			return
		}
		user := currentUser(r.Context())
		permission, err := h.service.EffectivePermission(r.Context(), user.ID, r.PathValue("databaseID"))
		if err != nil {
			writeServiceError(w, r.Context(), err)
			return
		}
		if !permission.satisfies(minimum) {
			slog.WarnContext(r.Context(), "access_denied", "reason", "database_permission_required", "required_permission", string(minimum), "status", "failed")
			writeServiceError(w, r.Context(), ErrForbidden)
			return
		}
		next.ServeHTTP(w, r)
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
		r = r.WithContext(telemetry.WithLogAttrs(r.Context(), slog.String("database_id", r.PathValue("databaseID"))))
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			slog.WarnContext(r.Context(), "access_denied", "reason", "session_required", "status", "failed")
			if redirectToLogin {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			httpjson.Error(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		session, err := h.service.Authenticate(r.Context(), cookie.Value)
		if errors.Is(err, ErrUnauthenticated) {
			slog.WarnContext(r.Context(), "access_denied", "reason", "invalid_session", "status", "failed")
			h.clearCookie(w, r)
			if redirectToLogin {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}
			httpjson.Error(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if err != nil {
			slog.ErrorContext(r.Context(), "session_lookup_failed", "status", "failed")
			httpjson.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		r = r.WithContext(telemetry.WithActor(r.Context(), session.User.ID))
		if session.RotatedToken != "" {
			h.setCookie(w, r, session.RotatedToken, session.AbsoluteExpiry)
		}
		if admin && !session.User.PlatformAdmin {
			slog.WarnContext(r.Context(), "access_denied", "reason", "platform_admin_required", "status", "failed")
			if redirectToLogin {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			httpjson.Error(w, http.StatusForbidden, "forbidden")
			return
		}
		stateChanging := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
		if csrfRequired && stateChanging && !h.validRequestOrigin(r) {
			slog.WarnContext(r.Context(), "access_denied", "reason", "invalid_origin", "status", "failed")
			httpjson.Error(w, http.StatusForbidden, "request origin rejected")
			return
		}
		if csrfRequired && stateChanging {
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			if !validCSRF(requestCSRF(r), session.CSRFToken) {
				slog.WarnContext(r.Context(), "access_denied", "reason", "invalid_csrf", "status", "failed")
				httpjson.Error(w, http.StatusForbidden, "csrf token required")
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

func internalError(w http.ResponseWriter, ctx context.Context, operation string) {
	slog.ErrorContext(ctx, "access_operation_failed", "operation", operation, "status", "failed")
	httpjson.Error(w, http.StatusInternalServerError, "internal error")
}

func writeServiceError(w http.ResponseWriter, ctx context.Context, err error) {
	var input *inputError
	switch {
	case errors.As(err, &input):
		httpjson.Error(w, http.StatusBadRequest, input.reason)
	case errors.Is(err, ErrNotFound):
		httpjson.Error(w, http.StatusNotFound, "not found")
	case errors.Is(err, ErrConflict):
		httpjson.Error(w, http.StatusConflict, "conflict")
	case errors.Is(err, ErrForbidden):
		httpjson.Error(w, http.StatusForbidden, "forbidden")
	case errors.Is(err, ErrInvalidInput):
		httpjson.Error(w, http.StatusBadRequest, "invalid request")
	default:
		slog.ErrorContext(ctx, "access_operation_failed", "status", "failed")
		httpjson.Error(w, http.StatusInternalServerError, "internal error")
	}
}
