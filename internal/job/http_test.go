package job

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/access"
)

type guardFunc func(access.Permission, http.Handler) http.Handler

func (g guardFunc) RequireDatabaseAccess(minimum access.Permission, next http.Handler) http.Handler {
	return g(minimum, next)
}

func allowGuard(t *testing.T) DatabaseGuard {
	return guardFunc(func(minimum access.Permission, next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			want := access.ViewOnly
			if r.Method == http.MethodPost {
				want = access.ViewEdit
			}
			if minimum != want {
				t.Errorf("%s %s requires %q, want %q", r.Method, r.URL.Path, minimum, want)
			}
			next.ServeHTTP(w, r)
		})
	})
}

func denyGuard() DatabaseGuard {
	return guardFunc(func(access.Permission, http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusForbidden, "forbidden")
		})
	})
}

func serve(t *testing.T, store Store, guard DatabaseGuard, path string) *httptest.ResponseRecorder {
	return serveMethod(t, store, guard, http.MethodGet, path)
}

func serveMethod(t *testing.T, store Store, guard DatabaseGuard, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	NewHTTPHandler(newTestService(t, store), guard).ServeHTTP(response, httptest.NewRequest(method, path, nil))
	return response
}

func TestGetJob(t *testing.T) {
	path := "/databases/" + testDatabaseID + "/jobs/" + testJobID
	response := serve(t, newFakeStore(queuedJob()), allowGuard(t), path)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status = %d, cache = %q", response.Code, response.Header().Get("Cache-Control"))
	}
	var body struct {
		Job Job `json:"job"`
	}
	if err := json.NewDecoder(strings.NewReader(response.Body.String())).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Job.ID != testJobID || body.Job.Status != Pending || body.Job.ExecutionID != testExecutionID || body.Job.Name != "add index" {
		t.Fatalf("job body = %+v", body.Job)
	}
	if strings.Contains(response.Body.String(), "CREATE INDEX") || strings.Contains(response.Body.String(), "traceparent") {
		t.Fatal("job response exposed the queued payload")
	}
}

func TestGetJobErrors(t *testing.T) {
	failing := newFakeStore()
	failing.err = errors.New("connection refused by db.internal")
	tests := []struct {
		name       string
		store      Store
		guard      DatabaseGuard
		path       string
		wantStatus int
	}{
		{name: "job of another database", store: newFakeStore(queuedJob()), path: "/databases/" + otherDatabaseID + "/jobs/" + testJobID, wantStatus: http.StatusNotFound},
		{name: "unknown job", store: newFakeStore(), path: "/databases/" + testDatabaseID + "/jobs/" + testJobID, wantStatus: http.StatusNotFound},
		{name: "storage failure stays internal", store: failing, path: "/databases/" + testDatabaseID + "/jobs/" + testJobID, wantStatus: http.StatusInternalServerError},
		{name: "guard denial", store: newFakeStore(queuedJob()), guard: denyGuard(), path: "/databases/" + testDatabaseID + "/jobs/" + testJobID, wantStatus: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			guard := test.guard
			if guard == nil {
				guard = allowGuard(t)
			}
			response := serve(t, test.store, guard, test.path)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if strings.Contains(response.Body.String(), "db.internal") {
				t.Fatal("storage error details were exposed")
			}
		})
	}
}

func TestListJobs(t *testing.T) {
	tests := []struct {
		query      string
		wantStatus int
		wantLimit  int
	}{
		{query: "", wantStatus: http.StatusOK, wantLimit: defaultListLimit},
		{query: "?limit=5", wantStatus: http.StatusOK, wantLimit: 5},
		{query: "?limit=100", wantStatus: http.StatusOK, wantLimit: 100},
		{query: "?limit=0", wantStatus: http.StatusBadRequest},
		{query: "?limit=101", wantStatus: http.StatusBadRequest},
		{query: "?limit=all", wantStatus: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.query, func(t *testing.T) {
			store := newFakeStore(queuedJob())
			response := serve(t, store, allowGuard(t), "/databases/"+testDatabaseID+"/jobs"+test.query)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if test.wantStatus != http.StatusOK {
				return
			}
			var body struct {
				Jobs []Job `json:"jobs"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil || len(body.Jobs) != 1 || store.listedLimit != test.wantLimit {
				t.Fatalf("jobs = %+v, %v; limit %d", body.Jobs, err, store.listedLimit)
			}
		})
	}
}

func TestResumeJob(t *testing.T) {
	path := "/databases/" + testDatabaseID + "/jobs/" + testJobID + "/resume"
	tests := []struct {
		name       string
		job        Job
		wantStatus int
	}{
		{name: "partial job is queued again", job: partialJob(), wantStatus: http.StatusAccepted},
		{name: "running job cannot be resumed", job: queuedJob(), wantStatus: http.StatusConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := serveMethod(t, newFakeStore(test.job), allowGuard(t), http.MethodPost, path)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", response.Code, test.wantStatus, response.Body.String())
			}
			if test.wantStatus == http.StatusAccepted && !strings.Contains(response.Body.String(), `"status":"pending"`) {
				t.Fatalf("resume response = %s", response.Body.String())
			}
		})
	}
	if response := serveMethod(t, newFakeStore(partialJob()), denyGuard(), http.MethodPost, path); response.Code != http.StatusForbidden {
		t.Fatalf("resume without edit access = %d", response.Code)
	}
}
