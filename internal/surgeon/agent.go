package surgeon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"hospital/internal/model"
)

// Agent long-polls Hospital and runs script operations on this machine.
type Agent struct {
	BaseURL  string
	ID       string
	Token    string
	Client   *http.Client
	Interval time.Duration
	Log      *slog.Logger
}

func (a *Agent) Run(ctx context.Context) error {
	if a.Log == nil {
		a.Log = slog.Default()
	}
	backoff := a.Interval
	if backoff <= 0 {
		backoff = 2 * time.Second
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		worked, err := a.Once(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			a.Log.Error("surgeon cycle", "error", err)
			if !sleep(ctx, backoff) {
				return ctx.Err()
			}
			continue
		}
		if !worked {
			continue
		}
	}
}

// Once claims at most one operation, runs it, and reports the result.
// worked is false when Hospital has nothing queued.
func (a *Agent) Once(ctx context.Context) (bool, error) {
	op, ok, err := a.poll(ctx)
	if err != nil || !ok {
		return false, err
	}
	logs, status := a.execute(ctx, op)
	if err := a.report(ctx, op.ID, status, logs); err != nil {
		return true, err
	}
	return true, nil
}

func (a *Agent) poll(ctx context.Context) (model.Operation, bool, error) {
	endpoint, err := url.Parse(strings.TrimRight(a.BaseURL, "/") + "/v1/operation")
	if err != nil {
		return model.Operation{}, false, err
	}
	q := endpoint.Query()
	q.Set("surgeon_id", a.ID)
	endpoint.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return model.Operation{}, false, err
	}
	a.authorize(req)
	resp, err := a.client().Do(req)
	if err != nil {
		return model.Operation{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return model.Operation{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return model.Operation{}, false, fmt.Errorf("poll %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var op model.Operation
	if err := json.NewDecoder(resp.Body).Decode(&op); err != nil {
		return model.Operation{}, false, err
	}
	return op, true, nil
}

func (a *Agent) execute(ctx context.Context, op model.Operation) (string, string) {
	if len(op.Command) == 0 {
		return "operation has no command", model.StatusFailed
	}
	timeout := commandTimeout(op.Parameters)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, op.Command[0], op.Command[1:]...)
	cmd.Env = operationEnv(os.Environ(), op)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	logs := buf.String()
	if err != nil {
		if logs != "" {
			logs += "\n"
		}
		logs += err.Error()
		return logs, model.StatusFailed
	}
	return logs, model.StatusSucceeded
}

func (a *Agent) report(ctx context.Context, id int64, status, logs string) error {
	body, err := json.Marshal(map[string]any{
		"operation_id": id,
		"status":       status,
		"logs":         logs,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.BaseURL, "/")+"/v1/report", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	a.authorize(req)
	resp, err := a.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("report %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

func (a *Agent) authorize(req *http.Request) {
	if a.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
}

func (a *Agent) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return http.DefaultClient
}

func commandTimeout(params map[string]string) time.Duration {
	n, err := strconv.Atoi(params["timeout_seconds"])
	if err != nil || n <= 0 || n > 300 {
		return 60 * time.Second
	}
	return time.Duration(n) * time.Second
}

func operationEnv(base []string, op model.Operation) []string {
	env := append([]string{}, base...)
	env = append(env,
		"HOSPITAL_OPERATION_ID="+strconv.FormatInt(op.ID, 10),
		"HOSPITAL_NAMESPACE="+safe(op.Namespace),
		"HOSPITAL_TARGET="+safe(op.Name),
		"HOSPITAL_ACTION="+safe(op.Action),
	)
	for k, v := range op.Labels {
		env = append(env, envKey(k)+"="+safe(v))
	}
	return env
}

func envKey(label string) string {
	var b strings.Builder
	b.WriteString("HOSPITAL_LABEL_")
	for _, r := range strings.ToUpper(label) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func safe(v string) string {
	return strings.ReplaceAll(v, "\x00", "")
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
