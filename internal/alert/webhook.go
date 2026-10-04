package alert

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Message is the Alertmanager webhook body
type Message struct {
	Version string  `json:"version"`
	Status  string  `json:"status"`
	Alerts  []Alert `json:"alerts"`
}

// Alert is one Prometheus alert inside a webhook.
type Alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	Fingerprint  string            `json:"fingerprint"`
	GeneratorURL string            `json:"generatorURL"`
}

// Parse reads an Alertmanager webhook and ignores fields this service does not use.
func Parse(r io.Reader) (Message, error) {
	dec := json.NewDecoder(io.LimitReader(r, 1<<20))
	var msg Message
	if err := dec.Decode(&msg); err != nil {
		return Message{}, fmt.Errorf("decode alertmanager webhook: %w", err)
	}
	if msg.Alerts == nil {
		return Message{}, fmt.Errorf("decode alertmanager webhook: missing alerts")
	}
	return msg, nil
}

func (a Alert) Name() string {
	return a.Labels["alertname"]
}

func (a Alert) Firing() bool {
	return a.Status == "firing"
}

func (a Alert) Summary() string {
	if s := a.Annotations["summary"]; s != "" {
		return s
	}
	if s := a.Annotations["description"]; s != "" {
		return s
	}
	return a.Name()
}
