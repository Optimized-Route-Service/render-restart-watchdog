# render-watchdog

One-shot CLI that checks CPU and memory for a list of Render.com services via
the Render API and restarts any service whose usage is over the configured
threshold. Then it exits.

It's designed to be run on a cron (system cron, Render Cron Job, Kubernetes
`CronJob`, Fly scheduled machine — whatever you already run). **Your cron
cadence is the effective cooldown between restarts**: a schedule of
`*/15 * * * *` means a given service can't be restarted more than once every
15 minutes no matter how hot it gets.

Generic: point it at any Render service. No assumptions about what the
service is, no health-endpoint probing, no long-running daemon, no HTTP
server. Just metrics → threshold → restart.

## Configuration

| Env                     | Required | Default | Description                                                          |
| ----------------------- | -------- | ------- | -------------------------------------------------------------------- |
| `RENDER_API_TOKEN`      | yes      | —       | Render API token. Account-scoped — use a dedicated monitoring user. |
| `RENDER_SERVICE_IDS`    | yes      | —       | Comma-separated Render service ids, e.g. `srv-abc,srv-def`.          |
| `MEM_THRESHOLD_PERCENT` | no       | `90`    | Restart if Render-reported memory ≥ this percent.                    |
| `CPU_THRESHOLD_PERCENT` | no       | `95`    | Restart if Render-reported CPU ≥ this percent.                       |
| `DRY_RUN`               | no       | unset   | If `1` / `true` / `yes` / `on`, log what would happen and skip the restart call. |
| `WEBHOOK_URL`           | no       | unset   | If set, POST a JSON payload (see below) for every non-OK service event — works with Zapier, Make, Slack-compatible webhooks, or any HTTPS endpoint. |

## Exit codes

| Code | Meaning                                                                  |
| ---- | ------------------------------------------------------------------------ |
| `0`  | All services evaluated cleanly. A triggered restart is still exit 0.     |
| `1`  | Configuration error (missing env, empty service list).                   |
| `2`  | One or more services failed to evaluate (metrics fetch or restart error). Wire this to your cron's alerting. |

## Logs

Structured JSON on stderr (`log/slog`). One `metrics` line per service every
run, plus `warn` / `error` lines on threshold breaches, dry-run, restart
calls, and failures. No secrets are logged.

## Running it

### Local

```
RENDER_API_TOKEN=rnd_... RENDER_SERVICE_IDS=srv-abc,srv-def ./render-watchdog
```

First run against a new service: add `DRY_RUN=1` to verify the observed
`memory_pct` and `cpu_pct` look sane (0–100 scale) before enabling real
restarts.

```
RENDER_API_TOKEN=rnd_... RENDER_SERVICE_IDS=srv-abc DRY_RUN=1 ./render-watchdog
```

### System cron

```
*/15 * * * * RENDER_API_TOKEN=rnd_... RENDER_SERVICE_IDS=srv-abc,srv-def \
  MEM_THRESHOLD_PERCENT=90 CPU_THRESHOLD_PERCENT=95 \
  /usr/local/bin/render-watchdog 2>> /var/log/render-watchdog.log
```

### Render Cron Job

Build and push the container (`make build-docker` + your registry), create a
Render Cron Job pointing at the image, set the env vars as Render secrets,
and schedule `*/15 * * * *`.

### Kubernetes CronJob

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: render-watchdog
spec:
  schedule: "*/15 * * * *"
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: OnFailure
          containers:
            - name: render-watchdog
              image: your-registry/render-watchdog:latest
              env:
                - name: RENDER_API_TOKEN
                  valueFrom:
                    secretKeyRef: { name: render-watchdog, key: token }
                - name: RENDER_SERVICE_IDS
                  value: "srv-abc,srv-def"
                - name: MEM_THRESHOLD_PERCENT
                  value: "90"
                - name: CPU_THRESHOLD_PERCENT
                  value: "95"
```

## Webhook notifications

Set `WEBHOOK_URL` to a Zapier / Make / generic webhook endpoint. For every
service whose run ends in anything other than `OK`, a POST is fired:

```json
{
  "event": "restarted",
  "service_id": "srv-abc123",
  "timestamp": "2026-04-22T11:03:15Z",
  "reason": "memory 95.1% >= 90.0%",
  "dry_run": false,
  "message": "Render service srv-abc123 restarted — memory 95.1% >= 90.0%",
  "metrics": {
    "memory_used_bytes": 32680000000,
    "memory_limit_bytes": 34359740000,
    "memory_percent": 95.1,
    "memory_human": "30.4 GiB / 32.0 GiB",
    "cpu_used": 6.2,
    "cpu_limit": 8,
    "cpu_percent": 77.5
  },
  "thresholds": {
    "memory_percent": 90,
    "cpu_percent": 95
  }
}
```

Event values:

| `event`           | When                                                          |
| ----------------- | ------------------------------------------------------------- |
| `restarted`       | A real Render restart was triggered.                          |
| `would_restart`   | Threshold breached, but `DRY_RUN` was on — no restart fired.  |
| `restart_failed`  | Restart API call returned an error.                           |
| `metrics_failed`  | Could not fetch metrics for this service.                     |

OK services are not notified — the webhook is for attention-worthy events only.

**Notes**

- One POST per non-OK service per run. Multiple services trigger multiple POSTs in parallel.
- Timeout is 10 s; failures are logged but don't fail the run (the restart already happened or didn't).
- The webhook URL itself is never logged, since Zapier / Make URLs contain path tokens.
- No retries — next cron tick will re-evaluate and notify again if still over threshold.

### Testing the webhook

Use [webhook.site](https://webhook.site) to grab an inspectable URL, then force
a `would_restart` event with a low threshold:

```
docker run --rm \
  -e RENDER_API_TOKEN=rnd_... \
  -e RENDER_SERVICE_IDS=srv-abc \
  -e MEM_THRESHOLD_PERCENT=1 \
  -e DRY_RUN=1 \
  -e WEBHOOK_URL=https://webhook.site/your-unique-id \
  xrouten-render-watchdog:local
```

## Dev

```
make fmt vet test
make build            # -> bin/render-watchdog
make build-docker     # -> xrouten-render-watchdog:local
```

## Security notes

- Zero external Go dependencies; only the standard library.
- Distroless / nonroot container image, static binary (`CGO_ENABLED=0`).
- The Render API token is read from env, sent only as `Authorization:
  Bearer` over HTTPS to `api.render.com`, and is not logged.
- Render's API response body is read under `io.LimitReader` (1 MiB cap).
- Recommended: create a dedicated Render user with access only to the
  services you want restartable, and mint the token for that user. Render
  tokens are account-scoped today, so a leaked token is as powerful as that
  user.
