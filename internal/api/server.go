package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"hospital/internal/alert"
	"hospital/internal/model"
	"hospital/internal/remedy"
	"hospital/internal/store"
)

// Server is the Hospital HTTP API.
type Server struct {
	store        store.Store
	notify       remedy.Notifier
	log          *slog.Logger
	token        string
	pollTimeout  time.Duration
	pollInterval time.Duration
}

func New(st store.Store, notify remedy.Notifier, token string, pollTimeout, pollInterval time.Duration, log *slog.Logger) *Server {
	if notify == nil {
		notify = remedy.NopNotifier{}
	}
	if log == nil {
		log = slog.Default()
	}
	if pollInterval <= 0 {
		pollInterval = time.Second
	}
	if pollTimeout <= 0 {
		pollTimeout = 25 * time.Second
	}
	return &Server{
		store:        st,
		notify:       notify,
		log:          log,
		token:        token,
		pollTimeout:  pollTimeout,
		pollInterval: pollInterval,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("POST /v1/reception", s.reception)
	mux.HandleFunc("GET /v1/operation", s.pollOperation)
	mux.HandleFunc("POST /v1/report", s.report)
	mux.HandleFunc("GET /v1/runbooks", s.listRunbooks)
	mux.HandleFunc("POST /v1/runbooks", s.upsertRunbook)
	mux.HandleFunc("DELETE /v1/runbooks/{id}", s.deleteRunbook)
	mux.HandleFunc("GET /v1/incidents", s.listIncidents)
	mux.HandleFunc("GET /v1/operations", s.listOperations)
	mux.HandleFunc("GET /v1/operations/{id}", s.getOperation)
	return s.authenticate(mux)
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	if s.token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/readyz":
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+s.token {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) reception(w http.ResponseWriter, r *http.Request) {
	msg, err := alert.Parse(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := remedy.Receive(r.Context(), s.store, msg, s.log)
	if err != nil {
		s.log.Error("receive alerts", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to record alerts")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) pollOperation(w http.ResponseWriter, r *http.Request) {
	surgeonID := r.URL.Query().Get("surgeon_id")
	if surgeonID == "" {
		writeError(w, http.StatusBadRequest, "surgeon_id is required")
		return
	}
	deadline := time.NewTimer(s.pollTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(s.pollInterval)
	defer tick.Stop()

	for {
		op, ok, err := s.store.ClaimSurgeonOperation(r.Context(), surgeonID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to claim operation")
			return
		}
		if ok {
			writeJSON(w, http.StatusOK, op)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-tick.C:
		}
	}
}

type reportRequest struct {
	OperationID int64  `json:"operation_id"`
	Status      string `json:"status"`
	Logs        string `json:"logs"`
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	var body reportRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid report")
		return
	}
	if body.Status != model.StatusSucceeded && body.Status != model.StatusFailed {
		writeError(w, http.StatusBadRequest, "status must be succeeded or failed")
		return
	}
	op, err := s.store.GetOperation(r.Context(), body.OperationID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "operation not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load operation")
		return
	}
	if op.Status != model.StatusRunning && op.Status != model.StatusPending {
		writeError(w, http.StatusConflict, "operation is already finished")
		return
	}
	logs := body.Logs
	if len(logs) > 32<<10 {
		logs = logs[:32<<10] + "\n...truncated"
	}
	if err := s.store.FinishOperation(r.Context(), op.ID, op.IncidentID, body.Status, logs); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to store report")
		return
	}
	inc, err := s.store.GetIncident(r.Context(), op.IncidentID)
	if err != nil {
		s.log.Error("load incident", "incident_id", op.IncidentID, "error", err)
	} else {
		op.Status = body.Status
		op.Logs = logs
		if err := s.notify.Notify(r.Context(), inc, op); err != nil {
			s.log.Error("notify", "operation_id", op.ID, "error", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": body.Status})
}

func (s *Server) listRunbooks(w http.ResponseWriter, r *http.Request) {
	books, err := s.store.ListRunbooks(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list runbooks")
		return
	}
	if books == nil {
		books = []model.Runbook{}
	}
	writeJSON(w, http.StatusOK, books)
}

func (s *Server) upsertRunbook(w http.ResponseWriter, r *http.Request) {
	var rb model.Runbook
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&rb); err != nil {
		writeError(w, http.StatusBadRequest, "invalid runbook")
		return
	}
	saved, err := s.store.UpsertRunbook(r.Context(), rb)
	if err != nil {
		if errors.Is(err, store.ErrDuplicateRunbook) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (s *Server) deleteRunbook(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid runbook id")
		return
	}
	if err := s.store.DeleteRunbook(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "runbook not found")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete runbook")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listIncidents(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListIncidents(r.Context(), queryLimit(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list incidents")
		return
	}
	if items == nil {
		items = []model.Incident{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) listOperations(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.ListOperations(r.Context(), queryLimit(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list operations")
		return
	}
	if items == nil {
		items = []model.Operation{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) getOperation(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid operation id")
		return
	}
	op, err := s.store.GetOperation(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "operation not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load operation")
		return
	}
	writeJSON(w, http.StatusOK, op)
}

func queryLimit(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	return n
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
