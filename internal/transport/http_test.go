package transport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleRunRequiresBearerToken(t *testing.T) {
	server := &Server{
		token: "secret",
		run: func(task string) string {
			return task
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(`{"task":"hello"}`))
	rec := httptest.NewRecorder()
	server.handleRun(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, rec.Code)
	}
}

func TestHandleRunSuccess(t *testing.T) {
	server := &Server{
		token: "secret",
		run: func(task string) string {
			return "stub:" + task
		},
	}

	req := httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(`{"task":"organize my downloads"}`))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	server.handleRun(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"result":"stub:organize my downloads"`) {
		t.Fatalf("unexpected response body: %s", rec.Body.String())
	}
}
