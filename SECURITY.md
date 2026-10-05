# Security policy

## Reporting a vulnerability

Please **do not open a public issue** for security problems.
Use GitHub's private reporting: **Security → Report a vulnerability** on this repository
(<https://github.com/danielingemar/lumen/security/advisories/new>).

Include what you found, how to reproduce it, and the version. You will get an answer as soon as possible;
this is an early-stage project maintained by a small team, so there is no formal response-time guarantee.

## Supported versions

Only the latest commit on `main` and the latest release receive fixes.

## What the project already does

- Passwords: salted PBKDF2-HMAC-SHA256, 600,000 iterations. Sessions: signed, HttpOnly, `SameSite=Strict` cookies. Logins are throttled.
- API keys are stored only as SHA-256 hashes and shown once.
- Stored Nextcloud credentials are encrypted with AES-GCM (`LUMEN_SECRET_KEY`) and never returned by the API.
- Every query is filtered by the tenant of the authenticated caller; permissions are enforced by the server, not the UI.
- Log paths pushed to agents from the server are checked against an allow-list on the machine.
- The logo upload is verified by its content, size-limited, refuses SVG with scripts or event handlers, and is served with `Content-Security-Policy: sandbox` and `nosniff`.

## Hardening your deployment

- Serve Lumen over **HTTPS** (API keys and agent configuration travel over it).
- Keep `deploy/.env` private (`chmod 600`) and back it up: it holds `LUMEN_SECRET_KEY`.
- Do not use `LUMEN_DEV_MODE=true` on a network you do not control: it disables authentication.
- Giving an agent `--docker` gives it access to the Docker socket, which is equivalent to root on that machine.
- The backup folder contains all tenants' data and a configuration dump with password hashes. Protect it like the database.
