# Running Lumen for several tenants (operator guide)

[Svenska](OPERATING.sv.md) · English

For the people who run an installation for customers or teams. The design behind this is in [design/tenancy.md](design/tenancy.md), the licence in [LICENSING.md](LICENSING.md).

## Set up

1. Install with `LUMEN_ADMIN_TENANT=operator` in `deploy/.env`. The first administrator then belongs to the reserved tenant `operator`, which holds the **Tenants** console and owns the licence. (To add more operators: `lumen users add --tenant operator --group admin NAME`.)
2. Install an **Operator licence** under *Settings, Licence*. Without it the console is closed, Lumen only counts what each tenant sends, and nothing is enforced: a lapsed licence never cuts your customers off.
3. Open **Tenants**. Tenants that already existed are there, with default settings (support access: *any time*, because they predate the setting). Add new ones with **+ Add tenant**.

## Day to day

| I want to... | Do this |
|---|---|
| Add a customer | Tenants, + Add tenant. Give an id (lower case, used in file names, cannot be changed), a name and the first administrator. A password is made and shown once. |
| Set limits | Open the tenant, fill in the limits, tick **Refuse** to enforce them, Save. Without Refuse they only warn, on your page and on the customer's Settings page. |
| Stop a customer who has not paid | Open the tenant, Suspend, give a reason. Choose in the tenant's details whether incoming data is *refused* (agents keep trying) or *thrown away* (agents think all is well). Resume restores everything at once. |
| Help a customer | Ask them to allow support access (Settings, Support access, Allow access). Then open the tenant and **Go into tenant**: read-only unless you tick changes, 30 minutes to 4 hours. A banner shows you are inside; **Leave tenant** ends it. |
| Invoice | Tenants, Usage: per tenant and day, or **Download CSV** (the period and tenant you chose). |
| Give a customer their data | Open the tenant, **Export** (a zip of settings and documents without secrets). Telemetry is in the daily backups. |
| Remove a customer | Open the tenant, **Remove...**, type its id. Everything of the tenant is deleted, also the archive and the daily backups, and Lumen checks that nothing is left. Usage records and the access log are kept. |

## What the customer sees

Under **Settings**: their usage against their limits (and a warning near a limit), **Support access** (off, only when they allow it, any time) and an **Access log** that lists every time an operator came in and everything that was changed.

## Good to know

- **Limits** apply to a customer's own users and keys, not to an operator inside the tenant, who can always fix things.
- **Hosts** are counted as those that sent metrics in the last 24 hours. A host already reporting is never turned away by the host limit.
- **A removal that fails** shows the step and the reason on the tenant's page, and can be run again. Stop the tenant's agents first if data keeps arriving.
- **Configuration dumps** in the backup directory contain the settings of every tenant until they are replaced (the newest 30 are kept); the removal says so.
- **One server evaluates everything.** There is no high-availability mode yet.
