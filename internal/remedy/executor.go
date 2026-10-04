package remedy

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"hospital/internal/model"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const restartAnnotation = "kubectl.kubernetes.io/restartedAt"

// Executor applies one remediation.
type Executor interface {
	Execute(ctx context.Context, op model.Operation) (string, error)
}

// Policy decides which namespaces an API action may touch.
type Policy struct {
	Allowed     map[string]struct{}
	AllowSystem bool
}

func NewPolicy(allowed []string, allowSystem bool) Policy {
	var set map[string]struct{}
	if len(allowed) > 0 {
		set = make(map[string]struct{}, len(allowed))
		for _, ns := range allowed {
			set[ns] = struct{}{}
		}
	}
	return Policy{Allowed: set, AllowSystem: allowSystem}
}

func (p Policy) Check(op model.Operation) error {
	if op.Action == model.ActionCordon || op.Action == model.ActionUncordon || op.Action == model.ActionScript {
		return nil
	}
	if len(p.Allowed) > 0 {
		if _, ok := p.Allowed[op.Namespace]; !ok {
			return fmt.Errorf("namespace %q is not in the allow list", op.Namespace)
		}
		return nil
	}
	if !p.AllowSystem && isSystemNamespace(op.Namespace) {
		return fmt.Errorf("namespace %q is a system namespace", op.Namespace)
	}
	return nil
}

func isSystemNamespace(ns string) bool {
	switch ns {
	case "kube-system", "kube-public", "kube-node-lease":
		return true
	default:
		return false
	}
}

// KubernetesExecutor talks to the cluster API.
type KubernetesExecutor struct {
	client kubernetes.Interface
	policy Policy
	dryRun bool
	now    func() time.Time
}

func NewKubernetesExecutor(client kubernetes.Interface, policy Policy, dryRun bool) *KubernetesExecutor {
	return &KubernetesExecutor{
		client: client,
		policy: policy,
		dryRun: dryRun,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

func (e *KubernetesExecutor) Execute(ctx context.Context, op model.Operation) (string, error) {
	if err := e.policy.Check(op); err != nil {
		return "", err
	}
	if e.dryRun {
		return fmt.Sprintf("dry-run %s", Describe(op)), nil
	}
	if e.client == nil {
		return "", fmt.Errorf("kubernetes client is not configured")
	}
	switch op.Action {
	case model.ActionDeletePod:
		return e.deletePod(ctx, op)
	case model.ActionRolloutRestart:
		return e.rolloutRestart(ctx, op)
	case model.ActionScale:
		return e.scale(ctx, op)
	case model.ActionCordon:
		return e.setUnschedulable(ctx, op, true)
	case model.ActionUncordon:
		return e.setUnschedulable(ctx, op, false)
	default:
		return "", fmt.Errorf("executor cannot run action %q", op.Action)
	}
}

func (e *KubernetesExecutor) deletePod(ctx context.Context, op model.Operation) (string, error) {
	err := e.client.CoreV1().Pods(op.Namespace).Delete(ctx, op.Name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Sprintf("pod %s/%s already absent", op.Namespace, op.Name), nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("deleted pod %s/%s", op.Namespace, op.Name), nil
}

func (e *KubernetesExecutor) rolloutRestart(ctx context.Context, op model.Operation) (string, error) {
	stamp := e.now().Format(time.RFC3339)
	switch op.TargetKind {
	case model.KindDeployment:
		dep, err := e.client.AppsV1().Deployments(op.Namespace).Get(ctx, op.Name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		if dep.Spec.Template.Annotations == nil {
			dep.Spec.Template.Annotations = map[string]string{}
		}
		dep.Spec.Template.Annotations[restartAnnotation] = stamp
		if _, err := e.client.AppsV1().Deployments(op.Namespace).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
			return "", err
		}
	case model.KindStatefulSet:
		sts, err := e.client.AppsV1().StatefulSets(op.Namespace).Get(ctx, op.Name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		if sts.Spec.Template.Annotations == nil {
			sts.Spec.Template.Annotations = map[string]string{}
		}
		sts.Spec.Template.Annotations[restartAnnotation] = stamp
		if _, err := e.client.AppsV1().StatefulSets(op.Namespace).Update(ctx, sts, metav1.UpdateOptions{}); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("rollout restart does not support %s", op.TargetKind)
	}
	return fmt.Sprintf("restarted %s %s/%s", op.TargetKind, op.Namespace, op.Name), nil
}

func (e *KubernetesExecutor) scale(ctx context.Context, op model.Operation) (string, error) {
	n, err := strconv.Atoi(op.Parameters["replicas"])
	if err != nil || n < 0 {
		return "", fmt.Errorf("invalid replicas %q", op.Parameters["replicas"])
	}
	replicas := int32(n)
	switch op.TargetKind {
	case model.KindDeployment:
		dep, err := e.client.AppsV1().Deployments(op.Namespace).Get(ctx, op.Name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		dep.Spec.Replicas = &replicas
		if _, err := e.client.AppsV1().Deployments(op.Namespace).Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
			return "", err
		}
	case model.KindStatefulSet:
		sts, err := e.client.AppsV1().StatefulSets(op.Namespace).Get(ctx, op.Name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		sts.Spec.Replicas = &replicas
		if _, err := e.client.AppsV1().StatefulSets(op.Namespace).Update(ctx, sts, metav1.UpdateOptions{}); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("scale does not support %s", op.TargetKind)
	}
	return fmt.Sprintf("scaled %s %s/%s to %d", op.TargetKind, op.Namespace, op.Name, replicas), nil
}

func (e *KubernetesExecutor) setUnschedulable(ctx context.Context, op model.Operation, unschedulable bool) (string, error) {
	node, err := e.client.CoreV1().Nodes().Get(ctx, op.Name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	node.Spec.Unschedulable = unschedulable
	if _, err := e.client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		return "", err
	}
	verb := "uncordoned"
	if unschedulable {
		verb = "cordoned"
	}
	return fmt.Sprintf("%s node %s", verb, op.Name), nil
}
