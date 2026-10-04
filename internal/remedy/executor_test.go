package remedy

import (
	"context"
	"testing"
	"time"

	"hospital/internal/model"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestExecutorDeleteRolloutScaleCordon(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "payments"}},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "payments"},
			Spec: appsv1.DeploymentSpec{
				Replicas: int32ptr(2),
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "api", Image: "nginx"}}}},
			},
		},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
	)
	exec := NewKubernetesExecutor(client, NewPolicy(nil, false), false)
	exec.now = func() time.Time { return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) }
	ctx := context.Background()

	logs, err := exec.Execute(ctx, model.Operation{Action: model.ActionDeletePod, Namespace: "payments", Name: "api-0"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Pods("payments").Get(ctx, "api-0", metav1.GetOptions{}); err == nil {
		t.Fatalf("pod still present after %s", logs)
	}

	if _, err := exec.Execute(ctx, model.Operation{Action: model.ActionRolloutRestart, TargetKind: model.KindDeployment, Namespace: "payments", Name: "api"}); err != nil {
		t.Fatal(err)
	}
	dep, err := client.AppsV1().Deployments("payments").Get(ctx, "api", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if dep.Spec.Template.Annotations[restartAnnotation] != "2026-10-01T00:00:00Z" {
		t.Fatalf("annotation = %v", dep.Spec.Template.Annotations)
	}

	if _, err := exec.Execute(ctx, model.Operation{
		Action:     model.ActionScale,
		TargetKind: model.KindDeployment,
		Namespace:  "payments",
		Name:       "api",
		Parameters: map[string]string{"replicas": "5"},
	}); err != nil {
		t.Fatal(err)
	}
	dep, err = client.AppsV1().Deployments("payments").Get(ctx, "api", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 5 {
		t.Fatalf("replicas = %v", dep.Spec.Replicas)
	}

	if _, err := exec.Execute(ctx, model.Operation{Action: model.ActionCordon, Name: "node-a"}); err != nil {
		t.Fatal(err)
	}
	node, err := client.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !node.Spec.Unschedulable {
		t.Fatal("node was not cordoned")
	}
}

func TestExecutorRejectsSystemNamespace(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "kube-apiserver", Namespace: "kube-system"}})
	exec := NewKubernetesExecutor(client, NewPolicy(nil, false), false)
	_, err := exec.Execute(context.Background(), model.Operation{Action: model.ActionDeletePod, Namespace: "kube-system", Name: "kube-apiserver"})
	if err == nil {
		t.Fatal("expected system namespace to be rejected")
	}
	if _, err := client.CoreV1().Pods("kube-system").Get(context.Background(), "kube-apiserver", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestExecutorDryRunDoesNotMutate(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-0", Namespace: "payments"}})
	exec := NewKubernetesExecutor(client, NewPolicy(nil, false), true)
	logs, err := exec.Execute(context.Background(), model.Operation{Action: model.ActionDeletePod, TargetKind: model.KindPod, Namespace: "payments", Name: "api-0"})
	if err != nil {
		t.Fatal(err, logs)
	}
	if _, err := client.CoreV1().Pods("payments").Get(context.Background(), "api-0", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
}

func int32ptr(n int32) *int32 { return &n }
