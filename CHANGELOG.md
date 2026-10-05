# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions follow [Semantic Versioning](https://semver.org/) once a first release is tagged.

## [Unreleased]

### Added
- **A page per Nextcloud instance** (click it on Instances): status and reason, Nextcloud/PHP/database versions, usage (users, files, shares, storage, database, free space, active users), performance and availability charts and the instance's own log. It also tells when only availability is monitored (no serverinfo token) or when the token is wrong.
- **Users and groups** with Admin, User (read-only) and custom groups (none / read / write per area), enforced by the server.
- **Hosts** and **Instances** pages: see every machine and Nextcloud instance, set log paths, watched services and fix a mistyped
  Nextcloud URL in the browser. Agents fetch their configuration from the server every minute.
- **Services and containers**: the agent reports systemd units and Docker containers (`--docker` on install).
- **Up / Down** status panels for Nextcloud instances, hosts, services and containers; a Down box shows a green 0 when all is well.
- **30 days of retention** with a **daily backup** of every completed day (gzip JSON lines per tenant and table) and an
  **archive**: load an old day and browse it with the Archive switch and a custom time range.
- Labels box in the Metrics explorer; custom time range in the top bar.
- Nextcloud credentials are stored encrypted (`LUMEN_SECRET_KEY`).
- Agent allow-list for log paths pushed from the server (`allowed_log_dirs`).
- CI (build, vet, race tests, cross-builds, SQL against a ClickHouse engine), release workflow, issue templates.

- **Dropdowns** of everything that has come in on Traces (service, operation, host), Logs (source, host) and Metrics (every metric, with a filter box); new `GET /api/v1/facets`, and `host`/`operation` filters on traces, logs and the series API.
- **Hosts:** display names (rename) and manual removal of hosts; removal is refused while Nextcloud instances are still checked by the host.
- **Settings page** with a site name and logo upload (new `settings` permission); shown in the menu, the browser tab and on the login page. Uploads are verified by content and SVG scripts are refused.
- **Host page shows everything about the machine:** CPU (by state), load, memory by state, swap, every local disk's usage and inodes, disk I/O (throughput, IOPS, busy time), network traffic and errors, processes and uptime, as charts. The Linux agent collects the new metrics; existing metric names are unchanged.
- Chart axes make room for long labels such as `57.2 MB/s`.
- **Tab icon:** the uploaded logo is used as the browser tab icon (a built-in icon otherwise).
- **Host IP addresses:** agents report the address they use to reach Lumen and their other addresses; shown on the Hosts list and host page.
- Nextcloud instance URLs ending in `/login` or `/index.php` are refused, and the agent explains a 404 from `status.php`.

### Docs
- Draft documents, in English and Swedish: the **edition charter** (what is Community, Enterprise and Operator, and the promises about it), a **tenancy design** (operator layer, tenant records, quotas, usage metering, per-tenant retention and branding, dedicated storage) and an **alerting design** (rules, state machine, notifications, Jira/email/PagerDuty, on-call, security). No code yet.
- `UPGRADING.md`: install, upgrade (git clone, read-only access, deploy key, ZIP), agent updates, rollback and troubleshooting.

- **Version stamp:** the Docker build identifies the source code with a short hash (`src-3fa91c2d`, or `--build-arg VERSION=…`). It is shown at the bottom of the menu, logged at start-up and reported by every agent; the Hosts page marks agents whose version differs from the server's with **update**. The agent's version used to be `dev` for every build.
- The web page is served with an `ETag` and `Cache-Control: no-cache`, so an upgrade shows at once without a hard reload.

### Fixed
- **Charts no longer cut off the top of a line.** When the highest value was slightly above a round number (for example 211 on an axis ending at 200, or 2100, 0.43, 105) the axis stopped below the data and the line was drawn outside the chart, so it looked empty. The top tick is now never below the data. Roughly one chart in four was affected.
- The content-security policy of the UI now allows `data:` images (`img-src 'self' data:`). Without it a browser blocked the logo preview on the Settings page.
- `install.sh` and `dev.sh` no longer overwrite `LUMEN_PUBLIC_URL` (and `dev.sh` no longer `LUMEN_BIND`) when re-run to upgrade: an address already in `deploy/.env` is kept unless `--public-url` / `--ip` is given. Covered by `scripts/test-installers.sh` in CI.

### Changed
- Default retention is 30 days (was 14).
- Accounts created before groups existed are treated as admins. `lumen users add` defaults to the read-only `user` group.
- `LUMEN_SECRET_KEY` is required by the compose file; the install scripts generate it.

### Known limitations
- No alerting yet. No Windows services collector (`agent.ps1` does not report services).
- Elasticsearch, Docker, systemd, Windows and Nextcloud integrations are covered by unit and fake-service tests, not yet by
  long-running real deployments.

## [0.0.1] - initial import
- OTLP/HTTP JSON ingest into ClickHouse, query API, single-file web UI, dashboards, agents for Linux, Windows and Docker,
  Nextcloud monitoring, API-key and password login, Elasticsearch document store.
