# Before the first release

A working list. Tick a box when it is done, in the same commit that proves it. Decisions are marked **Decide**.

## 1. Repository and secrets

The repository is public, so this comes first.

- [x] Scan the full history with `gitleaks detect --source . --log-opts="--all"`: no findings (the findings left are reviewed false alarms, listed in `.gitleaks.toml`)
- [x] Rotate every real secret that was ever in the old history: the scan of all 49 reachable commits found none (only variables, an example command and test passwords)
- [x] Check that no `.tar.gz`, `.env`, `.key` or `.pem` is tracked: `git ls-files | grep -E '\.tar\.gz$|\.env$|\.key$|\.pem$'` prints nothing (`keys/main.pub` is fine, `main.key` never)
- [ ] Settle what happens to the old repository `danielingemar/lumen`: if it redirects to `inmagma/lumen` it is the same repository and nothing is deleted; if it is a separate copy, delete it
- [ ] Remove the temporary push key from GitHub (SSH keys) and from the build server
- [ ] Remove the read-only deploy keys that servers no longer need (the repository is public)
- [ ] Remove old working copies and backups on the build server (`backup-före-rensning`, `/opt/lumen.old`)
- [ ] The private licence key (`main.key`) exists only offline; only `internal/license/keys/main.pub` is in the repository
- [ ] Turn on branch protection for `main`, and secret scanning and push protection in the repository settings
- [ ] Fix the Go module path in the source tree from `your-org` to `inmagma`, so deliveries need no rewriting

## 2. Decisions

- [ ] **Decide** the product name (Lumen is a working name); check trademark and domain
- [ ] **Decide** the licence of the core (the code says Apache-2.0; the charter has it open)
- [ ] **Decide** DCO or CLA for contributions
- [ ] **Decide** the trademark policy for name and logo
- [ ] **Decide** the final edition names
- [ ] **Decide** the contact address for licensing questions
- [ ] Legal review of the promises in `docs/EDITIONS.md` and the licence terms

## 3. Product

- [ ] A versioned release: tag, changelog moved from *Unreleased* to the version, build version shown in the UI
- [ ] `latest.json` published with each release, read by the update check
- [ ] Update check and upgrade button in the interface (see the plan)
- [ ] Per-person alert assignment and tracking (Enterprise): build it, or keep it marked *planned* in the charter
- [ ] Every item marked *planned* in `docs/EDITIONS.md` is either built or still marked *planned*, never presented as available

## 4. Documentation and site

- [ ] README status line is accurate (it still says early MVP and no alerting)
- [ ] `UPGRADING.md` covers every change that needs a manual step since the first tagged version
- [ ] `SECURITY.md` has a working contact for reporting vulnerabilities
- [ ] The product website lists the same Community and Enterprise features as the charter
- [ ] A clean install on a fresh Rocky or Alma machine and on a fresh Ubuntu machine, following only the README

## 5. After the release

- [ ] `.rpm` and `.deb` packages
- [ ] More Ubuntu and Debian polish
