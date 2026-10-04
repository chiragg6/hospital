package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"hospital/internal/model"
	"hospital/internal/remedy"
)

// Slack posts a short remediation summary to an incoming webhook.
type Slack struct {
	URL    string
	Client *http.Client
}

func NewSlack(url string) *Slack {
	return &Slack{
		URL: url,
		Client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

func (s *Slack) Notify(ctx context.Context, inc model.Incident, op model.Operation) error {
	if s == nil || s.URL == "" {
		return nil
	}
	logs := op.Logs
	if len(logs) > 1500 {
		logs = logs[:1500] + "..."
	}
	text := fmt.Sprintf("Hospital %s incident %d: %s (%s)\n%s", op.Status, inc.ID, inc.AlertName, remedy.Describe(op), logs)
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("slack webhook returned %s", resp.Status)
	}
	return nil
}
