package store

import (
	"context"
	"errors"
	"time"

	"hospital/internal/model"
)

// ErrNotFound is returned when an id does not exist.
var ErrNotFound = errors.New("not found")

// Store persists runbooks, incidents, and operations.
type Store interface {
	Ping(ctx context.Context) error
	Close() error

	UpsertRunbook(ctx context.Context, rb model.Runbook) (model.Runbook, error)
	DeleteRunbook(ctx context.Context, id int64) error
	ListRunbooks(ctx context.Context) ([]model.Runbook, error)
	MatchRunbook(ctx context.Context, alertName string, labels map[string]string) (model.Runbook, bool, error)

	// CreateRemediation inserts an incident and, unless the fingerprint is
	// inside its cooldown, an operation. skipped is true when no operation
	// was created.
	CreateRemediation(ctx context.Context, inc model.Incident, op model.Operation, cooldown time.Duration) (model.Incident, model.Operation, bool, error)
	ListIncidents(ctx context.Context, limit int) ([]model.Incident, error)
	GetIncident(ctx context.Context, id int64) (model.Incident, error)

	ClaimK8sOperation(ctx context.Context) (model.Operation, bool, error)
	ClaimSurgeonOperation(ctx context.Context, surgeonID string) (model.Operation, bool, error)
	FinishOperation(ctx context.Context, operationID, incidentID int64, status, logs string) error
	RequeueStale(ctx context.Context, updatedBefore time.Time) (int, error)
	GetOperation(ctx context.Context, id int64) (model.Operation, error)
	ListOperations(ctx context.Context, limit int) ([]model.Operation, error)
}

var (
	_ Store = (*Memory)(nil)
	_ Store = (*Postgres)(nil)
)

func clampLimit(n int) int {
	if n <= 0 || n > 500 {
		return 100
	}
	return n
}
