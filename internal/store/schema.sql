CREATE TABLE IF NOT EXISTS runbooks (
    id               BIGSERIAL PRIMARY KEY,
    signature        TEXT NOT NULL UNIQUE,
    alert_name       TEXT NOT NULL,
    match_labels     JSONB NOT NULL DEFAULT '{}'::jsonb,
    action           TEXT NOT NULL,
    target_kind      TEXT NOT NULL DEFAULT '',
    namespace_label  TEXT NOT NULL DEFAULT '',
    name_label       TEXT NOT NULL DEFAULT '',
    surgeon_label    TEXT NOT NULL DEFAULT '',
    command          JSONB NOT NULL DEFAULT '[]'::jsonb,
    parameters       JSONB NOT NULL DEFAULT '{}'::jsonb,
    cooldown_seconds INT
);

CREATE TABLE IF NOT EXISTS incidents (
    id          BIGSERIAL PRIMARY KEY,
    alert_name  TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    status      TEXT NOT NULL,
    summary     TEXT NOT NULL DEFAULT '',
    labels      JSONB NOT NULL DEFAULT '{}'::jsonb,
    starts_at   TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS incidents_fingerprint_created_idx
    ON incidents (fingerprint, created_at DESC);

CREATE TABLE IF NOT EXISTS operations (
    id          BIGSERIAL PRIMARY KEY,
    incident_id BIGINT NOT NULL REFERENCES incidents (id),
    surgeon_id  TEXT NOT NULL DEFAULT '',
    action      TEXT NOT NULL,
    target_kind TEXT NOT NULL DEFAULT '',
    namespace   TEXT NOT NULL DEFAULT '',
    name        TEXT NOT NULL DEFAULT '',
    command     JSONB NOT NULL DEFAULT '[]'::jsonb,
    parameters  JSONB NOT NULL DEFAULT '{}'::jsonb,
    labels      JSONB NOT NULL DEFAULT '{}'::jsonb,
    status      TEXT NOT NULL,
    logs        TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS operations_claim_idx
    ON operations (status, action, id);

CREATE INDEX IF NOT EXISTS operations_surgeon_idx
    ON operations (surgeon_id, status, id);
