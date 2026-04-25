package transport

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"otter/internal/config"
)

type Runner func(task string) string

type Server struct {
	httpServer *http.Server
	token      string
	run        Runner
}

type runRequest struct {
	Task string `json:"task"`
}

type runResponse struct {
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

func NewServer(cfg config.Config, run Runner) *Server {
	srv := &Server{
		token: cfg.Token,
		run:   run,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/run", srv.handleRun)
	mux.HandleFunc("/healthz", srv.handleHealth)

	srv.httpServer = &http.Server{
		Addr:    cfg.Address(),
		Handler: mux,
	}
	return srv
}

func (s *Server) ListenAndServe() error {
	return s.httpServer.ListenAndServe()
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, runResponse{Result: "ok"})
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, runResponse{Error: "method not allowed"})
		return
	}

	if err := validateAuthHeader(r.Header.Get("Authorization"), s.token); err != nil {
		writeJSON(w, http.StatusUnauthorized, runResponse{Error: err.Error()})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, runResponse{Error: "failed to read request body"})
		return
	}

	var req runRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, runResponse{Error: "invalid JSON body"})
		return
	}

	task := strings.TrimSpace(req.Task)
	if task == "" {
		writeJSON(w, http.StatusBadRequest, runResponse{Error: "task is required"})
		return
	}

	result := s.run(task)
	writeJSON(w, http.StatusOK, runResponse{Result: result})
}

func validateAuthHeader(headerValue, token string) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("server token is not configured")
	}

	const prefix = "Bearer "
	if !strings.HasPrefix(headerValue, prefix) {
		return errors.New("missing bearer token")
	}

	actual := strings.TrimSpace(strings.TrimPrefix(headerValue, prefix))
	if actual == "" {
		return errors.New("missing bearer token")
	}
	if actual != token {
		return errors.New("invalid bearer token")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload runResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusInternalServerError)
	}
}
