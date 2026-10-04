package model

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	ActionDeletePod      = "delete-pod"
	ActionRolloutRestart = "rollout-restart"
	ActionScale          = "scale"
	ActionCordon         = "cordon"
	ActionUncordon       = "uncordon"
	ActionScript         = "script"

	KindPod         = "Pod"
	KindDeployment  = "Deployment"
	KindStatefulSet = "StatefulSet"
	KindNode        = "Node"

	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusSkipped   = "skipped"
)

// Runbook maps an Alertmanager alert to one remediation.
type Runbook struct {
	ID              int64             `json:"id,omitempty"`
	AlertName       string            `json:"alert_name"`
	MatchLabels     map[string]string `json:"match_labels,omitempty"`
	Action          string            `json:"action"`
	TargetKind      string            `json:"target_kind,omitempty"`
	NamespaceLabel  string            `json:"namespace_label,omitempty"`
	NameLabel       string            `json:"name_label,omitempty"`
	SurgeonLabel    string            `json:"surgeon_label,omitempty"`
	Command         []string          `json:"command,omitempty"`
	Parameters      map[string]string `json:"parameters,omitempty"`
	CooldownSeconds *int              `json:"cooldown_seconds,omitempty"`
}

// Incident is one firing alert that Hospital considered.
type Incident struct {
	ID          int64             `json:"id"`
	AlertName   string            `json:"alert_name"`
	Fingerprint string            `json:"fingerprint"`
	Status      string            `json:"status"`
	Summary     string            `json:"summary,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	StartsAt    time.Time         `json:"starts_at"`
	CreatedAt   time.Time         `json:"created_at"`
}

// Operation is a single remediation step for an incident.
type Operation struct {
	ID         int64             `json:"id"`
	IncidentID int64             `json:"incident_id"`
	SurgeonID  string            `json:"surgeon_id,omitempty"`
	Action     string            `json:"action"`
	TargetKind string            `json:"target_kind,omitempty"`
	Namespace  string            `json:"namespace,omitempty"`
	Name       string            `json:"name,omitempty"`
	Command    []string          `json:"command,omitempty"`
	Parameters map[string]string `json:"parameters,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Status     string            `json:"status"`
	Logs       string            `json:"logs,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
}

// Normalize fills defaults and rejects runbooks that cannot be executed safely.
func (r *Runbook) Normalize() error {
	r.AlertName = strings.TrimSpace(r.AlertName)
	if r.AlertName == "" {
		return errors.New("alert_name is required")
	}
	r.MatchLabels = CopyMap(r.MatchLabels)
	r.Parameters = CopyMap(r.Parameters)
	r.Command = CopyStrings(r.Command)

	switch r.Action {
	case ActionDeletePod:
		defaultString(&r.NamespaceLabel, "namespace")
		defaultString(&r.NameLabel, "pod")
		r.TargetKind = KindPod
	case ActionRolloutRestart, ActionScale:
		defaultString(&r.NamespaceLabel, "namespace")
		switch strings.ToLower(r.TargetKind) {
		case "", strings.ToLower(KindDeployment):
			r.TargetKind = KindDeployment
			defaultString(&r.NameLabel, "deployment")
		case strings.ToLower(KindStatefulSet):
			r.TargetKind = KindStatefulSet
			defaultString(&r.NameLabel, "statefulset")
		default:
			return fmt.Errorf("unsupported target_kind %q", r.TargetKind)
		}
		if r.Action == ActionScale {
			n, err := strconv.Atoi(r.Parameters["replicas"])
			if err != nil || n < 0 {
				return errors.New("scale action requires parameters.replicas >= 0")
			}
		}
	case ActionCordon, ActionUncordon:
		r.TargetKind = KindNode
		defaultString(&r.NameLabel, "node")
	case ActionScript:
		defaultString(&r.SurgeonLabel, "node")
		if len(r.Command) == 0 || !filepath.IsAbs(r.Command[0]) {
			return errors.New("script action requires an absolute command path")
		}
		for _, arg := range r.Command {
			if strings.TrimSpace(arg) == "" {
				return errors.New("script command contains an empty argument")
			}
		}
	default:
		return fmt.Errorf("unsupported action %q", r.Action)
	}
	if r.CooldownSeconds != nil && *r.CooldownSeconds < 0 {
		return errors.New("cooldown_seconds must be >= 0")
	}
	return nil
}

// Cooldown is how long an identical target is left alone after a remediation.
// A missing cooldown uses five minutes. Zero disables the cooldown.
func (r Runbook) Cooldown() time.Duration {
	if r.CooldownSeconds == nil {
		return 5 * time.Minute
	}
	return time.Duration(*r.CooldownSeconds) * time.Second
}

// Signature identifies a runbook independent of its numeric id.
func (r Runbook) Signature() string {
	keys := make([]string, 0, len(r.MatchLabels))
	for k := range r.MatchLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(r.AlertName)
	for _, k := range keys {
		b.WriteByte(0)
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(r.MatchLabels[k])
	}
	return b.String()
}

// Matches reports whether this runbook applies to the alert.
func (r Runbook) Matches(alertName string, labels map[string]string) bool {
	if r.AlertName != alertName {
		return false
	}
	for k, want := range r.MatchLabels {
		if labels[k] != want {
			return false
		}
	}
	return true
}

// Specificity is the number of extra label matches. Higher wins.
func (r Runbook) Specificity() int {
	return len(r.MatchLabels)
}

// Resolve reads the target out of alert labels.
func (r Runbook) Resolve(labels map[string]string) (namespace, name, surgeonID string, err error) {
	switch r.Action {
	case ActionCordon, ActionUncordon:
		name = labels[r.NameLabel]
		if name == "" {
			err = fmt.Errorf("label %q is empty", r.NameLabel)
		}
	case ActionScript:
		surgeonID = labels[r.SurgeonLabel]
		if surgeonID == "" {
			err = fmt.Errorf("label %q is empty", r.SurgeonLabel)
			return
		}
		if r.NamespaceLabel != "" {
			namespace = labels[r.NamespaceLabel]
		}
		if r.NameLabel != "" {
			name = labels[r.NameLabel]
		}
	default:
		namespace = labels[r.NamespaceLabel]
		name = labels[r.NameLabel]
		if namespace == "" || name == "" {
			err = fmt.Errorf("labels %q and %q are required", r.NamespaceLabel, r.NameLabel)
		}
	}
	return namespace, name, surgeonID, err
}

// Fingerprint identifies the same failure on the same target.
func Fingerprint(alertName, namespace, name, surgeonID string) string {
	return alertName + "|" + namespace + "|" + name + "|" + surgeonID
}

func CopyMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func CopyStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return append([]string(nil), in...)
}

func CloneRunbook(r Runbook) Runbook {
	r.MatchLabels = CopyMap(r.MatchLabels)
	r.Parameters = CopyMap(r.Parameters)
	r.Command = CopyStrings(r.Command)
	if r.CooldownSeconds != nil {
		n := *r.CooldownSeconds
		r.CooldownSeconds = &n
	}
	return r
}

func CloneIncident(in Incident) Incident {
	in.Labels = CopyMap(in.Labels)
	return in
}

func CloneOperation(op Operation) Operation {
	op.Command = CopyStrings(op.Command)
	op.Parameters = CopyMap(op.Parameters)
	op.Labels = CopyMap(op.Labels)
	return op
}

func defaultString(dst *string, fallback string) {
	if strings.TrimSpace(*dst) == "" {
		*dst = fallback
	}
}
