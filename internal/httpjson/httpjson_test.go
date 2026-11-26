package httpjson

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecode(t *testing.T) {
	tests := []struct {
		name, body, contentType string
		want                    int
	}{
		{"valid", `{"name":"x"}`, "application/json; charset=utf-8", http.StatusOK},
		{"wrong content type", `{"name":"x"}`, "text/plain", http.StatusUnsupportedMediaType},
		{"unknown field", `{"other":1}`, "application/json", http.StatusBadRequest},
		{"two values", `{"name":"x"}{}`, "application/json", http.StatusBadRequest},
		{"too large", `{"name":"` + strings.Repeat("x", MaxBodyBytes) + `"}`, "application/json", http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(test.body))
			request.Header.Set("Content-Type", test.contentType)
			response := httptest.NewRecorder()
			var target struct {
				Name string `json:"name"`
			}
			if Decode(response, request, &target) {
				response.WriteHeader(http.StatusOK)
			}
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestWriteDisablesCaching(t *testing.T) {
	response := httptest.NewRecorder()
	Error(response, http.StatusNotFound, "not found")
	if response.Code != http.StatusNotFound || response.Header().Get("Cache-Control") != "no-store" ||
		strings.TrimSpace(response.Body.String()) != `{"error":"not found"}` {
		t.Fatalf("response = %d %q %q", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
	}
}
