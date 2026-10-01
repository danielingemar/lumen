# Lumen (working name)

Open-source, OpenTelemetry-native observability: traces, logs and metrics in one binary, multi-tenant from day one, with a built-in web UI (dashboards you build yourself) and one-command agents for Linux, Windows, Docker and Nextcloud.

**Storage:** telemetry (traces, logs, metrics) goes to **ClickHouse**. Small, frequently changing records (users, API keys, dashboards) go to **Elasticsearch**, a NoSQL document store.

**Status: early MVP.** Ingest, search, charts, dashboards and agent enrolment work. There is no alerting or PromQL yet (see Roadmap).

## What the UI gives you

- **Dashboards you build:** add line, area, stacked-bar, single-value, table, log and trace-list panels; pick the metric (or trace/log statistic), aggregation, "split by" label, label filters, unit, size; drag panels to reorder; every change shows a live preview. Four starter dashboards in one click: **Hosts**, **Nextcloud**, **Services (traces)**, **Logs**.
- **Metrics explorer:** chart any metric in seconds, then "Add to dashboard".
- **Traces** (search + waterfall), **Logs** (search + volume chart), **Home** (what is reporting right now), **Add a machine** (copy-paste install commands).
- Light and dark theme, time range (15 min to 7 days) and auto-refresh in the top bar.
- **Sign in with a username and password, or with an API key** (a key session can read data and use dashboards, but cannot create keys or change passwords).

## 1. Try it on your own machine

Needs Docker with the compose plugin and about 8 GB RAM (Elasticsearch + ClickHouse). On a Linux host, Elasticsearch also needs `vm.max_map_count=262144` (`dev.sh` checks this and prints the one-line fix; Docker Desktop on Mac/Windows handles it itself).

```bash
sudo sysctl -w vm.max_map_count=262144                                   # Linux hosts only, needed by Elasticsearch
echo 'vm.max_map_count=262144' | sudo tee /etc/sysctl.d/99-lumen.conf    # keeps it after a reboot
./dev.sh                       # auto-detects your LAN IP
./dev.sh --ip 192.168.56.1     # or pick the address your VM can reach
```

It starts Lumen, ClickHouse and Elasticsearch, then prints the web UI address, the login (username `admin` + a generated password) and copy-paste commands to run on another VM. `./dev.sh status | stop | reset` manage it.

If the VM cannot connect: use an IP the VM can reach (bridged network = your LAN IP; NAT/host-only = the host address of that virtual network) and allow inbound TCP 4318 on this machine's firewall.

## 2. Install on a server

Rocky Linux / AlmaLinux / RHEL 9, or any host that already has Docker. 2+ vCPU, 6 GB RAM minimum (8 GB+ recommended), 20 GB+ local SSD. `install.sh` and `dev.sh` also set `vm.max_map_count=262144` themselves if you skip the two lines above, so they are safe to run either way.

```bash
sudo sysctl -w vm.max_map_count=262144
echo 'vm.max_map_count=262144' | sudo tee /etc/sysctl.d/99-lumen.conf
sudo ./install.sh --public-url https://lumen.example.com --tenant main --with-agent
```

Installs Docker if missing, generates secrets, runs a preflight check, starts everything, waits until healthy, and prints the UI address and the admin login. Add `--allow-podman-removal` if podman is installed (Docker's runc replaces it). Manual steps: `deploy/install-rhel-family.sh`, `deploy/gen-env.sh`, `deploy/preflight.sh`, `make up`.

Production notes:
- Use HTTPS: API keys travel in headers. Put Nginx/Caddy in front and set `LUMEN_BIND=127.0.0.1` in `deploy/.env`. Docker-published ports are normally reachable regardless of firewalld, so do not rely on firewalld alone.
- Set `--public-url` (`LUMEN_PUBLIC_URL`) to the address agents will use; it is written into the install scripts.
- If agents reach Lumen through a proxy such as Squid, exempt the Lumen host from it.

## 3. Add machines (the "Add a machine" page in the UI)

Log in to the UI, choose **Add a machine**, create an agent key (give it a name such as `web-server-1`), pick the machine type, copy the command. The key is shown once; the commands already contain it.

| Type | What you paste | What it does |
|---|---|---|
| **Linux server** | `curl -fsSL URL/install/agent.sh \| sudo sh -s -- --key KEY` (or the `wget -qO-` form) | Installs a systemd service running as an unprivileged `lumen-agent` user. Optional `--logs '/var/log/*.log'`, `--docker-logs`. |
| **Windows** | `& ([scriptblock]::Create((irm URL/install/agent.ps1))) -Key KEY` in an elevated PowerShell | Installs a scheduled task running as SYSTEM that restarts on failure. Optional `-LogPath`. |
| **Docker host** | a `docker run ... alpine:3.20 sh -c 'wget -qO- URL/install/agent-docker.sh \| sh'` command | Runs the agent as a container that downloads and checksum-verifies the agent at start. Nothing to build or pull from a registry. |
| **Nextcloud** | Linux or Docker command with the instance URL and token | Monitors one Nextcloud instance remotely (see below). |
| **Download files** | links, `wget` and PowerShell `Invoke-WebRequest` commands | Raw binaries + SHA-256 for manual installs. |

Remove an agent with `--uninstall` (Linux script), `-Uninstall` (Windows) or `docker rm -f lumen-agent`.

Agents report their own health too (`service.name=lumen-agent`): uptime, items sent, send errors, scrape errors. Set `"metrics_listen": "127.0.0.1:9464"` to also expose it at `/metrics`. An agent that cannot reach Lumen cannot report that, so alert on missing `lumen_agent_uptime_seconds`.

### Nextcloud monitoring

One agent can watch one or more Nextcloud instances from anywhere that can reach their URL. It collects, per instance:
- availability and response time (`status.php`), maintenance mode, pending DB upgrade, version;
- with a token: active users (5m/1h/24h), user/file/storage counts, shares by type, installed apps and available updates, database size, PHP memory limit, OPcache hit rate/usage, free data space;
- optionally, `nextcloud.log` shipped as structured logs (level, app, user and request id are kept; the request URL is dropped because it can contain tokens).

Setup on the Nextcloud side (serverinfo is a default app):

```bash
docker exec -u www-data <nextcloud-container> php occ config:app:set serverinfo token --value 'A_LONG_RANDOM_STRING'
# or, without Docker:  sudo -u www-data php occ config:app:set serverinfo token --value '...'
```

Then use the Nextcloud tab under "Add a machine". The Nextcloud hostname must be in that instance's `trusted_domains`. To use an admin user instead of a token: `--nextcloud-user USER --nextcloud-password APP_PASSWORD`. Multiple instances: use the `nextcloud` list in the agent config file (`lumen-agent.example.json`), one entry each.

Note: the serverinfo field names were written from the app's documented response and covered by tests against a sample of that shape, not against a live server. Check the numbers against one real instance first; missing fields are skipped rather than failing.

## Users, login and keys

- **People log in with a username and password** (web UI). Passwords are stored as salted PBKDF2-HMAC-SHA256 hashes (600,000 iterations), sessions are signed HttpOnly `SameSite=Strict` cookies (12 h; Secure over HTTPS), logins are throttled per username (10 failures / 10 min), and changing or resetting a password ends that user's existing sessions.
- **Machines and agents use API keys**, not passwords. A browser session (either kind) can never send telemetry. **You can also sign in to the UI with an API key**: that session is read-only apart from dashboards, and it ends the moment the key is deleted. Only a username login can create keys or change passwords. Keys are stored only as SHA-256 hashes; the full key is shown once at creation. Create and delete them in the UI (**Add a machine**) or with the CLI.
- **Storage:** users, key hashes, dashboards and the session secret live in **Elasticsearch** (indices `lumen-users`, `lumen-keys`, `lumen-dashboards`, `lumen-meta`). Every operation touches a single document and uses optimistic concurrency (two people saving the same dashboard cannot silently overwrite each other); note that Elasticsearch has no multi-document transactions. Without `LUMEN_ELASTICSEARCH_URL` Lumen falls back to a JSON file (`documents.json`, 0600, in `LUMEN_DATA_DIR`), which is fine for a single small instance. An `auth.json` from an earlier version is imported automatically. Back up the `esdata` volume (or use Elasticsearch snapshots) together with ClickHouse.
- **Licensing note:** Elasticsearch is not Apache-licensed (Elastic License 2.0 / SSPL, plus AGPL from 8.16). Running it as a separate service next to Lumen is fine, but read the terms before you redistribute it or offer it as a hosted service. Lumen only uses the basic document and search APIs, so **OpenSearch** (Apache-2.0) should work as a drop-in by pointing `LUMEN_ELASTICSEARCH_URL` at it. That has not been tested.
- The first admin is created at startup from `LUMEN_ADMIN_USER` / `LUMEN_ADMIN_PASSWORD` / `LUMEN_ADMIN_TENANT` (the setup scripts generate these into `deploy/.env`). Changing the password in `.env` afterwards has no effect; use the UI or the CLI.
- Every user belongs to one tenant and only sees that tenant's data. Access is controlled by groups, see "Users, groups and permissions" below.

Manage users from the command line (also works while the server is running):

```bash
docker compose -f deploy/docker-compose.yml exec lumen /lumen users add alice --tenant main   # prints a generated password once
docker compose -f deploy/docker-compose.yml exec lumen /lumen users list
docker compose -f deploy/docker-compose.yml exec lumen /lumen users passwd alice
docker compose -f deploy/docker-compose.yml exec lumen /lumen users delete alice
docker compose -f deploy/docker-compose.yml exec lumen /lumen keys create --tenant main --name vm1
```

Locked out? Reset the admin from the host: `... exec lumen /lumen users passwd admin`.

## Configuration

Server (env): `LUMEN_ADDR` (:4318), `LUMEN_PUBLIC_URL`, `LUMEN_CLICKHOUSE_URL`, `LUMEN_CLICKHOUSE_DB` (lumen), `LUMEN_CLICKHOUSE_USER`, `LUMEN_CLICKHOUSE_PASSWORD`, `LUMEN_RETENTION_DAYS` (30), `LUMEN_BACKUP_DIR` (/backup, empty = no backups), `LUMEN_BACKUP_RETENTION_DAYS` (365, 0 = forever), `LUMEN_SECRET_KEY` (encrypts stored Nextcloud credentials; generated by the install scripts, keep deploy/.env safe), `LUMEN_API_KEYS` (`key:tenant,key:tenant`), `LUMEN_DIST_DIR` (/dist). `LUMEN_DATA_DIR` (/data, only for the JSON fallback), `LUMEN_ELASTICSEARCH_URL`, `LUMEN_ELASTICSEARCH_USER`, `LUMEN_ELASTICSEARCH_PASSWORD` (or `LUMEN_ELASTICSEARCH_API_KEY`), `LUMEN_ELASTICSEARCH_PREFIX` (lumen), `LUMEN_ADMIN_USER`, `LUMEN_ADMIN_PASSWORD`, `LUMEN_ADMIN_TENANT` (main). `LUMEN_API_KEYS` are optional bootstrap keys. If there are no users and no keys, nobody can log in and the server says so in its log. `LUMEN_DEV_MODE=true` disables authentication entirely (single tenant `default`): only for a private test machine.

Agent: a JSON file (`-config`, see `lumen-agent.example.json`) and/or env vars, env wins: `LUMEN_AGENT_URL`, `LUMEN_AGENT_API_KEY`, `LUMEN_AGENT_INTERVAL`, `LUMEN_AGENT_HOST_METRICS`, `LUMEN_AGENT_LOG_PATHS`, `LUMEN_AGENT_DOCKER_LOGS`, `LUMEN_AGENT_METRICS_LISTEN`, `LUMEN_AGENT_NEXTCLOUD_{URL,TOKEN,USER,PASSWORD,SERVICE,LOG}`. The agent also scrapes any Prometheus `/metrics` endpoint (`scrape` list), so node_exporter, cAdvisor and app exporters work unchanged.

Multi-tenancy: the tenant comes only from the authenticated API key, never from the request; every row is written with it and every query filters on it.

## API

| Method | Path | Notes |
|---|---|---|
| POST | `/v1/traces`, `/v1/logs`, `/v1/metrics` | OTLP/HTTP **JSON** only for now, gzip supported |
| POST | `/api/v1/login`, `/api/v1/logout`, `/api/v1/password` | JSON body; browser login |
| GET/POST/DELETE | `/api/v1/keys`, `/api/v1/keys/{id}` | agent keys, browser login required |
| POST | `/api/v1/login` | `{"username","password"}` or `{"api_key"}` |
| GET | `/api/v1/me` | who am I (`via_key` is true for API-key sessions) |
| GET | `/api/v1/series` | chart data. `source=metric\|traces\|logs`; metric: `name`, `agg=avg\|sum\|min\|max\|last\|rate`, `filter=label:value` (up to 5), `group_by=host`; traces: `metric=requests\|rps\|errors\|error_rate\|avg\|p50\|p95\|p99`, `group_by=service\|name`; logs: `group_by=severity\|service`, `severity`, `q`; plus `service`, `from`, `to`, `step` (auto by default, max 90 days) |
| GET | `/api/v1/services`, `/api/v1/metrics/names`, `/api/v1/metrics/labels?name=` | what is reporting; used by the editor dropdowns |
| GET/POST/PUT/DELETE | `/api/v1/dashboards`, `/api/v1/dashboards/{id}` | dashboards (`name`, `description`, `body.panels`, `version` for conflict detection); an API key may use these too, for scripting |
| GET | `/api/v1/traces` | `service`, `min_duration_ms`, `errors=true`, `from`, `to` (RFC3339), `limit` |
| GET | `/api/v1/traces/{id}` | all spans of a trace |
| GET | `/api/v1/logs` | `service`, `severity`, `q`, `trace_id`, `from`, `to`, `limit` |
| GET | `/install/agent.sh`, `/install/agent.ps1`, `/install/agent-docker.sh`, `/download/{file}` | agent enrolment (no auth; contain no secrets) |

OpenTelemetry SDKs and the Collector can send directly: set the exporter encoding to `json`, endpoint `http://lumen:4318`, header `Authorization: Bearer KEY`.

## Source layout

```
cmd/lumen           server        cmd/lumen-agent    agent
internal/server     HTTP API, UI and installer routes
internal/store      ClickHouse (HTTP interface, no driver dependency)
internal/otlp       OTLP JSON decoding
internal/agent      host metrics, Prometheus scrape, log tailing, Nextcloud, self-metrics, sender
internal/install    embedded install scripts + agent download handler
internal/ui         single-file web UI
internal/auth       users, passwords, sessions, API keys (on top of docstore)
internal/docstore   document store: Elasticsearch client + JSON-file fallback (estest = fake ES for tests only)
internal/dashboards dashboards per tenant (stored through docstore)
internal/edition    open-core hooks (auth, authorization)
ee/                 enterprise features (commercial license, build tag "enterprise")
deploy/             compose file, preflight, secrets generator, RHEL-family Docker installer
install.sh dev.sh   server installer / local test setup
```

Build and test: `go vet ./... && go test ./...`, `make build`, `make build-ee`. The SQL is also checked against a real ClickHouse engine: `pip install chdb`, then `LUMEN_DUMP_SQL=/tmp/sql.json go test ./internal/store -run Dump && python3 scripts/test-sql.py /tmp/sql.json`. Only the Go standard library is used, so there are no dependencies to download. The Elasticsearch client is tested against a small fake (`internal/docstore/estest`), not a real Elasticsearch.

## Before you publish the source

1. `./scripts/set-module.sh github.com/YOUR_USER/lumen` replaces the placeholder module path everywhere and checks the build.
2. The core is Apache-2.0 (`LICENSE`). `ee/LICENSE` is a placeholder: replace it with your commercial terms, or delete `ee/` and `cmd/lumen/enterprise.go` if you do not want an enterprise tier yet.
3. Pick the final name and check it for trademark conflicts, then search-and-replace "Lumen" and `lumen`.
4. `.gitignore` already excludes `deploy/.env` (secrets). Check `git status` shows no `.env` before the first commit.

## Roadmap

1. Alerting (rules on any chart query, notifications), then dashboard variables and sharing
2. Protobuf and gRPC OTLP ingest
3. Alerting, service map, span details, then automatic correlation across traces/logs/deploys
4. Kubernetes discovery and a Helm chart for the agent; native Windows service; Windows Event Log
5. Server self-monitoring, per-tenant quotas, agent on-disk buffering
6. Enterprise: OIDC/SSO, audit log (basic RBAC is already in the core)


## Users, groups and permissions

Three kinds of group, managed under **Users & groups** in the UI:

- **Admin**: everything, including users, groups, keys and backups.
- **User**: read-only access to data, dashboards, hosts and instances.
- **Custom**: an admin picks none / read / write per area: dashboards, traces, logs, metrics, hosts & instances, agent keys, users & groups, backups & archive. Traces, logs and metrics are read-only by nature.

The server enforces this on every request; the UI only hides what you cannot use. Accounts created before groups existed count as admins. API keys (agents, scripts) can read data and manage dashboards, never users or keys. Safeguards: you cannot delete yourself, change your own group, or remove the last user who can manage users. CLI: `lumen users add NAME --tenant T --group admin|user|GROUP_ID` (default `user`), `lumen users set-group NAME GROUP`.

## Hosts, instances and remote agent configuration

- **Hosts** lists every machine with an agent: status, agent version, OS, services and containers up/down. Open one to set log files to ship, Docker log shipping, which services must run (shown DOWN when stopped), and to toggle service/container reporting.
- **Instances** manages the Nextcloud installations to monitor (URL, token or login, log file, and which machine's agent checks it). Fix a typo and save: no change on the machine.
- Agents fetch their settings from `GET /api/v1/agent/config?host=NAME` every 60 s (API key only) and restart their collectors when they change. Tokens are stored encrypted (`LUMEN_SECRET_KEY`) but are sent to that agent, so serve Lumen over https.
- Server-supplied log paths are checked by the agent: only under /var/log, /var/lib/docker/containers, /var/lib/docker/volumes, /var/www, /srv, /mnt, /opt, never with `..`. Override with `allowed_log_dirs` in the agent config (`["*"]` allows all). Paths from the local config file are always trusted. Refused paths show as a warning on the host.
- **Services**: the agent reports systemd units (running or failed, plus watched ones) as `system_service_up`. **Containers**: `container_up`, via the Docker socket; install with `--docker`, which adds the agent to the `docker` group (root-equivalent). Windows services are not collected yet.
- Machines installed with an older agent must run the install command once more.

## Status and "Down"

Status panels (data source "Status" on a single-value panel; also on Home) show up/down counts for hosts, services, containers and Nextcloud instances, and show a green 0 when nothing is down. A host is down when it stopped reporting for 2 minutes; services and containers only count on hosts that report. A Nextcloud instance is down when the agent cannot reach it or the agent stopped reporting; a newly added one gets 5 minutes to deliver its first check.

## Retention, backup and archive

Live data is kept `LUMEN_RETENTION_DAYS` (30) in ClickHouse; changing it also updates existing tables at startup. Every completed UTC day is exported once, per tenant and table, as gzip JSON lines:

    /backup/telemetry/YYYY-MM-DD/<tenant>/otel_{spans,logs,metrics}.jsonl.gz   (+ DONE marker)
    /backup/config/lumen-config-YYYY-MM-DD.json.gz   users, groups, dashboards, hosts, instances (all tenants; operator only, not served by the API)

Backups are kept `LUMEN_BACKUP_RETENTION_DAYS` (365). To look at an old day: **Backups & archive** > View in archive (loads it into `*_archive` tables without expiry), then use the Archive switch in the top bar; pick a custom time range. Unload when done. Files can also be downloaded or read with `zcat`.

The backup lives on the same server as the data. Copy it elsewhere regularly:

    docker compose -f deploy/docker-compose.yml cp lumen:/backup ./lumen-backup

## API additions

| Endpoint | Permission |
|---|---|
| `GET/POST /api/v1/users`, `PUT/DELETE /api/v1/users/{name}` | users |
| `GET/POST /api/v1/groups`, `PUT/DELETE /api/v1/groups/{id}` | users |
| `GET /api/v1/status` | hosts or metrics |
| `GET /api/v1/hosts`, `GET/PUT/DELETE /api/v1/hosts/{host}` | hosts |
| `GET/POST /api/v1/instances`, `PUT/DELETE /api/v1/instances/{id}` | hosts |
| `GET /api/v1/agent/config?host=` | API key |
| `GET /api/v1/backups`, `POST /backups/run`, `POST/DELETE /backups/{day}/load`, `GET /backups/{day}/{spans\|logs\|metrics}` | backups |
| any read with `?archive=1` | backups (read) |
