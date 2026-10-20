package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRoutes(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/health", http.StatusOK},
		{"POST", "/health", http.StatusMethodNotAllowed},
		{"GET", "/missing", http.StatusNotFound},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			Routes().ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d", response.Code, tc.status)
			}
			if tc.status == http.StatusOK {
				if response.Body.String() != "{\"status\":\"ok\"}\n" {
					t.Fatalf("unexpected health response: %s", response.Body.String())
				}
				if response.Header().Get("Content-Type") != "application/json" {
					t.Fatal("expected JSON content type")
				}
			}
		})
	}
}
