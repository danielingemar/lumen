# Design proposal: alerting

[Svenska](alerting.sv.md) · English

> **Status: proposal, no code yet.** This document describes a design and the decisions it needs. Effort sizes are rough (S = days, M = a few weeks, L = more than a month, for one developer).

> **Implementation status (October 2026).** Phases 1 and 2 are built, plus the Enterprise notifiers of phase 3. Differences from this proposal: *routes* are matchers on the channel (severities and labels) instead of a separate object; alert history and the delivery log live in the document store (history is pruned to the newest 1000 per tenant) instead of a ClickHouse table; messages use a fixed layout, not templates; trace rules, escalation and on-call, maintenance windows, assignment, interactive chat actions, SLOs and the engine's own metrics are not built yet.

## 1. Summary

Lumen shows what is wrong but cannot yet **tell anyone**. This proposal adds an alerting engine to the open core, with email, webhook, Slack and Teams, and defines the extension point through which Enterprise adds Jira, ServiceNow, PagerDuty, Opsgenie, escalation and on-call.

The principle: **the core evaluates and delivers; Enterprise adds who gets woken up and which system receives a ticket.**

## 2. Goals and non-goals

**Goals**
- Alert on anything a chart can show: a metric, the up/down status, log lines, trace errors and latency.
- Alert from the screens people already use ("create an alert from this chart").
- Never lose an alert silently, and never flood people: grouping, repeat limits, silences.
- Be safe by default: the server will make outgoing requests for users, so this must not become a way into the internal network.
- Stay a single binary using only the Go standard library.

**Non-goals (for now)**
- Anomaly detection and forecasting (Enterprise, later).
- An incident-management product. Lumen hands alerts to the tools that do that.
- Notifying mobile apps directly.

## 3. What exists today that we can reuse

- The **query model** of dashboard panels (metric, aggregation, filters, group by, range) and the store methods that run it.
- The **up/down computation** (hosts, services, containers, Nextcloud instances) and the latest-value query.
- A way to **store secrets** encrypted (used for Nextcloud tokens), and the document store with per-tenant collections.
- Permissions by area, and an `edition` seam for replacing behaviour.
- The backup job's configuration dump, which will need the new collections.

What does **not** exist: a scheduler, outgoing email or HTTP from the server, alert state, and any history of alerts.

## 4. Concepts

| Term | Meaning |
|---|---|
| **Rule** | A condition, how often to check it, how long it must hold, a severity and labels |
| **Alert** | One firing instance of a rule for one set of labels (for example "disk over 90 per cent on web1 at `/data`") |
| **State** | `ok`, `pending` (condition true, not yet for long enough), `firing`, `resolved` |
| **Channel** | A destination: an email address list, a webhook, a Slack channel |
| **Route** | Which alerts go to which channels, by severity and labels |
| **Silence** | A time-limited mute for alerts that match a pattern |
| **Maintenance window** | A planned period during which matching alerts are muted (Enterprise adds recurring windows and approval) |

## 5. Rules

| Kind | Example | Evaluated with |
|---|---|---|
| **Metric** | CPU above 90 per cent for 10 minutes, per host | the chart query, aggregated over a window |
| **Status** | Host down for 2 minutes; Nextcloud instance down; service or container failed | the same up/down computation the UI uses |
| **No data** | A host or metric stopped reporting | latest value older than a limit |
| **Log** | More than 20 `ERROR` lines in 5 minutes from a service | a log count over a window |
| **Trace** | Error rate or the 95th percentile above a limit | the trace series query |

A rule has: a name, a kind and its query (stored exactly as a chart stores it), a comparison and threshold, an evaluation interval (default 60 s, minimum 15 s), a `for` duration, a severity (`critical`, `warning`, `info`), extra labels, an annotation text, and whether it is enabled. **"Create alert from this chart"** copies the chart's query into a new rule.

**Backtest.** Before saving, the editor can replay the rule over the last 24 hours and show when it *would* have fired. This is the single most useful feature for getting thresholds right, so it ships with the first version.

## 6. Evaluation

- A scheduler inside the server runs each rule at its interval, with a little jitter so rules do not all run on the same second.
- Each evaluation runs a **tenant-filtered query** with a timeout, and produces zero or more *alert instances*, each identified by a fingerprint (rule id plus its sorted labels).
- **State machine:** `ok` becomes `pending` when the condition is true; `pending` becomes `firing` once it has held for `for`; `firing` becomes `resolved` after the condition has been false for a **recovery period** (default two evaluations) so that a value hovering around the threshold does not flap.
- **No-data policy** per rule: treat as OK, treat as firing, or keep the last state.
- **Errors in evaluation** (database down, query too slow) do **not** resolve alerts. The rule shows an *error* health state, and a built-in alert tells people that alerting itself is unwell.
- **Restarts:** state is persisted, so a restart does not re-notify for alerts that were already firing, and does not lose a pending alert's clock.
- **Limits** protect the server: a maximum number of rules per tenant, a minimum interval, a maximum number of series per evaluation, and a query timeout.
- **Single evaluator.** The Community edition runs one server, so one evaluator. High availability (Enterprise) adds a lease so that only one server evaluates at a time.

## 7. Notifications

### 7.1 The pipeline

1. **Route** by severity and labels to one or more channels.
2. **Group** alerts that belong together (default: by rule and host), wait a short time (30 s) so related alerts arrive in one message, and send one message for the group.
3. **Repeat** while still firing, at a configurable interval (default 4 hours); stop when acknowledged.
4. **Send** with a timeout and retries with growing delays for up to an hour. A **delivery log** records every attempt and its result.
5. **Resolve** message when the group has recovered.

Failures to deliver are visible in the interface and raise a built-in alert. A channel that keeps failing is marked unhealthy, so nobody believes they are protected when they are not.

### 7.2 The extension point

The core defines what a **notifier** is: a name, a way to validate its settings, a way to **send** a group of alerts and report success or failure, and a **test** action. Community registers the notifiers below. Enterprise adds more by registering them at start-up; the core never imports Enterprise code (see the edition charter).

### 7.3 Channels by edition

| Channel | Edition |
|---|---|
| **Email** (SMTP with TLS and authentication) | Community |
| **Webhook** (JSON, signed with a secret, custom headers) | Community |
| **Slack**, **Microsoft Teams** (incoming webhooks) | Community |
| **Heartbeat** (a regular outgoing ping to an outside service, so you notice when Lumen itself is down) | Community |
| **PagerDuty**, **Opsgenie** | Enterprise |
| **Jira**, **ServiceNow** (create a ticket, add comments on repeats, close it on recovery, remember which alert belongs to which ticket) | Enterprise |
| **Escalation policies and on-call schedules** (notify A now, B after 15 minutes if nobody acknowledged; rotations; overrides) | Enterprise |
| **Interactive chat messages** (acknowledge from Slack or Teams) | Enterprise |

### 7.4 Messages

Message texts are templates over a small set of fields (rule, labels, value, state, link to Lumen). Templates cannot call functions or read anything else, so a template can never leak data or run code. Enterprise adds per-channel templates and a template gallery.

## 8. Acknowledging and silencing

- **Acknowledge** (Community): marks an alert as seen and stops repeat notifications; the state stays `firing` until it recovers. Who acknowledged, and when, is recorded. **Assignment** to a person is Enterprise.
- **Silence** (Community): mute alerts matching labels, with an end time and a reason, created from the interface in two clicks (for example from an alert, "silence for 2 hours"). Silenced alerts are still evaluated and shown, but not sent.
- **Maintenance windows** (Enterprise): recurring windows, approval, and a calendar view, per tenant.

## 9. Security

- **Server-side request forgery.** A webhook URL makes the server call something on a user's behalf. By default the server **refuses addresses that are loopback, link-local, private or cloud-metadata addresses**, checked *after* name resolution and again on every redirect. An administrator may allow specific internal destinations. Timeouts and response size limits apply.
- **Secrets** (SMTP passwords, tokens, webhook secrets) are encrypted at rest and **never shown again** in the interface, as for Nextcloud tokens. Changing a channel does not require re-entering a secret that stays the same.
- **Email:** headers are built from validated fields only, to prevent header injection; the sender address is fixed per installation or channel.
- **Webhook signing:** each request carries a signature over the body and a timestamp, so receivers can verify it and reject replays.
- **Permissions:** two new areas. `alerts` (read: see rules, alerts, silences; write: manage rules and silences) and `notifications` (manage channels, which hold secrets and can reach the network). The built-in *User* group gets read on alerts only.
- **Audit:** every change to a rule, route, channel or silence is recorded with who and when.
- **Abuse limits:** per-tenant limits on rules, channels and messages per hour, so a mistaken rule cannot send ten thousand emails.

## 10. Tenancy

Rules, channels, routes and silences belong to a tenant, and every evaluation query is tenant-filtered. Evaluation uses a fair share of the server: one tenant's expensive rules cannot starve the others. Under the Operator add-on, an operator can provide **default channels** (for example the provider's own mail server) that tenants may use without seeing the secrets.

## 11. A starter set of rules

Shipped as templates the administrator can enable in one click (some enabled by default on a new installation):

| Rule | Condition |
|---|---|
| Host down | an agent has not reported for 2 minutes |
| Nextcloud instance down | the instance is down for 1 minute |
| Service failed, container down | a watched service or container is not running |
| Disk almost full | a mount point is above 90 per cent |
| Memory pressure | available memory below 10 per cent for 10 minutes |
| High CPU | above 90 per cent for 10 minutes |
| Backup missing | no successful backup in 36 hours |
| Log paths refused | an agent refused log paths from the server |
| Lumen unwell | the database does not answer, ingest errors, alert delivery failing |
| Disk full in 24 hours (forecast) | Enterprise |

## 12. Interface and API

**Screens:** *Alerts* (what is firing and pending, with filters, acknowledge, silence); *Rules* (list and editor with query preview and backtest); *Channels* (with a **Test** button); *Silences*; *History*. A "create alert" action on chart panels, host pages and instance pages. The Home page shows how many alerts are firing.

**API (sketch):** `GET/POST/PUT/DELETE /api/v1/alerts/rules`, `POST /api/v1/alerts/rules/{id}/backtest`, `GET /api/v1/alerts` (current), `POST /api/v1/alerts/{id}/ack`, `GET/POST/DELETE /api/v1/alerts/silences`, `GET/POST/PUT/DELETE /api/v1/notifications/channels`, `POST /api/v1/notifications/channels/{id}/test`, `GET /api/v1/alerts/history`.

## 13. Storage

New document collections: rules, routes, channels (with sealed secrets), silences, and the current state of alerts. **History** (state changes and delivery attempts) goes to a ClickHouse table with a time limit, so the document store does not grow without bound. The backup job's configuration dump includes the new collections (but never decrypted secrets).

## 14. Watching the watcher

Lumen reports its own alerting as metrics: evaluations, evaluation time, errors, alerts by state, notifications sent and failed, and queue length. The **heartbeat** channel and the built-in "Lumen unwell" rules cover the case where Lumen cannot tell you itself.

## 15. Failure modes

| Situation | Behaviour |
|---|---|
| ClickHouse unavailable | Evaluation errors; alerts keep their state; the built-in alert fires through channels that do not need the database |
| Document store unavailable | The server keeps running on in-memory state and retries writing; it never discards firing state |
| SMTP or a webhook is down | Retries with backoff; delivery log shows the failure; the channel is marked unhealthy |
| A rule is very expensive | Query timeout; the rule is marked in error; limits stop it from affecting others |
| The server restarts | State is reloaded; alerts that were already notified are not sent again |
| The clock jumps | Time-based decisions use monotonic time where possible; large jumps are logged |

## 16. Which edition

See section 7.3 for channels. In short: **Community** has rules, state, grouping, repeat limits, acknowledge, silences, email, webhook, Slack, Teams, heartbeat, the starter rules and backtest. **Enterprise** adds ticket and on-call integrations, escalation, routing and templates at scale, recurring maintenance windows with approval, assignment, interactive chat actions, SLOs and forecasting.

## 17. Phases

| Phase | Contents | Size |
|---|---|---|
| 1 | Engine, metric and status rules, state machine, email and webhook, alerts list and rule editor, acknowledge, basic silences | L |
| 2 | Log and trace rules, Slack and Teams, backtest, starter rules, history, self-monitoring and heartbeat, "create alert" from charts | M |
| 3 (Enterprise) | PagerDuty, Opsgenie, Jira, ServiceNow, escalation and on-call, routing and templates, maintenance windows | L |
| 4 (Enterprise) | SLOs and burn-rate alerts, forecasting, anomaly detection | L |

## 18. Decisions needed

1. Minimum evaluation interval (15 s proposed) and the maximum number of rules per tenant.
2. Where alert history lives (a ClickHouse table is proposed).
3. Whether email needs a per-tenant sender, or one sender per installation.
4. How to acknowledge: in the interface only, or also from a link in the email.
5. Whether log-based rules are limited, because they are the most expensive to evaluate.
6. Which starter rules are enabled by default on a new installation.

## 19. How we will test it

- **State machine:** table-driven tests with a fake clock for every transition, including flapping, no-data policies and restarts.
- **Evaluation:** a fake store that returns scripted series.
- **Notifiers:** contract tests against a local HTTP server and a local SMTP server (retries, timeouts, redirects, signature).
- **Security:** tests that private, loopback and metadata addresses are refused, including through a redirect and through name resolution; template sandbox tests.
- **End to end:** the real agent and the real server, an agent stopped on purpose, and a check that exactly one notification arrives and one resolve follows.
- **Chaos:** restart the server while an alert is firing and pending.
