package repository

import (
	"context"
	"errors"
	"time"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/model"
)

// A commit error does not prove rollback. Resolve from a new authoritative
// DataState transaction before writing/deleting files or returning success.
var ErrCommitUncertain = errors.New("image data transaction outcome is uncertain")

// Transfers is separate from the metadata catalog. It performs SQL only.
// Callers must authorize and supply a fresh server-generated attempt UUID.
// No method confirms filesystem I/O, performs cleanup, or runs reconciliation.
type Transfers interface {
	ReserveData(context.Context, string, string, *int64, time.Time) (model.DataOperation, error)
	StageData(context.Context, string, string, model.DataContent, time.Time) error
	PublishData(context.Context, string, string, time.Time) error
	CommitData(context.Context, string, string, time.Time) error
	BeginDataCleanup(context.Context, string, string, string, *string, time.Time) error
	CompleteDataCleanup(context.Context, string, string, time.Time) error
	DataState(context.Context, string) (model.DataState, error)
	// DataStateSnapshot: same data, non-locking; only for read-only GET paths.
	DataStateSnapshot(context.Context, string) (model.DataState, error)
	ListDataOperations(context.Context, string, int) ([]model.DataOperation, error)
}
