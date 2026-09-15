package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/dedezza1D/taskflow/internal/pipeline"
	"github.com/dedezza1D/taskflow/internal/store"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"go.uber.org/zap"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

type apiError struct {
	Error   string `json:"error"`
	Details string `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string, details string) {
	writeJSON(w, status, apiError{Error: msg, Details: details})
}

type createTaskRequest struct {
	Type     string          `json:"type"`
	Payload  json.RawMessage `json:"payload"`
	Priority string          `json:"priority,omitempty"` // low|normal|high
}

type createTaskResponse struct {
	Task store.Task `json:"task"`
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Type == "" {
		writeErr(w, http.StatusBadRequest, "validation_error", "type is required")
		return
	}
	if len(req.Payload) == 0 {
		writeErr(w, http.StatusBadRequest, "validation_error", "payload is required")
		return
	}
	// Pipeline tasks are minted only by POST /documents, which checks that the
	// document is the caller's. Accepting one here would let a payload name any
	// document id — another tenant's included — and have the worker process it.
	if req.Type == pipeline.TaskType {
		writeErr(w, http.StatusBadRequest, "validation_error",
			"type "+pipeline.TaskType+" is reserved; upload through /api/v1/documents")
		return
	}

	priority := store.PriorityNormal

	if req.Priority != "" {
		switch req.Priority {
		case "low":
			priority = store.PriorityLow
		case "normal":
			priority = store.PriorityNormal
		case "high":
			priority = store.PriorityHigh
		default:
			writeErr(w, http.StatusBadRequest, "validation_error", "priority must be low|normal|high")
			return
		}
	}

	task, err := s.store.CreateTask(r.Context(), store.CreateTaskParams{
		Type:     req.Type,
		Payload:  []byte(req.Payload),
		Priority: priority,
		OrgID:    principal(r).OrgID,
	})
	if err != nil {
		// The raw error can carry SQL and schema detail; that belongs in the
		// log, not in a response body.
		s.logger.Error("create task failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to create task")
		return
	}

	s.publishTaskMessage(r, task)

	writeJSON(w, http.StatusCreated, createTaskResponse{Task: *task})
}

type getTaskResponse struct {
	Task store.Task `json:"task"`
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	idStr := mux.Vars(r)["id"]
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "invalid task id")
		return
	}

	task, err := s.store.GetTaskForOrg(r.Context(), id, principal(r).OrgID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "task not found")
			return
		}
		// The raw error can carry SQL and schema detail; that belongs in the
		// log, not in a response body.
		s.logger.Error("get task failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to load task")
		return
	}

	writeJSON(w, http.StatusOK, getTaskResponse{Task: *task})
}

type listTasksResponse struct {
	Items  []store.Task `json:"items"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()

	var status *store.TaskStatus
	if v := qp.Get("status"); v != "" {
		sv := store.TaskStatus(v)
		switch sv {
		case store.StatusQueued, store.StatusProcessing, store.StatusCompleted, store.StatusFailed, store.StatusCancelled:
			status = &sv
		default:
			writeErr(w, http.StatusBadRequest, "validation_error", "invalid status")
			return
		}
	}

	var taskType *string
	if v := qp.Get("type"); v != "" {
		taskType = &v
	}

	limit := 50
	if v := qp.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeErr(w, http.StatusBadRequest, "validation_error", "limit must be 1..200")
			return
		}
		limit = n
	}

	offset := 0
	if v := qp.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "validation_error", "offset must be >= 0")
			return
		}
		offset = n
	}

	items, err := s.store.ListTasksForOrg(r.Context(), principal(r).OrgID, store.ListTasksParams{
		Status: status,
		Type:   taskType,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		// The raw error can carry SQL and schema detail; that belongs in the
		// log, not in a response body.
		s.logger.Error("list tasks failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to list tasks")
		return
	}

	writeJSON(w, http.StatusOK, listTasksResponse{
		Items:  items,
		Limit:  limit,
		Offset: offset,
	})
}

type listExecutionsResponse struct {
	Items []store.TaskExecution `json:"items"`
	Limit int                   `json:"limit"`
}

func (s *Server) handleListExecutions(w http.ResponseWriter, r *http.Request) {
	idStr := mux.Vars(r)["id"]
	taskID, err := uuid.Parse(idStr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "validation_error", "invalid task id")
		return
	}

	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeErr(w, http.StatusBadRequest, "validation_error", "limit must be 1..200")
			return
		}
		limit = n
	}

	// Executions carry no tenant of their own; they inherit the task's. Checking
	// ownership first also makes another tenant's task a 404 here, not an empty
	// list that would still confirm the id exists.
	if _, err := s.store.GetTaskForOrg(r.Context(), taskID, principal(r).OrgID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "task not found")
			return
		}
		s.logger.Error("get task failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to load task")
		return
	}

	items, err := s.store.ListExecutions(r.Context(), taskID, limit)
	if err != nil {
		// The raw error can carry SQL and schema detail; that belongs in the
		// log, not in a response body.
		s.logger.Error("list executions failed", zap.Error(err))
		writeErr(w, http.StatusInternalServerError, "internal_error", "failed to list executions")
		return
	}

	writeJSON(w, http.StatusOK, listExecutionsResponse{
		Items: items,
		Limit: limit,
	})
}
