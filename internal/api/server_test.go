package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"hospital/internal/model"
	"hospital/internal/notify"
	"hospital/internal/remedy"
	"hospital/internal/store"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestReceptionToKubernetesRemediation(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	if _, err := st.UpsertRunbook(ctx, model.Runbook{AlertName: "KubePodCrashLooping", Action: model.ActionDeletePod}); err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "payments"}})
	exec := remedy.NewKubernetesExecutor(client, remedy.NewPolicy(nil, false), false)
	var slackBody string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		slackBody = body["text"]
		w.WriteHeader(http.StatusOK)
	}))
	defer slackSrv.Close()
	notifier := notify.NewSlack(slackSrv.URL)
	worker := remedy.NewWorker(st, exec, notifier, time.Hour, time.Hour, nil)

	srv := New(st, notifier, "secret", time.Second, 10*time.Millisecond, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := []byte(`{
      "version": "4",
      "status": "firing",
      "alerts": [{
        "status": "firing",
        "labels": {"alertname": "KubePodCrashLooping", "namespace": "payments", "pod": "api-0"},
        "annotations": {"summary": "crash looping"},
        "startsAt": "2026-10-01T00:00:00Z"
      }]
    }`)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/reception", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s", resp.Status)
	}

	unauth, err := http.Post(ts.URL+"/v1/reception", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	unauth.Body.Close()
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth status = %s", unauth.Status)
	}

	worker.Tick(context.Background())
	if _, err := client.CoreV1().Pods("payments").Get(ctx, "api-0", metav1.GetOptions{}); err == nil {
		t.Fatal("pod was not deleted")
	}
	ops, err := st.ListOperations(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Status != model.StatusSucceeded {
		t.Fatalf("ops = %+v", ops)
	}
	if slackBody == "" {
		t.Fatal("slack was not notified")
	}
}

func TestSurgeonPollAndReport(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	_, op, _, err := st.CreateRemediation(ctx, model.Incident{
		AlertName:   "DiskFull",
		Fingerprint: "DiskFull|node-a",
		Status:      model.StatusPending,
		Summary:     "disk full",
	}, model.Operation{
		Action:    model.ActionScript,
		SurgeonID: "node-a",
		Command:   []string{"/bin/echo", "trimmed"},
		Status:    model.StatusPending,
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(st, nil, "", 50*time.Millisecond, 10*time.Millisecond, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/operation?surgeon_id=node-a")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("poll status = %s", resp.Status)
	}
	var claimed model.Operation
	if err := json.NewDecoder(resp.Body).Decode(&claimed); err != nil {
		t.Fatal(err)
	}
	if claimed.ID != op.ID || claimed.Status != model.StatusRunning {
		t.Fatalf("claimed = %+v", claimed)
	}

	report, _ := json.Marshal(map[string]any{"operation_id": claimed.ID, "status": "succeeded", "logs": "trimmed\n"})
	resp, err = http.Post(ts.URL+"/v1/report", "application/json", bytes.NewReader(report))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("report status = %s", resp.Status)
	}
	got, err := st.GetOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusSucceeded || got.Logs != "trimmed\n" {
		t.Fatalf("operation = %+v", got)
	}
}
