package access

import (
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/wafhr/IF3110_K01-13/internal/httpjson"
)

const sessionCookieName = "platform_session"

func (h *HTTPHandler) login(w http.ResponseWriter, r *http.Request) {
	if !h.validRequestOrigin(r) {
		slog.WarnContext(r.Context(), "auth_login_rejected", "reason", "invalid_origin", "status", "failed")
		if isHTMLRequest(r) {
			http.Redirect(w, r, "/login?notice=login-error", http.StatusSeeOther)
			return
		}
		httpjson.Error(w, http.StatusForbidden, "request origin rejected")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if isFormRequest(r) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if r.ParseForm() != nil {
			slog.WarnContext(r.Context(), "auth_login_rejected", "reason", "invalid_request", "status", "failed")
			http.Redirect(w, r, "/login?notice=login-error", http.StatusSeeOther)
			return
		}
		input.Username, input.Password = r.PostForm.Get("username"), r.PostForm.Get("password")
	} else if !httpjson.Decode(w, r, &input) {
		slog.WarnContext(r.Context(), "auth_login_rejected", "reason", "invalid_request", "status", "failed")
		return
	}
	if input.Username == "" || input.Password == "" {
		slog.WarnContext(r.Context(), "auth_login_rejected", "reason", "invalid_request", "status", "failed")
		if isHTMLRequest(r) {
			http.Redirect(w, r, "/login?notice=login-error", http.StatusSeeOther)
			return
		}
		httpjson.Error(w, http.StatusBadRequest, "invalid login request")
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
		httpjson.Error(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "auth_login_failed", "status", "failed")
		if isHTMLRequest(r) {
			http.Error(w, "The page could not be loaded.", http.StatusInternalServerError)
			return
		}
		httpjson.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if oldToken != "" {
		if err := h.service.Logout(r.Context(), oldToken); err != nil {
			slog.ErrorContext(r.Context(), "previous_session_revoke_failed", "status", "failed")
			if isHTMLRequest(r) {
				http.Error(w, "The page could not be loaded.", http.StatusInternalServerError)
				return
			}
			httpjson.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	h.setCookie(w, r, session.RotatedToken, session.AbsoluteExpiry)
	if isHTMLRequest(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"user": session.User})
}

func (h *HTTPHandler) me(w http.ResponseWriter, r *http.Request) {
	httpjson.Write(w, http.StatusOK, map[string]any{"user": currentUser(r.Context())})
}

func (h *HTTPHandler) csrf(w http.ResponseWriter, r *http.Request) {
	token, _ := r.Context().Value(csrfContextKey{}).(string)
	httpjson.Write(w, http.StatusOK, map[string]string{"csrf_token": token})
}

func (h *HTTPHandler) logout(w http.ResponseWriter, r *http.Request) {
	token, _ := r.Context().Value(sessionTokenContextKey{}).(string)
	if err := h.service.Logout(r.Context(), token); err != nil {
		slog.ErrorContext(r.Context(), "auth_logout_failed", "status", "failed")
		httpjson.Error(w, http.StatusInternalServerError, "internal error")
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
