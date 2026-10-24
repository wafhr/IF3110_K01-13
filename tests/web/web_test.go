package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/access"
	"github.com/wafhr/IF3110_K01-13/internal/app"
	"github.com/wafhr/IF3110_K01-13/web"
)

func TestLoginPageAndStylesheet(t *testing.T) {
	auth := access.NewHTTPHandler(nil, false)
	handler := web.NewHandler(nil, auth)
	for _, tc := range []struct {
		path, contentType, contains string
	}{
		{"/login", "text/html; charset=utf-8", "Welcome back"},
		{"/assets/css/app.css", "text/css; charset=utf-8", "--green:"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if response.Code != http.StatusOK || response.Header().Get("Content-Type") != tc.contentType {
				t.Fatalf("response = %d %q", response.Code, response.Header().Get("Content-Type"))
			}
			if !strings.Contains(response.Body.String(), tc.contains) {
				t.Fatalf("body does not contain %q", tc.contains)
			}
		})
	}
}

func TestApplicationRoutesSeparateFrontendFromAccessAPI(t *testing.T) {
	accessHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Route", "access")
		w.WriteHeader(http.StatusNoContent)
	})
	frontendHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Route", "frontend")
		w.WriteHeader(http.StatusNoContent)
	})
	handler := app.RoutesWithFrontend(accessHandler, frontendHandler)
	for _, tc := range []struct{ path, route string }{
		{"/auth/me", "access"}, {"/", "frontend"}, {"/login", "frontend"}, {"/assets/css/app.css", "frontend"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if response.Code != http.StatusNoContent || response.Header().Get("X-Route") != tc.route {
				t.Fatalf("route = %q status=%d, want %q", response.Header().Get("X-Route"), response.Code, tc.route)
			}
		})
	}
}
