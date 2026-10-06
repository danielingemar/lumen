# Changelog

All notable changes are listed here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions follow [Semantic Versioning](https://semver.org/) once a first release is tagged.

## [Unreleased]

### Fixed
- **"The settings store is unavailable" when using a feature added in a newer version** (for example clicking *update* on an agent, or removing an instance that is gone). The Elasticsearch store had a fixed list of collections, and the ones added recently (`agent_updates`, `instances_gone`, and the tenant console's `tenants`, `usage`, `audit`, `support_grants`) were missing from it. They now have mappings; a collection that has no index yet gets one on its first write, also when Elasticsearch is set not to create indices itself; a search in a collection nobody has written to is empty instead of an error; the list limit is 10 000 (Elasticsearch's default window) instead of 1 000; and a test fails if the code declares a collection that has no mapping. The error message now says where to find the reason.

### Added
- **Deeper Nextcloud monitoring (Community).** Per instance: how long ago Nextcloud's **cron** last ran (the value Nextcloud's own admin overview checks), cron mode and cron errors; whether a **Nextcloud update** is available and which version; days until the **TLS certificate** expires and whether it is trusted and for this name (also read while the instance is down); and a **WebDAV login check** (log in and list files) every five minutes. Background jobs and the login check need an administrator user with an app password; a serverinfo token cannot read them, and the instance page says so. New metrics `nextcloud_cron_*`, `nextcloud_update_available`, `nextcloud_tls_cert_*`, `nextcloud_webdav_*`; warnings, panels and charts on the instance page; five new alert templates (background jobs not running, update available, certificate expires soon, certificate not valid, login check failing). Slow checks are cached (cron once a minute, login every five, certificate every ten).
- **One-click agent updates.** The *update* label on the Hosts page (and on a host's page) is now a button, and *Update N agents* updates all outdated agents at once. A Linux agent installed with the script asks a small root-owned helper (a systemd path unit) to install what the server offers, after checking the server's checksums, with rollback if the new version does not stay up; a container agent downloads, checks and replaces its own process in place. Agents from before this, and Windows agents, show the command to run once. The installer has a new `--update` that keeps the agent's settings and needs no key. While an update is under way the label says *updating…*; after ten minutes without result it says *update failed?* and where to look. New endpoints `POST /api/v1/hosts/{host}/update` and `/api/v1/hosts-update-all`; the agent tells the server how it can update itself (`self_update`); the agent configuration carries `update_to` (not part of its revision).
- **Remove instances that are gone.** An instance that nobody registered here (set up with agent flags, or the leftover of an old setup) can be removed from the list once its agent has stopped reporting it, from the list or from its own page. Removing an instance that was added here no longer lets it come back as a "from agent flags" row while its agent catches up. A removed instance is listed again only if an agent reports it again. New endpoint `POST /api/v1/instance-keys/{address}/remove`; new collection `instances_gone`.
- **Tenants and the operator console** (Operator add-on). Tenant records (made automatically for existing tenants); the reserved `operator` tenant with the console, which a customer can never be given; create, suspend (refuse or drop incoming data), resume, export and remove tenants (removal deletes documents, telemetry, archive and backups, checks that nothing is left and keeps a tombstone); limits on hosts, instances, users, groups, keys, dashboards, items per second and bytes per day that warn or, if enforced, refuse (403 with a plain message, or 429 with `Retry-After`); metering per tenant and day with a CSV export; support access where the tenant decides (off, ask, allow), the operator goes in read-only or with write access for at most 4 hours, a banner shows it and everything done is in an audit log that the tenant can read. Nothing is enforced without an Operator licence, so a lapsed licence never cuts customers off. New pages: Tenants, and under Settings: usage and limits, support access, access log. New collections: `tenants`, `usage`, `audit`, `support_grants`. New command line: none; the operator is an administrator of the `operator` tenant.
- **Licensing.** Enterprise features now need a licence: a small file signed with Ed25519 and verified **offline** against public keys built into the program (nothing is sent anywhere). States none, valid, expiring (30 days), grace (30 days after the end, everything still works) and expired (Enterprise off; Community and all data untouched, channels kept and marked "licence needed", and they catch up on firing alerts when a licence is installed). Soft limits for hosts and tenants that only warn. Install it under Settings, or with `LUMEN_LICENSE_FILE`. A banner reminds administrators of the owning tenant. The publisher's tool `lumen-license` makes keys and licences (`keygen`, `issue`, `inspect`, with an optional ledger of what was issued). The Enterprise channel types (Jira, ServiceNow, PagerDuty, Opsgenie) are gated; the Docker build takes `LUMEN_BUILD_TAGS=enterprise`. See `docs/LICENSING.md`.
- **Alerting.** Rules on metrics, up/down status and log counts; a state machine (pending, firing, resolved, with a recovery period so values near a threshold do not flap); grouping into one message, reminders until acknowledged, silences, acknowledge, history and a delivery log with retries and back-off; a replay of the last 24 hours before a rule is saved; starter rules. Notifications by email (SMTP), signed webhook, Slack, Microsoft Teams and a heartbeat. Enterprise: Jira, ServiceNow, PagerDuty and Opsgenie (a ticket is created, commented on and closed with the alert). The Alerts page, a count in the menu and an "Alerts firing" box on Home. New permissions: Alerts and Notification channels. Channels refuse loopback, private and cloud-metadata addresses unless an administrator allows private ones (`LUMEN_ALERT_ALLOW_PRIVATE`).
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
