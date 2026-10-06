# Licensing

[Svenska](LICENSING.sv.md) · English

How Enterprise and Operator licences work, for the **publisher** who issues them and for the **customer** who uses them. The promises behind this are in [EDITIONS.md](EDITIONS.md): the check is offline, Lumen never contacts anyone, nothing is ever deleted when a licence ends, and Community features always keep working.

## How it works in one minute

- A licence is a small file signed with the publisher's **private** key. The program contains the matching **public** key, so it can check the signature without any network.
- The file says who it is for, which editions it covers, when it ends and, if wanted, soft limits for hosts and tenants.
- Without a licence Lumen is the **Community edition**. With a valid one the Enterprise features (today: Jira, ServiceNow, PagerDuty and Opsgenie notifications) work.
- 30 days before the end an amber reminder appears for administrators. After the end date there is a **30-day grace period** in which everything keeps working, with a red reminder. After that the Enterprise features stop. Community features, all data and all settings stay as they are, and a renewed licence switches everything back on at once.

## For the publisher

### One-time setup

```bash
go run ./cmd/lumen-license keygen --id main --dir keys
```

This writes `keys/main.key` (**private, secret**) and `keys/main.pub` (public). Then:

```bash
cp keys/main.pub internal/license/keys/main.pub
git add internal/license/keys/main.pub && git commit -m "Add the licence verification key"
```

Only the `.pub` file is committed. `*.key`, `*.license`, `/keys/` and `issued.jsonl` are in `.gitignore` so that they cannot be committed by mistake. **Keep the private key offline** (an encrypted USB stick or a hardware token is enough to start with), keep a second encrypted copy somewhere else, and never put it on a server that customers can reach or in a CI system. Whoever has it can issue licences.

### Building the Enterprise edition

The Enterprise code is compiled in only with the build tag `enterprise`, and the public keys are compiled in from `internal/license/keys/`.

```bash
# Docker: set it once in deploy/.env, then build as usual
echo "LUMEN_BUILD_TAGS=enterprise" >> deploy/.env
sudo docker compose -f deploy/docker-compose.yml up -d --build

# or without Docker
go build -tags enterprise -o lumen ./cmd/lumen
```

A build without your public key trusts no licence, so it is a Community build whatever file it is given.

### Issuing a licence

```bash
go run ./cmd/lumen-license issue --key keys/main.key --customer "ACME AB" \
  --editions enterprise --days 365 --hosts 50 --ledger issued.jsonl --out acme.license
```

- `--editions` is `enterprise`, `operator` or both (`enterprise,operator`).
- Give an end with `--days 365` or `--expires 2027-12-31` (the end of that day, UTC).
- `--hosts` and `--tenants` are **soft** limits: Lumen warns at 90 percent and when they are exceeded and never refuses data.
- `--issued 2026-12-31` dates the licence earlier or later than today, for example so that a renewal starts when the old one ends.
- `--ledger issued.jsonl` appends a line per licence: your own record of what you issued, to whom, until when. The tool does not phone anything; this file is yours to keep (and to back up, but not in the public repository).
- Send the customer the `.license` file. It is not secret, but it is made for them.
- Check a file at any time with `go run ./cmd/lumen-license inspect acme.license --pub keys/main.pub`.

A trial is a licence with a short end date (`--days 30`). A renewal is a new licence: the customer installs it over the old one.

### Renewing, revoking, changing keys

- **Renewing:** issue a new licence and send it. The customer installs it under Settings (or replaces the file, see below).
- **Revoking:** because nothing is checked online, a licence cannot be recalled. This is deliberate. Use **short terms** (a year, or less for customers you are unsure about) and simply do not renew.
- **Changing keys:** you can have several `.pub` files; a licence names the key that signed it. To retire a key, issue new licences with a new key, ship a release that contains both public keys until all old licences have ended, then remove the old one. If the private key is ever lost to someone else, do exactly this at once.

## For the customer

- **Install:** sign in as an administrator of the tenant that owns the installation, open **Settings**, and under **Licence** choose the `.license` file (or paste its contents) and press **Install licence**. A wrong or edited file is refused with the reason, and nothing changes.
- **As a file:** set `LUMEN_LICENSE_FILE` to a file path. It is read at start-up and again every minute, so a renewal can be dropped in as a file. A licence that comes from a file is renewed by replacing the file; the interface says so.
- **Who manages it:** the licence belongs to the installation, so in an installation with several tenants only the tenant that owns it (the one created at installation, `LUMEN_ADMIN_TENANT`) may see or change it. Another customer's administrator cannot remove it.

| State | What it means | Enterprise features |
|---|---|---|
| none | No licence: the Community edition | off |
| valid | In force, more than 30 days left | on |
| expiring | In force, 30 days or less left (amber reminder) | on |
| grace | Ended less than 30 days ago (red reminder) | **still on** |
| expired | Ended more than 30 days ago (red reminder) | off |
| cannot be used | A stored licence that no longer verifies (for example the build was changed to trust other keys); the reason is shown | off |

When Enterprise features are off, **nothing is deleted**: channels and settings stay, are marked "licence needed", and work again the moment a valid licence is installed. A notification channel that was shut out meanwhile is told about the alerts that are firing as soon as it is allowed again.

### Messages you may see

| Message | Meaning |
|---|---|
| the signature does not match | The file was changed after it was made (even one character), or it was made for something else |
| signed with the key "x", which this build does not trust | Another publisher's licence, or a build without your public key |
| this build of Lumen trusts no licence keys | A Community build of the program: use the build you received with the licence |
| that is not a Lumen licence file | Not the `.license` file (try pasting the whole file, braces included) |
| the licence in force is read from the file … | `LUMEN_LICENSE_FILE` is set: replace that file instead |

## Honest limits

- **The source is open.** Anyone who can build Lumen could change the check. The licence check keeps honest customers honest and makes accidental use easy to see; the contract is what protects the commercial edition. If some Enterprise code has real value, consider keeping it in a private repository (see the notes in the main discussion of editions).
- **A clock set back** extends a licence. Lumen does not try to defend against that (it would need an online check, which we promise never to do).
- **Host counts** in the warnings are those of the signed-in tenant until the Operator add-on has tenant records to count the whole installation.
