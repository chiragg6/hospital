package remedy

import (
	"context"
	"log/slog"
	"time"

	"hospital/internal/model"
	"hospital/internal/store"
)

// Notifier reports a finished remediation.
type Notifier interface {
	Notify(ctx context.Context, inc model.Incident, op model.Operation) error
}

// NopNotifier discards notifications.
type NopNotifier struct{}

func (NopNotifier) Notify(context.Context, model.Incident, model.Operation) error { return nil }

// Worker drains Kubernetes API operations from the store.
type Worker struct {
	store      store.Store
	exec       Executor
	notify     Notifier
	interval   time.Duration
	staleAfter time.Duration
	log        *slog.Logger
}

func NewWorker(st store.Store, exec Executor, notify Notifier, interval, staleAfter time.Duration, log *slog.Logger) *Worker {
	if notify == nil {
		notify = NopNotifier{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Worker{
		store:      st,
		exec:       exec,
		notify:     notify,
		interval:   interval,
		staleAfter: staleAfter,
		log:        log,
	}
}

func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	w.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.Tick(ctx)
		}
	}
}

// Tick requeues abandoned work and executes every currently pending API operation.
func (w *Worker) Tick(ctx context.Context) {
	if n, err := w.store.RequeueStale(ctx, time.Now().Add(-w.staleAfter)); err != nil {
		w.log.Error("requeue stale operations", "error", err)
	} else if n > 0 {
		w.log.Warn("requeued stale operations", "count", n)
	}
	for {
		if ctx.Err() != nil {
			return
		}
		op, ok, err := w.store.ClaimK8sOperation(ctx)
		if err != nil {
			w.log.Error("claim operation", "error", err)
			return
		}
		if !ok {
			return
		}
		logs, execErr := w.exec.Execute(ctx, op)
		status := model.StatusSucceeded
		if execErr != nil {
			status = model.StatusFailed
			if logs != "" {
				logs = logs + "\n" + execErr.Error()
			} else {
				logs = execErr.Error()
			}
			w.log.Error("remediation failed", "operation_id", op.ID, "action", op.Action, "error", execErr)
		} else {
			w.log.Info("remediation finished", "operation_id", op.ID, "action", op.Action, "logs", logs)
		}
		if err := w.store.FinishOperation(ctx, op.ID, op.IncidentID, status, truncate(logs, 32<<10)); err != nil {
			w.log.Error("finish operation", "operation_id", op.ID, "error", err)
			continue
		}
		inc, err := w.store.GetIncident(ctx, op.IncidentID)
		if err != nil {
			w.log.Error("load incident", "incident_id", op.IncidentID, "error", err)
			inc = model.Incident{ID: op.IncidentID, Status: status}
		}
		op.Status = status
		op.Logs = logs
		if err := w.notify.Notify(ctx, inc, op); err != nil {
			w.log.Error("notify", "operation_id", op.ID, "error", err)
		}
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n...truncated"
}
