package surgeon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"hospital/internal/model"
)

func TestAgentRunsCommandAndReports(t *testing.T) {
	var reported map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/operation":
			if r.URL.Query().Get("surgeon_id") != "node-a" {
				t.Errorf("surgeon_id = %s", r.URL.Query().Get("surgeon_id"))
			}
			_ = json.NewEncoder(w).Encode(model.Operation{
				ID:      7,
				Action:  model.ActionScript,
				Command: []string{"/bin/echo", "hello"},
				Labels:  map[string]string{"node": "node-a"},
			})
		case "/v1/report":
			if err := json.NewDecoder(r.Body).Decode(&reported); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"status":"succeeded"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	agent := &Agent{BaseURL: srv.URL, ID: "node-a", Client: srv.Client()}
	worked, err := agent.Once(context.Background())
	if err != nil || !worked {
		t.Fatal(err, worked)
	}
	if reported["status"] != model.StatusSucceeded {
		t.Fatalf("report = %#v", reported)
	}
	if reported["logs"] != "hello\n" {
		t.Fatalf("logs = %#v", reported["logs"])
	}
}

func TestAgentNoWork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	agent := &Agent{BaseURL: srv.URL, ID: "node-a", Client: srv.Client(), Interval: time.Millisecond}
	worked, err := agent.Once(context.Background())
	if err != nil || worked {
		t.Fatal(err, worked)
	}
}
