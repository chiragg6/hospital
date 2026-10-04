package runbook

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"hospital/internal/model"
	"hospital/internal/store"
)

// LoadFile upserts every runbook in a JSON array.
func LoadFile(ctx context.Context, st store.Store, path string) error {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var books []model.Runbook
	if err := json.NewDecoder(f).Decode(&books); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	for _, rb := range books {
		if _, err := st.UpsertRunbook(ctx, rb); err != nil {
			return fmt.Errorf("load runbook %q: %w", rb.AlertName, err)
		}
	}
	return nil
}
