package store

import (
	"context"
	"sort"
	"sync"
	"time"

	"hospital/internal/model"
)

// Memory is a process-local store. It is the default when DATABASE_URL is unset.
type Memory struct {
	mu         sync.Mutex
	nextRB     int64
	nextInc    int64
	nextOp     int64
	runbooks   map[int64]model.Runbook
	incidents  map[int64]model.Incident
	operations map[int64]model.Operation
}

func NewMemory() *Memory {
	return &Memory{
		runbooks:   map[int64]model.Runbook{},
		incidents:  map[int64]model.Incident{},
		operations: map[int64]model.Operation{},
	}
}

func (m *Memory) Ping(context.Context) error { return nil }
func (m *Memory) Close() error               { return nil }

func (m *Memory) UpsertRunbook(_ context.Context, rb model.Runbook) (model.Runbook, error) {
	if err := rb.Normalize(); err != nil {
		return model.Runbook{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if rb.ID != 0 {
		if _, ok := m.runbooks[rb.ID]; !ok {
			return model.Runbook{}, ErrNotFound
		}
		for id, existing := range m.runbooks {
			if id != rb.ID && existing.Signature() == rb.Signature() {
				return model.Runbook{}, ErrDuplicateRunbook
			}
		}
		m.runbooks[rb.ID] = model.CloneRunbook(rb)
		return model.CloneRunbook(rb), nil
	}
	for id, existing := range m.runbooks {
		if existing.Signature() == rb.Signature() {
			rb.ID = id
			m.runbooks[id] = model.CloneRunbook(rb)
			return model.CloneRunbook(rb), nil
		}
	}
	m.nextRB++
	rb.ID = m.nextRB
	m.runbooks[rb.ID] = model.CloneRunbook(rb)
	return model.CloneRunbook(rb), nil
}

func (m *Memory) DeleteRunbook(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runbooks[id]; !ok {
		return ErrNotFound
	}
	delete(m.runbooks, id)
	return nil
}

func (m *Memory) ListRunbooks(context.Context) ([]model.Runbook, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.Runbook, 0, len(m.runbooks))
	for _, rb := range m.runbooks {
		out = append(out, model.CloneRunbook(rb))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AlertName == out[j].AlertName {
			return out[i].ID < out[j].ID
		}
		return out[i].AlertName < out[j].AlertName
	})
	return out, nil
}

func (m *Memory) MatchRunbook(_ context.Context, alertName string, labels map[string]string) (model.Runbook, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var (
		best  model.Runbook
		found bool
	)
	for _, rb := range m.runbooks {
		if !rb.Matches(alertName, labels) {
			continue
		}
		if !found || betterRunbook(rb, best) {
			best = rb
			found = true
		}
	}
	if !found {
		return model.Runbook{}, false, nil
	}
	return model.CloneRunbook(best), true, nil
}

func betterRunbook(candidate, current model.Runbook) bool {
	if candidate.Specificity() != current.Specificity() {
		return candidate.Specificity() > current.Specificity()
	}
	if candidate.ID != current.ID {
		return candidate.ID < current.ID
	}
	return candidate.Signature() < current.Signature()
}

func (m *Memory) CreateRemediation(_ context.Context, inc model.Incident, op model.Operation, cooldown time.Duration) (model.Incident, model.Operation, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	if inc.CreatedAt.IsZero() {
		inc.CreatedAt = now
	}
	if inc.StartsAt.IsZero() {
		inc.StartsAt = inc.CreatedAt
	}

	if cooldown > 0 {
		since := inc.CreatedAt.Add(-cooldown)
		var active bool
		var skipped *model.Incident
		for _, existing := range m.incidents {
			if existing.Fingerprint != inc.Fingerprint || existing.CreatedAt.Before(since) {
				continue
			}
			if existing.Status == model.StatusSkipped {
				copy := existing
				skipped = &copy
				continue
			}
			active = true
		}
		if active {
			if skipped != nil {
				return model.CloneIncident(*skipped), model.Operation{}, true, nil
			}
			inc.Status = model.StatusSkipped
			m.nextInc++
			inc.ID = m.nextInc
			m.incidents[inc.ID] = model.CloneIncident(inc)
			return model.CloneIncident(inc), model.Operation{}, true, nil
		}
	}

	if op.CreatedAt.IsZero() {
		op.CreatedAt = now
	}
	if op.UpdatedAt.IsZero() {
		op.UpdatedAt = op.CreatedAt
	}
	m.nextInc++
	inc.ID = m.nextInc
	m.incidents[inc.ID] = model.CloneIncident(inc)
	m.nextOp++
	op.ID = m.nextOp
	op.IncidentID = inc.ID
	m.operations[op.ID] = model.CloneOperation(op)
	return model.CloneIncident(inc), model.CloneOperation(op), false, nil
}

func (m *Memory) ListIncidents(_ context.Context, limit int) ([]model.Incident, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.Incident, 0, len(m.incidents))
	for _, inc := range m.incidents {
		out = append(out, model.CloneIncident(inc))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	limit = clampLimit(limit)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *Memory) GetIncident(_ context.Context, id int64) (model.Incident, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inc, ok := m.incidents[id]
	if !ok {
		return model.Incident{}, ErrNotFound
	}
	return model.CloneIncident(inc), nil
}

func (m *Memory) ClaimK8sOperation(context.Context) (model.Operation, bool, error) {
	return m.claim(func(op model.Operation) bool {
		return op.Status == model.StatusPending && op.Action != model.ActionScript
	})
}

func (m *Memory) ClaimSurgeonOperation(_ context.Context, surgeonID string) (model.Operation, bool, error) {
	return m.claim(func(op model.Operation) bool {
		return op.Status == model.StatusPending && op.Action == model.ActionScript && op.SurgeonID == surgeonID
	})
}

func (m *Memory) claim(match func(model.Operation) bool) (model.Operation, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var picked *model.Operation
	for _, op := range m.operations {
		if !match(op) {
			continue
		}
		if picked == nil || op.ID < picked.ID {
			copy := op
			picked = &copy
		}
	}
	if picked == nil {
		return model.Operation{}, false, nil
	}
	picked.Status = model.StatusRunning
	picked.UpdatedAt = time.Now().UTC()
	m.operations[picked.ID] = model.CloneOperation(*picked)
	if inc, ok := m.incidents[picked.IncidentID]; ok && inc.Status == model.StatusPending {
		inc.Status = model.StatusRunning
		m.incidents[inc.ID] = inc
	}
	return model.CloneOperation(*picked), true, nil
}

func (m *Memory) FinishOperation(_ context.Context, operationID, incidentID int64, status, logs string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.operations[operationID]
	if !ok {
		return ErrNotFound
	}
	op.Status = status
	op.Logs = logs
	op.UpdatedAt = time.Now().UTC()
	m.operations[operationID] = op
	if inc, ok := m.incidents[incidentID]; ok {
		inc.Status = status
		m.incidents[incidentID] = inc
	}
	return nil
}

func (m *Memory) RequeueStale(_ context.Context, updatedBefore time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, op := range m.operations {
		if op.Status == model.StatusRunning && op.UpdatedAt.Before(updatedBefore) {
			op.Status = model.StatusPending
			op.UpdatedAt = time.Now().UTC()
			m.operations[id] = op
			if inc, ok := m.incidents[op.IncidentID]; ok && inc.Status == model.StatusRunning {
				inc.Status = model.StatusPending
				m.incidents[inc.ID] = inc
			}
			n++
		}
	}
	return n, nil
}

func (m *Memory) GetOperation(_ context.Context, id int64) (model.Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.operations[id]
	if !ok {
		return model.Operation{}, ErrNotFound
	}
	return model.CloneOperation(op), nil
}

func (m *Memory) ListOperations(_ context.Context, limit int) ([]model.Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]model.Operation, 0, len(m.operations))
	for _, op := range m.operations {
		out = append(out, model.CloneOperation(op))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	limit = clampLimit(limit)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
