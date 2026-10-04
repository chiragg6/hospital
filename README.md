# Hospital

Hospital is a Kubernetes remediation service. Prometheus Alertmanager sends a webhook, Hospital matches the alert to a runbook, and either calls the Kubernetes API or hands a script to a node agent. When the fix finishes, Hospital can post the result to Slack.

Design: reception takes the alert, a runbook says what to do, and a surgeon carries it out. Targets are pods, deployments, statefulsets, and nodes instead of virtual machines.

## How it works

Two processes share one image.

| Process | Binary | Role |
| --- | --- | --- |
| Hospital | `cmd/hospital` | HTTP API, runbook matching |
| Surgeon | `cmd/surgeon` | Node agent that runs `script` runbooks |

```
Alertmanager
    |
    |  POST /v1/reception
    v
Hospital API
    |
    |  match runbook, write incident + operation
    v
Store (memory or Postgres)
    |                         \
    | API actions              \ script actions
    v                           v
Worker                          Surgeon on the node
Kubernetes API                  GET /v1/operation
    |                           exec (no shell)
    |                           POST /v1/report
    v                           /
Slack (optional) <------------
```

1. Alertmanager POSTs a firing alert to `/v1/reception`. Resolved alerts are ignored.
2. Hospital selects the runbook whose `alert_name` matches and whose `match_labels` are the most specific. A runbook with more matching labels wins over a broader one for the same alert name.
3. The runbook reads the target out of alert labels. Defaults are `namespace` plus `pod`, `deployment`, or `node`, depending on the action. Override them with `namespace_label`, `name_label`, and `surgeon_label`.
4. Hospital records an **incident** (the alert it considered) and an **operation** (the remediation step). The fingerprint is `alert|namespace|name|surgeon`. The same target is not touched again until the runbook cooldown expires. A missing cooldown is five minutes. `cooldown_seconds: 0` disables it.
5. A worker claims API actions and performs them: delete a pod, restart a deployment or statefulset, scale it, or cordon or uncordon a node.
6. Script actions stay queued for the surgeon whose id matches the alert (usually the node name). The surgeon long-polls `GET /v1/operation` and reports the result to `POST /v1/report`.
7. A terminal result is sent to Slack when `SLACK_WEBHOOK_URL` is set.

Operations left in `running` for longer than `STALE_AFTER` (default 15 minutes) are queued again. Runbooks should be safe to repeat.

### Cluster access

On startup, Hospital tries an in-cluster service account, then falls back to the local kubeconfig (`KUBECONFIG`, otherwise `~/.kube/config`). A missing cluster config does not stop the process: the API still accepts alerts, and API remediations fail later with `kubernetes client is not configured`. `DRY_RUN=true` records the action and does not call the Kubernetes API.

`kube-system`, `kube-public`, and `kube-node-lease` are refused unless `ALLOW_SYSTEM_NAMESPACES=true` or the namespace is listed in `ALLOWED_NAMESPACES`. Cordon, uncordon, and script actions are not namespace-scoped.

### Scripts

Runbook commands must be absolute paths and are executed without a shell. Alert labels are exposed to the script as environment variables, not interpolated into the command. In Kubernetes, those scripts come from a ConfigMap named `surgeon-scripts` mounted at `/opt/runbooks` and run inside the surgeon container. The surgeon service account has no cluster permissions, and the default DaemonSet does not grant host access.

### State

| `DATABASE_URL` | Store |
| --- | --- |
| unset | In-memory. Lost when the process exits. |
| `postgres://...` | Postgres. Schema is applied on startup. |

`DATABASE_URL` is a Postgres connection string, not an HTTP URL. `http://localhost:5432` is rejected by the driver.

## Layout

| Path | Purpose |
| --- | --- |
| `cmd/hospital` | API process |
| `cmd/surgeon` | Node agent |
| `internal/alert` | Alertmanager webhook types |
| `internal/api` | HTTP routes |
| `internal/model` | Runbook, incident, and operation types |
| `internal/remedy` | Reception, worker, namespace policy, Kubernetes executor |
| `internal/surgeon` | Long-poll client and script runner |
| `internal/store` | Memory and Postgres implementations |
| `internal/notify` | Slack |
| `examples/` | Sample runbooks and an Alertmanager receiver |
| `deploy/kubernetes/` | Namespace, RBAC, Deployment, Service, surgeon DaemonSet |

## Run

```bash
go test ./...
DRY_RUN=true RUNBOOKS_FILE=examples/runbooks.json go run ./cmd/hospital
```

With Postgres via Compose (user `hospital`, database `hospital`, dry-run on):

```bash
docker compose up --build
```

Against a Postgres you already run:

```bash
DATABASE_URL='postgres://hospital:hospital@localhost:5432/hospital?sslmode=disable' \
  RUNBOOKS_FILE=examples/runbooks.json \
  go run ./cmd/hospital
```

Run a surgeon against that API (any id is fine for local script tests; in a cluster this is the node name):

```bash
SURGEON_ID=minikube HOSPITAL_URL=http://localhost:8080 go run ./cmd/surgeon
```

## API

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/healthz`, `/readyz` | Liveness and readiness |
| POST | `/v1/reception` | Alertmanager webhook |
| GET | `/v1/runbooks` | List runbooks |
| POST | `/v1/runbooks` | Create or replace a runbook |
| DELETE | `/v1/runbooks/{id}` | Delete a runbook |
| GET | `/v1/incidents` | Recent incidents |
| GET | `/v1/operations` | Recent operations |
| GET | `/v1/operations/{id}` | One operation |
| GET | `/v1/operation?surgeon_id=` | Long-poll for a script operation |
| POST | `/v1/report` | Surgeon result |

If `HOSPITAL_TOKEN` is set, send `Authorization: Bearer <token>` on every route except the health checks.

A runbook looks like this:

```json
{
  "alert_name": "KubePodCrashLooping",
  "action": "delete-pod",
  "match_labels": {"namespace": "payments"},
  "cooldown_seconds": 300
}
```

Actions are `delete-pod`, `rollout-restart`, `scale`, `cordon`, `uncordon`, and `script`. `scale` needs `parameters.replicas`. `script` needs `command` as an absolute executable plus arguments. See `examples/runbooks.json` for one of each kind.

Example webhook:

```bash
curl -s -X POST localhost:8080/v1/reception \
  -H 'content-type: application/json' \
  -d '{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"KubePodCrashLooping","namespace":"payments","pod":"api-0"},"annotations":{"summary":"crash looping"},"startsAt":"2026-10-01T00:00:00Z"}]}'
```

## Kubernetes

Build the image, then install the manifests. The image contains both processes. The deployment runs `hospital`. The surgeon DaemonSet overrides the command with `/usr/local/bin/surgeon`.

```bash
docker build -t hospital:latest .
kubectl apply -k deploy/kubernetes
```

Copy `deploy/kubernetes/secret.example.yaml` if you want a token, Postgres, or Slack. Leave `DATABASE_URL` unset and Hospital keeps incidents in memory. An example Alertmanager receiver is in `examples/alertmanager.yaml`.

The Hospital service account can delete pods, update deployments and statefulsets, and cordon nodes. Narrow that with `ALLOWED_NAMESPACES`.

## Configuration

Hospital (`cmd/hospital`):

| Variable | Default | Meaning |
| --- | --- | --- |
| `HOSPITAL_ADDR` | `:8080` | Listen address |
| `DATABASE_URL` | empty | Postgres URL (`postgres://user:pass@host:5432/db`); empty uses memory |
| `RUNBOOKS_FILE` | empty | JSON file loaded at startup |
| `HOSPITAL_TOKEN` | empty | Bearer token for the API |
| `SLACK_WEBHOOK_URL` | empty | Slack incoming webhook |
| `DRY_RUN` | false | Do not call the Kubernetes API |
| `ALLOWED_NAMESPACES` | empty | Comma-separated namespace allow list |
| `ALLOW_SYSTEM_NAMESPACES` | false | Permit kube-system and related namespaces |
| `WORKER_INTERVAL` | `2s` | How often queued API actions are claimed |
| `POLL_TIMEOUT` | `25s` | Surgeon long-poll length |
| `POLL_INTERVAL` | `1s` | Delay between empty surgeon polls |
| `STALE_AFTER` | `15m` | When a stuck operation is requeued |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |

Surgeon (`cmd/surgeon`):

| Variable | Default | Meaning |
| --- | --- | --- |
| `SURGEON_ID` | required | Usually the node name |
| `HOSPITAL_URL` | required | Hospital base URL |
| `HOSPITAL_TOKEN` | empty | Bearer token, when the API requires one |
| `POLL_TIMEOUT` | `25s` | HTTP client timeout is this plus 10 seconds |
| `POLL_INTERVAL` | `2s` | Delay after a failed poll |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, or `error` |
