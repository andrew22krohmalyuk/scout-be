// Package server wires the HTTP routes and handlers for scout-be.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// Server holds the dependencies shared by every handler.
type Server struct {
	logger *slog.Logger
	// Add dependencies here as the service grows (db, config, clients...).
}

// New returns a Server ready to serve traffic.
func New(logger *slog.Logger) *Server {
	return &Server{logger: logger}
}

// Routes registers every route and returns the fully wrapped handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/v1/ping", s.handlePing)

	return s.withRequestLogging(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, r, http.StatusOK, map[string]string{"message": "pong"})
}

// writeJSON encodes v as the response body. The status is written first, so a
// late encoding failure can only be logged, not turned into an error response.
func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.logger.ErrorContext(r.Context(), "encode response", "error", err, "path", r.URL.Path)
	}
}

// withRequestLogging logs one line per request with its status and duration.
func (s *Server) withRequestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rec, r)

		s.logger.InfoContext(r.Context(), "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).String(),
		)
	})
}

// statusRecorder captures the status code on its way to the client.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
