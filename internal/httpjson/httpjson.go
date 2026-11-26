// Package httpjson reads and writes the JSON bodies of the platform APIs.
package httpjson

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
)

// MaxBodyBytes limits every JSON request body.
const MaxBodyBytes = 1 << 20

// Write sends value as a JSON response that clients must not cache.
func Write(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// Error sends {"error": message}.
func Error(w http.ResponseWriter, status int, message string) {
	Write(w, status, map[string]string{"error": message})
}

// Decode reads exactly one JSON value without unknown fields into target. The
// request must be application/json and at most MaxBodyBytes. On failure it
// writes the error response and returns false.
func Decode(w http.ResponseWriter, r *http.Request, target any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		Error(w, http.StatusUnsupportedMediaType, "content type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		decodeError(w, err, "invalid JSON request")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		decodeError(w, err, "request must contain one JSON value")
		return false
	}
	return true
}

func decodeError(w http.ResponseWriter, err error, message string) {
	var sizeError *http.MaxBytesError
	if errors.As(err, &sizeError) {
		Error(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	Error(w, http.StatusBadRequest, message)
}
