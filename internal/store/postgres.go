package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"hospital/internal/model"

	"github.com/lib/pq"
)

//go:embed schema.sql
var schemaSQL string

// Postgres stores Hospital state in PostgreSQL.
type Postgres struct {
	db *sql.DB
}

func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Postgres{db: db}, nil
}

func (p *Postgres) Ping(ctx context.Context) error { return p.db.PingContext(ctx) }
func (p *Postgres) Close() error                   { return p.db.Close() }

func (p *Postgres) UpsertRunbook(ctx context.Context, rb model.Runbook) (model.Runbook, error) {
	if err := rb.Normalize(); err != nil {
		return model.Runbook{}, err
	}
	if rb.ID != 0 {
		row := p.db.QueryRowContext(ctx, `
UPDATE runbooks SET
    signature = $2,
    alert_name = $3,
    match_labels = $4,
    action = $5,
    target_kind = $6,
    namespace_label = $7,
    name_label = $8,
    surgeon_label = $9,
    command = $10,
    parameters = $11,
    cooldown_seconds = $12
WHERE id = $1
RETURNING id, signature, alert_name, match_labels, action, target_kind, namespace_label, name_label, surgeon_label, command, parameters, cooldown_seconds
`, rb.ID, rb.Signature(), rb.AlertName, jsonMap(rb.MatchLabels), rb.Action, rb.TargetKind, rb.NamespaceLabel, rb.NameLabel, rb.SurgeonLabel, jsonList(rb.Command), jsonMap(rb.Parameters), cooldownValue(rb.CooldownSeconds))
		out, err := scanRunbook(row)
		if errors.Is(err, sql.ErrNoRows) {
			return model.Runbook{}, ErrNotFound
		}
		if isUnique(err) {
			return model.Runbook{}, ErrDuplicateRunbook
		}
		return out, err
	}

	row := p.db.QueryRowContext(ctx, `
INSERT INTO runbooks (
    signature, alert_name, match_labels, action, target_kind, namespace_label, name_label, surgeon_label, command, parameters, cooldown_seconds
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
ON CONFLICT (signature) DO UPDATE SET
    alert_name = EXCLUDED.alert_name,
    match_labels = EXCLUDED.match_labels,
    action = EXCLUDED.action,
    target_kind = EXCLUDED.target_kind,
    namespace_label = EXCLUDED.namespace_label,
    name_label = EXCLUDED.name_label,
    surgeon_label = EXCLUDED.surgeon_label,
    command = EXCLUDED.command,
    parameters = EXCLUDED.parameters,
    cooldown_seconds = EXCLUDED.cooldown_seconds
RETURNING id, signature, alert_name, match_labels, action, target_kind, namespace_label, name_label, surgeon_label, command, parameters, cooldown_seconds
`, rb.Signature(), rb.AlertName, jsonMap(rb.MatchLabels), rb.Action, rb.TargetKind, rb.NamespaceLabel, rb.NameLabel, rb.SurgeonLabel, jsonList(rb.Command), jsonMap(rb.Parameters), cooldownValue(rb.CooldownSeconds))
	return scanRunbook(row)
}

func (p *Postgres) DeleteRunbook(ctx context.Context, id int64) error {
	res, err := p.db.ExecContext(ctx, `DELETE FROM runbooks WHERE id = $1`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (p *Postgres) ListRunbooks(ctx context.Context) ([]model.Runbook, error) {
	rows, err := p.db.QueryContext(ctx, `
SELECT id, signature, alert_name, match_labels, action, target_kind, namespace_label, name_label, surgeon_label, command, parameters, cooldown_seconds
FROM runbooks
ORDER BY alert_name, id
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Runbook
	for rows.Next() {
		rb, err := scanRunbook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rb)
	}
	return out, rows.Err()
}

func (p *Postgres) MatchRunbook(ctx context.Context, alertName string, labels map[string]string) (model.Runbook, bool, error) {
	books, err := p.ListRunbooks(ctx)
	if err != nil {
		return model.Runbook{}, false, err
	}
	var (
		best  model.Runbook
		found bool
	)
	for _, rb := range books {
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
	return best, true, nil
}

func (p *Postgres) CreateRemediation(ctx context.Context, inc model.Incident, op model.Operation, cooldown time.Duration) (model.Incident, model.Operation, bool, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Incident{}, model.Operation{}, false, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, inc.Fingerprint); err != nil {
		return model.Incident{}, model.Operation{}, false, err
	}
	now := time.Now().UTC()
	if inc.CreatedAt.IsZero() {
		inc.CreatedAt = now
	}
	if inc.StartsAt.IsZero() {
		inc.StartsAt = inc.CreatedAt
	}

	if cooldown > 0 {
		since := inc.CreatedAt.Add(-cooldown)
		var activeID int64
		err := tx.QueryRowContext(ctx, `
SELECT id FROM incidents
WHERE fingerprint = $1 AND status <> $2 AND created_at >= $3
ORDER BY id DESC
LIMIT 1
`, inc.Fingerprint, model.StatusSkipped, since).Scan(&activeID)
		if err == nil {
			skipped, err := p.insertSkip(ctx, tx, inc, since)
			if err != nil {
				return model.Incident{}, model.Operation{}, false, err
			}
			if err := tx.Commit(); err != nil {
				return model.Incident{}, model.Operation{}, false, err
			}
			return skipped, model.Operation{}, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return model.Incident{}, model.Operation{}, false, err
		}
	}

	if err := tx.QueryRowContext(ctx, `
INSERT INTO incidents (alert_name, fingerprint, status, summary, labels, starts_at, created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7)
RETURNING id
`, inc.AlertName, inc.Fingerprint, inc.Status, inc.Summary, jsonMap(inc.Labels), inc.StartsAt, inc.CreatedAt).Scan(&inc.ID); err != nil {
		return model.Incident{}, model.Operation{}, false, err
	}

	if op.CreatedAt.IsZero() {
		op.CreatedAt = now
	}
	if op.UpdatedAt.IsZero() {
		op.UpdatedAt = op.CreatedAt
	}
	op.IncidentID = inc.ID
	if err := tx.QueryRowContext(ctx, `
INSERT INTO operations (
    incident_id, surgeon_id, action, target_kind, namespace, name, command, parameters, labels, status, logs, created_at, updated_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
RETURNING id
`, op.IncidentID, op.SurgeonID, op.Action, op.TargetKind, op.Namespace, op.Name, jsonList(op.Command), jsonMap(op.Parameters), jsonMap(op.Labels), op.Status, op.Logs, op.CreatedAt, op.UpdatedAt).Scan(&op.ID); err != nil {
		return model.Incident{}, model.Operation{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return model.Incident{}, model.Operation{}, false, err
	}
	return inc, op, false, nil
}

func (p *Postgres) insertSkip(ctx context.Context, tx *sql.Tx, inc model.Incident, since time.Time) (model.Incident, error) {
	var existingID int64
	err := tx.QueryRowContext(ctx, `
SELECT id FROM incidents
WHERE fingerprint = $1 AND status = $2 AND created_at >= $3
ORDER BY id DESC
LIMIT 1
`, inc.Fingerprint, model.StatusSkipped, since).Scan(&existingID)
	if err == nil {
		return loadIncident(ctx, tx, existingID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.Incident{}, err
	}
	inc.Status = model.StatusSkipped
	err = tx.QueryRowContext(ctx, `
INSERT INTO incidents (alert_name, fingerprint, status, summary, labels, starts_at, created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7)
RETURNING id
`, inc.AlertName, inc.Fingerprint, inc.Status, inc.Summary, jsonMap(inc.Labels), inc.StartsAt, inc.CreatedAt).Scan(&inc.ID)
	return inc, err
}

func (p *Postgres) ListIncidents(ctx context.Context, limit int) ([]model.Incident, error) {
	rows, err := p.db.QueryContext(ctx, `
SELECT id, alert_name, fingerprint, status, summary, labels, starts_at, created_at
FROM incidents
ORDER BY id DESC
LIMIT $1
`, clampLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Incident
	for rows.Next() {
		inc, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

func (p *Postgres) GetIncident(ctx context.Context, id int64) (model.Incident, error) {
	inc, err := loadIncident(ctx, p.db, id)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Incident{}, ErrNotFound
	}
	return inc, err
}

func (p *Postgres) ClaimK8sOperation(ctx context.Context) (model.Operation, bool, error) {
	return p.claim(ctx, `
UPDATE operations SET status = 'running', updated_at = now()
WHERE id = (
    SELECT id FROM operations
    WHERE status = 'pending' AND action <> 'script'
    ORDER BY id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING id, incident_id, surgeon_id, action, target_kind, namespace, name, command, parameters, labels, status, logs, created_at, updated_at
`)
}

func (p *Postgres) ClaimSurgeonOperation(ctx context.Context, surgeonID string) (model.Operation, bool, error) {
	return p.claim(ctx, `
UPDATE operations SET status = 'running', updated_at = now()
WHERE id = (
    SELECT id FROM operations
    WHERE status = 'pending' AND action = 'script' AND surgeon_id = $1
    ORDER BY id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING id, incident_id, surgeon_id, action, target_kind, namespace, name, command, parameters, labels, status, logs, created_at, updated_at
`, surgeonID)
}

func (p *Postgres) claim(ctx context.Context, query string, args ...any) (model.Operation, bool, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Operation{}, false, err
	}
	defer tx.Rollback()

	op, err := scanOperation(tx.QueryRowContext(ctx, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return model.Operation{}, false, err
		}
		return model.Operation{}, false, nil
	}
	if err != nil {
		return model.Operation{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE incidents SET status = $2 WHERE id = $1 AND status = $3
`, op.IncidentID, model.StatusRunning, model.StatusPending); err != nil {
		return model.Operation{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return model.Operation{}, false, err
	}
	return op, true, nil
}

func (p *Postgres) FinishOperation(ctx context.Context, operationID, incidentID int64, status, logs string) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
UPDATE operations SET status = $2, logs = $3, updated_at = now() WHERE id = $1
`, operationID, status, logs)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE incidents SET status = $2 WHERE id = $1`, incidentID, status); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *Postgres) RequeueStale(ctx context.Context, updatedBefore time.Time) (int, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
UPDATE operations SET status = 'pending', updated_at = now()
WHERE status = 'running' AND updated_at < $1
RETURNING incident_id
`, updatedBefore)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `
UPDATE incidents SET status = 'pending' WHERE id = $1 AND status = 'running'
`, id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

func (p *Postgres) GetOperation(ctx context.Context, id int64) (model.Operation, error) {
	op, err := scanOperation(p.db.QueryRowContext(ctx, `
SELECT id, incident_id, surgeon_id, action, target_kind, namespace, name, command, parameters, labels, status, logs, created_at, updated_at
FROM operations WHERE id = $1
`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Operation{}, ErrNotFound
	}
	return op, err
}

func (p *Postgres) ListOperations(ctx context.Context, limit int) ([]model.Operation, error) {
	rows, err := p.db.QueryContext(ctx, `
SELECT id, incident_id, surgeon_id, action, target_kind, namespace, name, command, parameters, labels, status, logs, created_at, updated_at
FROM operations
ORDER BY id DESC
LIMIT $1
`, clampLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Operation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRunbook(row rowScanner) (model.Runbook, error) {
	var (
		rb        model.Runbook
		signature string
		matchRaw  []byte
		command   []byte
		params    []byte
		cooldown  sql.NullInt64
	)
	err := row.Scan(&rb.ID, &signature, &rb.AlertName, &matchRaw, &rb.Action, &rb.TargetKind, &rb.NamespaceLabel, &rb.NameLabel, &rb.SurgeonLabel, &command, &params, &cooldown)
	if err != nil {
		return model.Runbook{}, err
	}
	rb.MatchLabels = decodeMap(matchRaw)
	rb.Command = decodeList(command)
	rb.Parameters = decodeMap(params)
	if cooldown.Valid {
		n := int(cooldown.Int64)
		rb.CooldownSeconds = &n
	}
	return rb, nil
}

func scanIncident(row rowScanner) (model.Incident, error) {
	var (
		inc model.Incident
		raw []byte
	)
	err := row.Scan(&inc.ID, &inc.AlertName, &inc.Fingerprint, &inc.Status, &inc.Summary, &raw, &inc.StartsAt, &inc.CreatedAt)
	if err != nil {
		return model.Incident{}, err
	}
	inc.Labels = decodeMap(raw)
	return inc, nil
}

func scanOperation(row rowScanner) (model.Operation, error) {
	var (
		op      model.Operation
		command []byte
		params  []byte
		labels  []byte
	)
	err := row.Scan(&op.ID, &op.IncidentID, &op.SurgeonID, &op.Action, &op.TargetKind, &op.Namespace, &op.Name, &command, &params, &labels, &op.Status, &op.Logs, &op.CreatedAt, &op.UpdatedAt)
	if err != nil {
		return model.Operation{}, err
	}
	op.Command = decodeList(command)
	op.Parameters = decodeMap(params)
	op.Labels = decodeMap(labels)
	return op, nil
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func loadIncident(ctx context.Context, q queryRower, id int64) (model.Incident, error) {
	return scanIncident(q.QueryRowContext(ctx, `
SELECT id, alert_name, fingerprint, status, summary, labels, starts_at, created_at
FROM incidents WHERE id = $1
`, id))
}

func jsonMap(m map[string]string) []byte {
	if m == nil {
		m = map[string]string{}
	}
	b, _ := json.Marshal(m)
	return b
}

func jsonList(s []string) []byte {
	if s == nil {
		s = []string{}
	}
	b, _ := json.Marshal(s)
	return b
}

func decodeMap(raw []byte) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return nil
	}
	return m
}

func decodeList(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var s []string
	if err := json.Unmarshal(raw, &s); err != nil || len(s) == 0 {
		return nil
	}
	return s
}

func cooldownValue(n *int) any {
	if n == nil {
		return nil
	}
	return *n
}

func isUnique(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}
