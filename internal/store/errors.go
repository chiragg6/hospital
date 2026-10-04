package store

import "errors"

// ErrDuplicateRunbook is returned when a different runbook already uses the same match.
var ErrDuplicateRunbook = errors.New("runbook already exists")
