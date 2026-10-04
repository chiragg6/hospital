package remedy

import (
	"context"
	"testing"
	"time"

	"hospital/internal/alert"
	"hospital/internal/model"
	"hospital/internal/store"
)

func TestReceiveQueuesAndSkips(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	if _, err := st.UpsertRunbook(ctx, model.Runbook{AlertName: "KubePodCrashLooping", Action: model.ActionDeletePod}); err != nil {
		t.Fatal(err)
	}
	msg := alert.Message{Alerts: []alert.Alert{
		{
			Status: "firing",
			Labels: map[string]string{"alertname": "KubePodCrashLooping", "namespace": "payments", "pod": "api-0"},
			Annotations: map[string]string{
				"summary": "crash looping",
			},
			StartsAt: time.Now().UTC(),
		},
		{Status: "resolved", Labels: map[string]string{"alertname": "KubePodCrashLooping", "namespace": "payments", "pod": "api-0"}},
		{Status: "firing", Labels: map[string]string{"alertname": "Unknown"}},
		{Status: "firing", Labels: map[string]string{"alertname": "KubePodCrashLooping", "namespace": "payments", "pod": "api-0"}},
	}}
	result, err := Receive(ctx, st, msg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Received != 4 || result.Remediated != 1 || result.Unmapped != 1 || result.Skipped != 1 {
		t.Fatalf("result = %+v", result)
	}
	ops, err := st.ListOperations(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Namespace != "payments" || ops[0].Name != "api-0" || ops[0].Status != model.StatusPending {
		t.Fatalf("ops = %+v", ops)
	}
}

func TestReceiveUnresolvedTarget(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	if _, err := st.UpsertRunbook(ctx, model.Runbook{AlertName: "KubePodCrashLooping", Action: model.ActionDeletePod}); err != nil {
		t.Fatal(err)
	}
	result, err := Receive(ctx, st, alert.Message{Alerts: []alert.Alert{{
		Status: "firing",
		Labels: map[string]string{"alertname": "KubePodCrashLooping"},
	}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Failed != 1 || result.Remediated != 0 {
		t.Fatalf("result = %+v", result)
	}
	ops, err := st.ListOperations(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Status != model.StatusFailed {
		t.Fatalf("ops = %+v", ops)
	}
}
