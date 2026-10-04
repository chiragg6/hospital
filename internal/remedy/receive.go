package remedy

import (
	"context"
	"fmt"
	"log/slog"

	"hospital/internal/alert"
	"hospital/internal/model"
	"hospital/internal/store"
)

// Result summarizes one Alertmanager webhook.
type Result struct {
	Received   int `json:"received"`
	Remediated int `json:"remediated"`
	Skipped    int `json:"skipped"`
	Unmapped   int `json:"unmapped"`
	Failed     int `json:"failed"`
}

// Receive matches firing alerts to runbooks and records operations.
func Receive(ctx context.Context, st store.Store, msg alert.Message, log *slog.Logger) (Result, error) {
	if log == nil {
		log = slog.Default()
	}
	var result Result
	for _, item := range msg.Alerts {
		result.Received++
		if !item.Firing() {
			continue
		}
		name := item.Name()
		if name == "" {
			result.Failed++
			log.Warn("alert missing alertname label")
			continue
		}
		rb, ok, err := st.MatchRunbook(ctx, name, item.Labels)
		if err != nil {
			return result, err
		}
		if !ok {
			result.Unmapped++
			log.Info("no runbook for alert", "alert", name)
			continue
		}
		namespace, target, surgeonID, resolveErr := rb.Resolve(item.Labels)
		inc := model.Incident{
			AlertName:   name,
			Fingerprint: model.Fingerprint(name, namespace, target, surgeonID),
			Status:      model.StatusPending,
			Summary:     item.Summary(),
			Labels:      model.CopyMap(item.Labels),
			StartsAt:    item.StartsAt,
		}
		op := model.Operation{
			SurgeonID:  surgeonID,
			Action:     rb.Action,
			TargetKind: rb.TargetKind,
			Namespace:  namespace,
			Name:       target,
			Command:    model.CopyStrings(rb.Command),
			Parameters: model.CopyMap(rb.Parameters),
			Labels:     model.CopyMap(item.Labels),
			Status:     model.StatusPending,
		}
		if resolveErr != nil {
			inc.Status = model.StatusFailed
			op.Status = model.StatusFailed
			op.Logs = resolveErr.Error()
		}
		savedInc, savedOp, skipped, err := st.CreateRemediation(ctx, inc, op, rb.Cooldown())
		if err != nil {
			return result, err
		}
		if skipped {
			result.Skipped++
			log.Info("remediation skipped by cooldown", "alert", name, "fingerprint", inc.Fingerprint, "incident_id", savedInc.ID)
			continue
		}
		if resolveErr != nil {
			result.Failed++
			log.Warn("remediation target unresolved", "alert", name, "incident_id", savedInc.ID, "error", resolveErr)
			continue
		}
		result.Remediated++
		log.Info("remediation queued", "alert", name, "action", rb.Action, "incident_id", savedInc.ID, "operation_id", savedOp.ID)
	}
	return result, nil
}

// Describe returns a one-line description of an operation for notifications.
func Describe(op model.Operation) string {
	target := op.Name
	if op.Namespace != "" {
		target = op.Namespace + "/" + op.Name
	}
	if op.Action == model.ActionScript {
		return fmt.Sprintf("script on %s", op.SurgeonID)
	}
	return fmt.Sprintf("%s %s %s", op.Action, op.TargetKind, target)
}
