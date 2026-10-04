package store

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hospital/internal/model"
)

func TestMemoryRunbookMatchAndCooldown(t *testing.T) {
	st := NewMemory()
	ctx := context.Background()
	if _, err := st.UpsertRunbook(ctx, model.Runbook{AlertName: "KubePodCrashLooping", Action: model.ActionDeletePod}); err != nil {
		t.Fatal(err)
	}
	specific := model.Runbook{
		AlertName:   "KubePodCrashLooping",
		Action:      model.ActionDeletePod,
		MatchLabels: map[string]string{"namespace": "payments"},
	}
	saved, err := st.UpsertRunbook(ctx, specific)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.MatchRunbook(ctx, "KubePodCrashLooping", map[string]string{"namespace": "payments", "pod": "api"})
	if err != nil || !ok || got.ID != saved.ID {
		t.Fatalf("specific match = %#v ok=%v err=%v", got, ok, err)
	}
	generic, ok, err := st.MatchRunbook(ctx, "KubePodCrashLooping", map[string]string{"namespace": "other", "pod": "api"})
	if err != nil || !ok || generic.ID == saved.ID {
		t.Fatalf("generic match = %#v ok=%v err=%v", generic, ok, err)
	}

	inc := model.Incident{AlertName: "KubePodCrashLooping", Fingerprint: "fp", Status: model.StatusPending}
	op := model.Operation{Action: model.ActionDeletePod, Status: model.StatusPending, Namespace: "payments", Name: "api"}
	if _, _, skipped, err := st.CreateRemediation(ctx, inc, op, time.Minute); err != nil || skipped {
		t.Fatalf("first create skipped=%v err=%v", skipped, err)
	}
	if _, _, skipped, err := st.CreateRemediation(ctx, inc, op, time.Minute); err != nil || !skipped {
		t.Fatalf("second create skipped=%v err=%v", skipped, err)
	}
	if _, _, skipped, err := st.CreateRemediation(ctx, inc, op, time.Minute); err != nil || !skipped {
		t.Fatalf("third create skipped=%v err=%v", skipped, err)
	}
	incidents, err := st.ListIncidents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 2 {
		t.Fatalf("incidents = %d, want one remediation and one skip", len(incidents))
	}
}

func TestMemoryClaimIsExclusive(t *testing.T) {
	st := NewMemory()
	ctx := context.Background()
	_, _, skipped, err := st.CreateRemediation(ctx, model.Incident{
		AlertName:   "n",
		Fingerprint: "fp",
		Status:      model.StatusPending,
	}, model.Operation{Action: model.ActionDeletePod, Status: model.StatusPending}, 0)
	if err != nil || skipped {
		t.Fatal(err, skipped)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ok, err := st.ClaimK8sOperation(ctx)
			if err != nil {
				t.Error(err)
			}
			if ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("claims = %d", wins.Load())
	}
}

func TestMemoryRequeueStale(t *testing.T) {
	st := NewMemory()
	ctx := context.Background()
	_, op, _, err := st.CreateRemediation(ctx, model.Incident{
		AlertName:   "n",
		Fingerprint: "fp",
		Status:      model.StatusRunning,
	}, model.Operation{
		Action:    model.ActionCordon,
		Status:    model.StatusRunning,
		UpdatedAt: time.Now().Add(-time.Hour),
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	n, err := st.RequeueStale(ctx, time.Now().Add(-time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("requeue = %d err=%v", n, err)
	}
	got, err := st.GetOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusPending {
		t.Fatalf("status = %s", got.Status)
	}
}
