package model

import "time"

// Task is a row of the original's tasks table with its task_info
// (input/result/message JSON text). Imports are type "api_image_import".
type Task struct {
	ID, Type, Status, Owner string
	ImageID, RequestID      *string
	UserID                  *string
	Input, Result           *string // JSON text
	Message                 string
	ExpiresAt               *time.Time
	CreatedAt, UpdatedAt    time.Time
}

// TaskListOptions are the original's GET /v2/tasks parameters.
type TaskListOptions struct {
	Limit            int
	Marker           string
	SortKey, SortDir string
	Type, Status     string
	Owner            string // "" for an admin: every project
}

// The import lock and staging properties the original keeps on the image.
const (
	ImportTaskProperty = "os_glance_import_task"
	StageHostProperty  = "os_glance_stage_host"
)

// Location is one active image_locations row.
type Location struct{ URL, Metadata string }
