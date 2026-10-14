#!/usr/bin/env bash
# Regression test for install.sh and dev.sh: re-running them (to upgrade) must never replace the address or the
# other settings already in deploy/.env. Uses a fake docker; touches nothing outside a temp directory.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
mkdir -p "$T/bin"
REAL_HOSTNAME="$(command -v hostname || echo /bin/hostname)"
for c in docker curl sysctl; do printf '#!/bin/sh\nexit 0\n' > "$T/bin/$c"; done
printf '#!/bin/sh\n[ "$1" = "-I" ] && { echo 192.168.1.50; exit 0; }\nexec %s "$@"\n' "$REAL_HOSTNAME" > "$T/bin/hostname"
# install.sh insists on root; the test must work for any user (CI runs as a normal user), so pretend to be root
REAL_ID="$(command -v id)"
printf '#!/bin/sh\n[ "$1" = "-u" ] && { echo 0; exit 0; }\nexec %s "$@"\n' "$REAL_ID" > "$T/bin/id"
# install.sh also writes /etc/sysctl.d/99-lumen.conf through tee; do not let a test touch the real system
printf '#!/bin/sh\ncat >/dev/null\n' > "$T/bin/tee"
chmod +x "$T/bin/"*
fail=0
ok() { echo "PASS $1"; }
bad() { echo "FAIL $1"; echo "----- output of the last run:"; tail -n 25 "$T/last.log" | sed 's/^/    | /'; echo "----- deploy/.env now:"; grep -E '^(LUMEN_PUBLIC_URL|LUMEN_BIND)=' "$T/repo/deploy/.env" 2>/dev/null | sed 's/^/    | /' || true; fail=1; }
env_of() { grep "^$1=" "$T/repo/deploy/.env" | cut -d= -f2-; }
fresh() { rm -rf "$T/repo"; mkdir -p "$T/repo"; cp -r "$ROOT/dev.sh" "$ROOT/install.sh" "$ROOT/deploy" "$T/repo/"; rm -f "$T/repo/deploy/.env"; }
seed() { cat > "$T/repo/deploy/.env" <<'ENV'
ELASTIC_PASSWORD=keep-es
LUMEN_SECRET_KEY=keep-secret
LUMEN_ADMIN_USER=admin
LUMEN_ADMIN_PASSWORD=keep-admin
LUMEN_ADMIN_TENANT=main
LUMEN_API_KEYS=lmn_k:main
LUMEN_PUBLIC_URL=https://lumen.example.com
LUMEN_BIND=127.0.0.1
ENV
}
# a failing installer must show up as a FAIL with its output, not silently end the test (set -e)
run() { (cd "$T/repo" && PATH="$T/bin:$PATH" LUMEN_SKIP_SYSCTL_CHECK=1 "$@" >"$T/last.log" 2>&1) || true; }
same() { [ "$(env_of ELASTIC_PASSWORD)" = keep-es ] && [ "$(env_of LUMEN_SECRET_KEY)" = keep-secret ] && [ "$(env_of LUMEN_ADMIN_PASSWORD)" = keep-admin ]; }

fresh; seed; run bash install.sh --force
[ "$(env_of LUMEN_PUBLIC_URL)" = https://lumen.example.com ] && [ "$(env_of LUMEN_BIND)" = 127.0.0.1 ] && same && ok "install.sh without flags keeps the address, bind address and secrets" || bad "install.sh without flags changed .env"
fresh; seed; run bash install.sh --force --public-url https://other.example.com/
[ "$(env_of LUMEN_PUBLIC_URL)" = https://other.example.com ] && [ "$(env_of LUMEN_BIND)" = 127.0.0.1 ] && same && ok "install.sh --public-url changes only the address" || bad "install.sh --public-url"
fresh; run bash install.sh --force
[ "$(env_of LUMEN_PUBLIC_URL)" = http://192.168.1.50:4318 ] && ok "install.sh on a new machine detects an address" || bad "install.sh fresh"
fresh; seed; run bash dev.sh
[ "$(env_of LUMEN_PUBLIC_URL)" = https://lumen.example.com ] && [ "$(env_of LUMEN_BIND)" = 127.0.0.1 ] && same && ok "dev.sh without flags keeps the address, bind address and secrets" || bad "dev.sh without flags changed .env"
fresh; seed; run bash dev.sh --ip 10.0.0.5
[ "$(env_of LUMEN_PUBLIC_URL)" = http://10.0.0.5:4318 ] && [ "$(env_of LUMEN_BIND)" = 127.0.0.1 ] && same && ok "dev.sh --ip changes the address and keeps the rest" || bad "dev.sh --ip"
fresh; run bash dev.sh
[ "$(env_of LUMEN_PUBLIC_URL)" = http://192.168.1.50:4318 ] && [ "$(env_of LUMEN_BIND)" = 0.0.0.0 ] && ok "dev.sh on a new machine detects an address and binds to all interfaces" || bad "dev.sh fresh"
exit $fail
