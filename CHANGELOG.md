# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions follow [Semantic Versioning](https://semver.org/) once a first release is tagged.

## [Unreleased]

### Added
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
- `UPGRADING.md`: install, upgrade (git clone, read-only access, deploy key, ZIP), agent updates, rollback and troubleshooting.

### Fixed
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
