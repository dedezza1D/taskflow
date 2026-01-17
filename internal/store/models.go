package store

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type TaskStatus string
type TaskPriority string

const (
	StatusQueued     TaskStatus = "queued"
	StatusProcessing TaskStatus = "processing"
	StatusCompleted  TaskStatus = "completed"
	StatusFailed     TaskStatus = "failed"
	StatusCancelled  TaskStatus = "cancelled"
)

const (
	PriorityLow    TaskPriority = "low"
	PriorityNormal TaskPriority = "normal"
	PriorityHigh   TaskPriority = "high"
)

type Task struct {
	ID        uuid.UUID       `json:"id"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	Priority  TaskPriority    `json:"priority"`
	Status    TaskStatus      `json:"status"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Version   int             `json:"version"`
}

type ExecutionStatus string

const (
	ExecStarted   ExecutionStatus = "started"
	ExecSucceeded ExecutionStatus = "succeeded"
	ExecFailed    ExecutionStatus = "failed"
)

type TaskExecution struct {
	ID         uuid.UUID       `json:"id"`
	TaskID     uuid.UUID       `json:"task_id"`
	Attempt    int             `json:"attempt"`
	Status     ExecutionStatus `json:"status"`
	Error      *string         `json:"error,omitempty"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
}
