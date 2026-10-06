# Design proposal: tenancy

[Svenska](tenancy.sv.md) · English

> **Status: proposal, no code yet.** This document describes a design and the decisions it needs. Effort sizes are rough (S = days, M = a few weeks, L = more than a month, for one developer).

> **Implementation status (October 2026).** Phases 1 and 2 are built, and the removal with purge from phase 3. Differences from this proposal: usage is kept in the document store (collection `usage`) instead of a ClickHouse table; instead of an operator *asking* and the tenant approving, the tenant allows access beforehand for a chosen time (Settings, Support access); there is no limit on stored volume, on the length of a query or on concurrent queries; retention, branding and sign-in addresses per tenant and dedicated storage (phases 3 and 4) are not built.

## 1. Summary

Lumen already separates customers by *tenant*. This proposal adds what both of our buyers need on top of that: an **operator level above the tenants** (the people who run Lumen), **tenant records** with limits and lifecycle, **usage metering**, **per-tenant retention and branding**, and, later, **dedicated storage** for tenants that need hard isolation.

The one-line recommendation: **keep a flat list of tenants and add an operator layer above it**, and leave room for a parent/child relationship between tenants without building it yet.

## 2. Who needs what

| | Service providers | Platform teams in larger companies |
|---|---|---|
| A tenant is | a customer | a team or business unit |
| An operator is | the provider's own staff | the central platform team |
| Needs first | quotas, usage for billing, white-label, customer reports | audit, SSO, central control, chargeback |
| Needs later | dedicated storage for large customers | dedicated storage for regulated units |

The same primitives serve both: tenant, operator, quota, usage, audit.

## 3. How tenancy works today

- A **tenant** is a string. It exists because users, API keys, groups, dashboards, hosts and instances carry it. There is no tenant record.
- The tenant of a request comes **only from the authenticated identity** (the user's tenant, or the API key's tenant). A request cannot name another tenant.
- **ClickHouse:** every row of spans, logs and metrics has a `tenant` column, and it is the first column in each table's sort key. Every query is built with a tenant filter, and tests check that another tenant gets nothing.
- **Elasticsearch (or the JSON file):** one shared index per collection. Documents carry `tenant`, and reads filter on it.
- **Backups** are written per tenant and per day, in separate directories. The configuration dump contains all tenants and is for the operator only.
- **Branding** (name and logo) is for the whole installation.
- **Retention** is one setting for the whole installation.
- **Quotas and metering:** none. **Tenant creation:** implicit, through the command line (`users add --tenant`, `keys create --tenant`) or the first administrator from the environment.
- There is **no operator concept**: every administrator is an administrator of one tenant.

## 4. Requirements

| # | Requirement | Both buyers? |
|---|---|---|
| R1 | A person who runs Lumen can see and manage all tenants, without being able to read their data by accident | Yes |
| R2 | Create, suspend, resume and offboard a tenant, including a complete purge of its data | Yes |
| R3 | Limits per tenant (hosts, users, ingest rate, stored volume) with clear errors and warnings | Yes |
| R4 | Usage per tenant and day, exportable, suitable for billing or chargeback | Yes |
| R5 | Retention that can differ per tenant | Yes |
| R6 | Branding and a sign-in address per tenant (white-label) | Providers |
| R7 | Support access: the operator can look inside a tenant, only with a trace and ideally with the tenant's consent | Yes |
| R8 | Hard isolation on request: separate storage for a tenant | Large customers |
| R9 | One tenant cannot slow down the others (noisy neighbour) | Yes |
| R10 | A single-tenant installation sees none of this and nothing changes for it | Yes |

## 5. Options for the model

| | A. Flat tenants plus an operator layer | B. Hierarchical tenants (organisation, business units) | C. Workspaces inside a tenant |
|---|---|---|---|
| What it is | Operator, then tenants, then users | Tenants can have child tenants that inherit limits and policies | A tenant contains projects with their own dashboards and permissions |
| Fits providers | Yes | Partly (resellers) | No |
| Fits companies | Mostly | Yes | Yes |
| Cost | Small | Large: inheritance of quotas, permissions, branding | Medium: a new scope on every object |
| Risk | Companies may want sub-units later | Complexity before it is proven | Duplicates what groups already do |

**Recommendation: A, with a hook for B.** Give each tenant record an optional `parent` field that is empty and unused for now. If a customer needs sub-units, add them then. Option C is not needed: groups and dashboards already cover most of it.

## 6. The design

### 6.1 Tenant record

A new collection `tenants` with one record per tenant: id (a short slug used in URLs and storage), display name, status (`active`, `suspended`, `offboarding`), created and last-active time, contact, notes, quotas (6.4), retention override (6.6), branding (6.7), domains (6.7), `support_access` (`off`, `ask`, `allow`), and the empty `parent`.

**Migration:** at start-up, every tenant string that already exists on users, keys or documents gets a record with default values. This is idempotent, so running it twice changes nothing.

### 6.2 The operator

- **Operators are users of a reserved tenant** called `operator`. They sign in like anyone else.
- A new permission area, `operator`, controls the console. It can only be given to groups in the reserved tenant.
- An operator **cannot read a tenant's data by default**. To look inside, they *enter* a tenant: a time-limited session (default 60 minutes), read-only unless they explicitly ask for write, with a visible banner. Every entry and every action inside is written to the audit log with the operator's name.
- When a tenant has set `support_access` to `ask`, the entry waits for a tenant administrator to approve; with `off`, entry is refused. (The Operator add-on provides the setting; in Community, an operator is whoever holds the installation, as today.)
- In a **single-tenant installation** the operator layer is dormant: no console, no banner, no behaviour change.

### 6.3 Lifecycle

| Step | What happens |
|---|---|
| Create | The operator creates the tenant and its first administrator. A tenant is created with the limits of a chosen *plan* (a named set of quotas). |
| Suspend | Sign-in is refused for the tenant's users with a clear message. Incoming data is **rejected** with a clear error (default) or accepted and dropped (an option), so the customer's agents do not fill their disks. Data already stored is kept. |
| Resume | Everything works again at once. |
| Offboard | An export is offered first (backup files and a JSON dump of documents). Then a purge runs: ClickHouse rows are deleted by tenant, documents are deleted, backup directories are removed, keys are revoked. A purge report lists what was removed. The audit record of the offboarding is kept. |

### 6.4 Quotas and limits

| Limit | Counted as | Enforced |
|---|---|---|
| Hosts | distinct hosts reporting in the last 24 hours | warn; hard limit refuses new hosts |
| Instances, users, groups, keys, dashboards | number of objects | creation fails with a message that names the limit |
| Ingest rate | accepted items per second per tenant | the server answers **429 with `Retry-After`**; OTLP clients retry |
| Daily ingest volume | accepted bytes per day | 429 after the limit; warn at 80 per cent |
| Stored volume | rows or bytes in storage, sampled periodically | warn; optionally refuse ingest |
| Queries | concurrent queries and maximum time range | 429 or 400 with a message |

Limits are *soft* by default (warnings) and *hard* when the operator chooses. A tenant administrator sees their own limits and use on a page; the operator sees all tenants in the console.

### 6.5 Usage metering

- The server counts accepted items and bytes per tenant and signal in memory and writes them once a minute to a ClickHouse table `usage_events`, rolled up daily into `usage_daily`: tenant, day, signal, items, bytes, distinct hosts, instances, queries.
- Export as CSV or JSON through the API, for the operator's own billing. Lumen does **not** do billing.
- Metering is accurate enough for billing *guidance*. The definition ("accepted OTLP payload bytes") is documented, so customers can verify it.

### 6.6 Retention per tenant

Today the whole table expires by one rule. Three ways to differ per tenant:

| Option | How | Trade-off |
|---|---|---|
| **A. Expiry column** | A new `expires` column is set at insert from the tenant's retention; the table's rule becomes "delete when `expires` has passed" | One rule for all tenants; a one-time backfill of old rows is needed. **Recommended.** |
| B. Several conditional rules | One expiry rule per tenant, each with a condition on `tenant` | Changes the table definition whenever a tenant changes |
| C. Scheduled deletes | A job deletes old rows per tenant | Heavy deletes; slower to reclaim space |

Backups follow the tenant's own setting too: per-tenant backup retention, since backups are already per tenant.

### 6.7 Branding and sign-in address

- Name and logo become per tenant. The sign-in page does not know the tenant before sign-in, so the tenant is chosen by **host name**: `customer.example.com` maps to a tenant, and the sign-in page shows that tenant's branding. The bare host name belongs to a default tenant.
- On a tenant's host name, sign-in is **limited to that tenant's users** (and operators). That also stops anyone from discovering which tenants exist.
- TLS for custom domains stays with the reverse proxy, as it does today. Lumen only maps the host name.
- A tenant's logo is served with the same restrictions as today (verified by content, no scripts).

### 6.8 Isolation

Isolation stays **logical** (shared tables, tenant filter) for everyone, and is protected in layers:

1. The tenant comes only from the identity, never from a request parameter.
2. Query builders add the tenant filter themselves; a database query without one cannot be built through them.
3. Tests: every new endpoint and every new query gets a test that a second tenant sees nothing. A shared test helper makes that cheap to write.
4. Fuzz or property tests on the query builders and the document filters.
5. Per-query limits (time, memory) against noisy neighbours (R9).

**Dedicated storage** (R8) is a later option: a tenant is mapped to a *storage profile* (ClickHouse database or server, Elasticsearch index prefix, backup directory). Everything that talks to storage looks the profile up by tenant. It costs real operations work (migrations, backups and upgrades per profile), so it is the last phase and an add-on feature. Database-level row policies in ClickHouse were considered and rejected for now: they need a database user per tenant.

### 6.9 API and screens

- `GET/POST /api/v1/operator/tenants`, `GET/PUT/DELETE /api/v1/operator/tenants/{id}`, with `suspend`, `resume` and `offboard` actions.
- `GET /api/v1/operator/usage?from=&to=&tenant=` (JSON or CSV), `GET /api/v1/usage` for a tenant's own use.
- `POST /api/v1/operator/tenants/{id}/enter` and `/leave`.
- Operator screens: *Tenants* (status, hosts, instances, users, ingest rate, stored volume, last activity, warnings), a tenant page (limits, usage charts, users, domains, suspend, enter), and *Usage*.
- Tenant screens: *Usage and limits* under Settings.

### 6.10 Permissions

One new area, `operator`, with the usual none, read and write levels, assignable only in the reserved tenant. A tenant's own limits and usage are readable with the existing Settings permission.

## 7. Migration from the code as it is

1. Create tenant records for existing tenants (idempotent) at start-up.
2. Add the `operator` area and the reserved tenant; nothing is visible until an operator exists.
3. Existing administrators stay what they are. The first administrator created through the environment in a **single-tenant** installation does not become an operator.
4. Per-tenant retention (6.6) needs the `expires` column and a backfill. This is the one change that touches stored data; run it as an explicit, resumable step with a progress display.
5. Everything else adds new collections and new fields with defaults.

## 8. Security considerations

- **Operator accounts are the most valuable accounts.** Recommend and (in Enterprise) enforce MFA for them; a short session lifetime; every operator action audited.
- **Enumeration:** sign-in on a tenant's host name must not reveal whether a user exists in another tenant.
- **Quota bypass:** limits are checked on the server and cannot be influenced by a client.
- **Impersonation:** an entered session cannot change passwords, keys or the tenant's own administrators unless write was requested, and these always appear in the audit log.
- **Purge must be verified:** after an offboarding, a check confirms that no rows, documents or files remain for the tenant.

## 9. Which edition

| Edition | Contents |
|---|---|
| Community | Tenant isolation (always), implicit tenants and the command line as today, the audit log of sensitive actions |
| Operator add-on | Tenant console and lifecycle, quotas, usage metering and export, per-tenant branding, domains and retention, support access with consent, dedicated storage |
| Enterprise | MFA enforcement for operators, SAML per tenant, audit export |

Tenant isolation never moves to a paid edition (see the edition charter).

## 10. Phases

| Phase | Contents | Size |
|---|---|---|
| 1 | Tenant records, the operator layer and entering a tenant, audit log, lifecycle (create, suspend, resume) | M |
| 2 | Quotas and usage metering with export, the tenant's own usage page | M |
| 3 | Per-tenant retention (the `expires` column), per-tenant branding and host-name mapping, offboarding with purge | M to L |
| 4 | Dedicated storage profiles | L |

## 11. Decisions needed

1. Operator model: flat tenants plus an operator layer (recommended) or hierarchical tenants.
2. The reserved tenant name for operators, and whether operators may exist in a single-tenant installation.
3. Suspension: reject incoming data (recommended) or accept and drop.
4. Retention approach: the `expires` column (recommended) or one of the alternatives.
5. Whether the tenant-facing usage page is Community or part of the Operator add-on.
6. How strong isolation must be: logical only, or dedicated storage for some tenants.

## 12. How we will test it

- A shared cross-tenant test: every endpoint and query is called as tenant A with tenant B's identifiers, and must return nothing or refuse.
- Quota tests with a fake clock: limits, warnings, `Retry-After`, and resetting at midnight.
- Migration tests on a copy of real-shaped data, including running the migration twice.
- Offboarding test: after a purge, no row, document, file or key is left.
- A load test with a noisy tenant next to a quiet one, to check query limits and 429 behaviour.
