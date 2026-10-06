# Lumen editions: the charter

[Svenska](EDITIONS.sv.md) · English

> **Status: draft 0.1.** This is a statement of intent. It is not a contract and it is not legal advice. Items in [square brackets] are still to be decided. Have a lawyer review it before it is published as a promise.

## 1. Why this document exists

Lumen is developed in the open, and some of it will be sold. Anyone who builds on Lumen needs to know what stays free, what costs money and why, and that the line will not be moved against them later. This charter fixes that line in public.

## 2. Our promises

1. **Community is a real product.** Everything a single administrator or a small team needs to monitor their own infrastructure is in the Community edition: collecting, searching, dashboards, agents, alerting and backup. There are no artificial limits on hosts, users, dashboards, data volume or retention.
2. **No take-backs.** A feature that has shipped in a Community release will never be moved into a paid edition. A feature may move the other way, from a paid edition to Community, at any time.
3. **Security is never a paid feature.** Tenant isolation, authentication, authorisation checks, encryption of stored secrets and security fixes are in every edition. Security fixes are released to all editions at the same time.
4. **Your data is yours.** Data can always be exported through the API and the backup files, in plain documented formats that can be read without Lumen.
5. **No phone-home.** Neither edition sends usage data anywhere. Licence checks happen offline.
6. **No lock-out.** If an Enterprise licence expires, nothing is deleted and nobody is locked out of their data (see section 6).
7. **Agents are free.** The agents for Linux, Windows and Docker, and everything they collect, are Community features.
8. **Changes are public.** This charter changes only in the open (section 8).

## 3. The editions at a glance

*Exists* means the feature is in the code today. *Planned* means it is on the roadmap and may change before it ships.

| Capability | Community | Enterprise | Operator add-on |
|---|---|---|---|
| OTLP ingest, search, dashboards, metrics explorer, traces, logs | Yes (exists) | Yes | Yes |
| Agents, host performance, services and containers, Hosts and Instances, Nextcloud monitoring, up/down status | Yes (exists) | Yes | Yes |
| Local backups and archive, retention | Yes (exists) | Yes | Yes |
| Users, groups, area permissions, API keys | Yes (exists) | Yes | Yes |
| Tenant isolation | Yes (exists) | Yes | Yes |
| Site name and logo | Instance-wide (exists) | Instance-wide | Per tenant (planned) |
| Alert rules on metrics, status, logs and traces; email, webhook, Slack and Teams; acknowledge; basic silences; starter rules | Yes (exists, except trace rules) | Yes | Yes |
| OIDC sign-in with one provider | Yes (planned) | Yes | Yes |
| Audit log: who changed what, viewable by administrators | Yes (planned) | Yes | Yes |
| SAML, SCIM, LDAP group mapping; enforced MFA; API-key and session policies; PII redaction at ingest | | Yes (planned) | |
| Tamper-evident audit log with long retention and SIEM export | | Yes (planned) | |
| Jira, ServiceNow, PagerDuty, Opsgenie; escalation policies and on-call schedules; alert routing, grouping and templates; recurring maintenance windows with approval | | Yes (Jira, ServiceNow, PagerDuty and Opsgenie exist; the rest is planned) | |
| High availability, ClickHouse cluster, tiered storage and downsampling, zero-downtime upgrades, point-in-time restore | | Yes (planned) | |
| Encrypted offsite backups (for example S3), legal hold | | Yes (planned) | |
| Kubernetes operator, Terraform provider | | Yes (planned) | |
| SLOs and burn-rate alerts, SLA reports, status pages, multi-location checks, forecasting | | Yes (planned) | |
| Tenant console, quotas, usage metering and export | | | Yes (exists) |
| Per-tenant branding, domains and retention | | | Yes (planned) |
| Support access with consent, and the access log the tenant can read | | | Yes (exists) |
| Customer-facing reports, dedicated storage per tenant | | | Yes (planned) |
| Support with an SLA, long-term-support releases, signed builds | Community support through issues | Yes | Yes |

The **Operator add-on** is for running Lumen *for other people*: a hosting provider with customers, or a company with internal business units that each need isolated data, limits and reports. It can be combined with Enterprise.

## 4. How we decide where a feature goes

We ask these questions in order and stop at the first answer:

1. Is it needed for the security of the data, or for the product to work at all? **Community.**
2. Would one administrator or a small team on one server use it? **Community.**
3. Does it matter mainly for governance and compliance, for scale across teams and sites, for integration into corporate systems, or for running Lumen as a service for customers? **Enterprise** or **Operator.**
4. Is it a close call? **Community.**

## 5. What will never need a licence

Agents; the API and data export; tenant isolation and every authorisation check; encryption of stored secrets; basic users, groups and permissions; alert rules with email, webhook, Slack and Teams notifications; local backup and archive; security fixes; documentation.

## 6. Licence mechanics

*Status: this section is implemented (see [LICENSING.md](LICENSING.md)); the 90 percent warning and the grace period work as described.*

- An Enterprise or Operator licence is a **signed file that is verified offline**. It names the organisation, the editions, the expiry date and, for the Operator add-on, any limits.
- Limits in a licence are **soft**: Lumen shows warnings at 90 per cent and when a limit is exceeded. Lumen does not refuse data because of a licence. (Quotas that an operator sets for their own customers are a different thing; they are the operator's own configuration.)
- **When a licence expires** there is a 30-day grace period with a banner. After that the Enterprise features stop. Community features and all data are untouched, and nothing is deleted. A local administrator account always works, even if single sign-on is no longer available.

## 7. Contributions and licences

- The core is licensed under [Apache-2.0 — to be confirmed]. The `ee/` directory has its own commercial licence.
- Contributions to the core are accepted under [DCO or CLA — decide before the first outside contribution is accepted].
- Enterprise code is written by the maintainers or under contract.
- The name and logo are protected by a trademark policy [to be written].

## 8. How this charter changes

Changes are proposed in a public issue or pull request and announced in the changelog. The promises in section 2 may be strengthened, never weakened. The table in section 3 may change in two ways: a feature may move from Enterprise or Operator to Community at any time, and a *new* feature may be placed in any edition. A released Community feature is never moved.

## 9. Open items before this is published

- [ ] Legal review of the promises and the licence terms
- [ ] Decide the licence of the core
- [ ] Decide between DCO and CLA
- [ ] Trademark policy for the name and logo
- [ ] Final edition names
- [ ] Contact address for licensing questions
- [ ] Decide whether OIDC with one provider belongs in Community (this charter assumes it does)
