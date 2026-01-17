package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/dedezza1D/taskflow/internal/queue"
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

	priority := store.PriorityNormal
	subject := queue.SubjectNormal

	if req.Priority != "" {
		switch req.Priority {
		case "low":
			priority = store.PriorityLow
			subject = queue.SubjectLow
		case "normal":
			priority = store.PriorityNormal
			subject = queue.SubjectNormal
		case "high":
			priority = store.PriorityHigh
			subject = queue.SubjectHigh
		default:
			writeErr(w, http.StatusBadRequest, "validation_error", "priority must be low|normal|high")
			return
		}
	}

	task, err := s.store.CreateTask(r.Context(), store.CreateTaskParams{
		Type:     req.Type,
		Payload:  []byte(req.Payload),
		Priority: priority,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Best-effort enqueue (MVP). Task stays in DB even if enqueue fails.
	if s.queue != nil {
		err := s.queue.PublishTask(r.Context(), subject, queue.TaskMessage{
			TaskID:   task.ID.String(),
			Priority: string(task.Priority),
		})
		if err != nil {
			s.logger.Warn("failed to enqueue task", zap.Error(err), zap.String("task_id", task.ID.String()))
		}
	}

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

	task, err := s.store.GetTask(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "task not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
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

	items, err := s.store.ListTasks(r.Context(), store.ListTasksParams{
		Status: status,
		Type:   taskType,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
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

	items, err := s.store.ListExecutions(r.Context(), taskID, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, listExecutionsResponse{
		Items: items,
		Limit: limit,
	})
}
