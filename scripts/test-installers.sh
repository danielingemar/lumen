#!/usr/bin/env bash
# Regression test for install.sh and dev.sh: re-running them (to upgrade) must never replace the address or the
# other settings already in deploy/.env. Uses a fake docker; touches nothing outside a temp directory.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
mkdir -p "$T/bin"
for c in docker curl sysctl; do printf '#!/bin/sh\nexit 0\n' > "$T/bin/$c"; done
printf '#!/bin/sh\n[ "$1" = "-I" ] && { echo 192.168.1.50; exit 0; }\nexec /bin/hostname "$@"\n' > "$T/bin/hostname"
# install.sh also writes /etc/sysctl.d/99-lumen.conf through tee; do not let a test touch the real system
printf '#!/bin/sh\ncat >/dev/null\n' > "$T/bin/tee"
chmod +x "$T/bin/"*
fail=0
ok() { echo "PASS $1"; }
bad() { echo "FAIL $1"; fail=1; }
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
run() { (cd "$T/repo" && PATH="$T/bin:$PATH" LUMEN_SKIP_SYSCTL_CHECK=1 "$@" >/dev/null 2>&1); }
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
