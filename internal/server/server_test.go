package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRoutes(t *testing.T) {
	srv := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := srv.Routes()

	tests := []struct {
		name       string
		method     string
		target     string
		wantStatus int
		wantBody   string
	}{
		{"health", http.MethodGet, "/healthz", http.StatusOK, `{"status":"ok"}` + "\n"},
		{"ping", http.MethodGet, "/api/v1/ping", http.StatusOK, `{"message":"pong"}` + "\n"},
		{"unknown route", http.MethodGet, "/nope", http.StatusNotFound, ""},
		{"wrong method", http.MethodPost, "/healthz", http.StatusMethodNotAllowed, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.target, nil)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantBody != "" && rec.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}
