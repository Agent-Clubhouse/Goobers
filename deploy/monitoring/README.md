# Monitoring: Prometheus + Grafana + Home Assistant

Surface a Goobers daemon's health and workforce activity in Prometheus, a
ready-made Grafana dashboard, and Home Assistant (sensors + phone alerts).

The daemon does not expose a `/metrics` endpoint. Instead, a small **exporter**
scrapes the daemon's read API (`/health`, `/runs`, `/gaggles`) plus — optionally
— the project's backlog, and re-publishes them as Prometheus gauges. Everything
downstream is standard Prometheus.

```
 goobers daemon                       ┌─▶ Grafana        (grafana-dashboard.json)
 read API :8085 ──▶ exporter :9779 ──▶ Prometheus ──┤
 + backlog (opt)     (this dir)                      └─▶ Home Assistant (homeassistant.yaml)
```

It is **read-only telemetry** — nothing here can change the workforce.

## Metrics

| Metric | Meaning |
|---|---|
| `goobers_up` | daemon read API reachable |
| `goobers_healthy` / `goobers_ready` | daemon health / ready booleans |
| `goobers_scheduler_last_tick_age_seconds` | dead-man switch — grows if the scheduler stalls |
| `goobers_journal_age_seconds` | staleness of the run journal |
| `goobers_degraded_subsystems` | count of degraded read-model subsystems |
| `goobers_runs_active` | active (non-terminal) runs |
| `goobers_runs_phase{phase}` | runs by phase (running/completed/failed/escalated/aborted) |
| `goobers_workflow_active_runs{workflow}` | active runs per workflow (lane) |
| `goobers_gaggles_total` | configured gaggles |
| `goobers_backlog_issues{status}` | open issues by status (approved/claimed/in_review/needs_human) |
| `goobers_open_prs` | open PRs on the project repo |

The backlog metrics (`goobers_backlog_issues`, `goobers_open_prs`) require the
optional backlog vars below; without them the exporter runs daemon-only.

## Setup

### 1. Run the exporter

Build and run it next to the daemon. `--network host` lets it reach the daemon's
loopback read API:

```sh
docker build -t goobers-exporter deploy/monitoring
docker run -d --name goobers-exporter --network host \
  -e GOOBERS_API=http://127.0.0.1:8085/api/v1 \
  goobers-exporter
# metrics now on http://127.0.0.1:9779/metrics
```

To include the backlog funnel + open-PR count, add your provider's API (the
example is Gitea; the same shape works for a GitHub API base + `owner/repo`):

```sh
  -e BACKLOG_API=http://your-forge:3000/api/v1 \
  -e BACKLOG_REPO=owner/repo \
  -e BACKLOG_TOKEN=<read-only token for issues/PRs> \
```

### 2. Scrape it from Prometheus

```yaml
# prometheus.yml
scrape_configs:
  - job_name: goobers
    static_configs:
      - targets: ["EXPORTER_HOST:9779"]
```

### 3. Import the Grafana dashboard

`grafana-dashboard.json` is a self-contained board: fleet health, the dead-man
gauge, active runs, the needs-human count, open PRs, runs-by-phase, per-lane
activity, and the backlog funnel.

**Dashboards → New → Import → Upload JSON file**, then choose your Prometheus
datasource when prompted (the board references it as the `${DS_PROMETHEUS}`
variable, so nothing is hard-coded).

### 4. Home Assistant

`homeassistant.yaml` adds REST sensors that query the Prometheus HTTP API, a
`binary_sensor` dead-man switch, and three notify automations (scheduler
stalled, needs-human, fleet degraded).

1. Drop it in as a package — in `configuration.yaml`:
   ```yaml
   homeassistant:
     packages: !include_dir_named packages
   ```
   then save this file as `packages/goobers.yaml`. (Or paste its `rest:` /
   `template:` / `automation:` blocks into `configuration.yaml` directly.)
2. **Replace the placeholders** before reloading:
   - `PROMETHEUS_HOST:9090` → your Prometheus address.
   - `notify.YOUR_NOTIFY_TARGET` → your notify service (e.g. a mobile app).
   - the `your-implementation-lane` / `your-second-lane` / `your-nightly-workflow`
     sensor labels → your actual workflow names (or delete the ones you don't run).
3. Reload without a restart: **Developer Tools → YAML → All YAML configuration**
   (or `homeassistant.reload_all`).

`lovelace-dashboard.yaml` is a matching one-view dashboard (fleet status badge,
dead-man gauge, active-work stats, the backlog funnel, a throughput history
graph, and PRs). Add it via a new dashboard in **raw configuration editor** mode.

## Configuration reference (exporter)

| Var | Default | Purpose |
|---|---|---|
| `GOOBERS_API` | `http://127.0.0.1:8085/api/v1` | Daemon read API base |
| `BACKLOG_API` | *(unset)* | Provider API base — enables backlog metrics with the two below |
| `BACKLOG_REPO` | *(unset)* | `owner/name` of the project repo |
| `BACKLOG_TOKEN` | *(unset)* | Read token for issues/PRs |
| `PORT` | `9779` | Port the `/metrics` endpoint listens on |
| `INTERVAL` | `30` | Seconds between daemon scrapes |

## Files

| File | What |
|---|---|
| `exporter.py` | the exporter (Python, `prometheus_client` + `requests`) |
| `Dockerfile` | builds the exporter image |
| `grafana-dashboard.json` | importable Grafana dashboard |
| `homeassistant.yaml` | HA REST sensors + dead-man binary sensor + notify automations |
| `lovelace-dashboard.yaml` | HA Lovelace dashboard view |
