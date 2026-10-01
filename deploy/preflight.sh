#!/usr/bin/env bash
# Lumen preflight: checks the host before installation. Read-only, changes nothing.
# Exit code 1 if any check FAILs; WARNs are advisory.
set -u
FAIL=0; WARN=0
ok()   { printf '  \033[32mPASS\033[0m  %s\n' "$*"; }
warn() { printf '  \033[33mWARN\033[0m  %s\n' "$*"; WARN=$((WARN+1)); }
fail() { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAIL=$((FAIL+1)); }
have() { command -v "$1" >/dev/null 2>&1; }
PORT="${LUMEN_PORT:-4318}"
HERE="$(cd "$(dirname "$0")" && pwd)"

echo "Host"
. /etc/os-release 2>/dev/null
case "${ID:-}" in
  almalinux|rocky|rhel|centos) ok "OS: ${PRETTY_NAME:-$ID}";;
  *) warn "OS: ${PRETTY_NAME:-unknown} (written for Rocky/Alma/RHEL 9; the install script is dnf-based)";;
esac
CPUS=$(nproc 2>/dev/null || echo 1)
[ "$CPUS" -ge 2 ] && ok "CPU cores: $CPUS" || fail "CPU cores: $CPUS (need >= 2)"
MEM_KB=$(awk '/MemTotal/{print $2}' /proc/meminfo)
MEM_GB=$((MEM_KB/1024/1024))
if   [ "$MEM_KB" -ge 7500000 ]; then ok "Memory: ~${MEM_GB} GB"
elif [ "$MEM_KB" -ge 5800000 ]; then warn "Memory: ~${MEM_GB} GB (works for small setups; 8 GB+ recommended for ClickHouse + Elasticsearch)"
else fail "Memory: ~${MEM_GB} GB (ClickHouse + Elasticsearch need at least 6 GB, 8 GB recommended)"; fi
DIR=/var/lib/docker; [ -d "$DIR" ] || DIR=/var
FREE_GB=$(df -PBG "$DIR" | awk 'NR==2{gsub("G","",$4);print $4}')
if   [ "$FREE_GB" -ge 50 ]; then ok "Free disk on $DIR: ${FREE_GB} GB"
elif [ "$FREE_GB" -ge 20 ]; then warn "Free disk on $DIR: ${FREE_GB} GB (fine to start; size for retention x ingest volume)"
else fail "Free disk on $DIR: ${FREE_GB} GB (need >= 20 GB)"; fi
FST=$(df -PT "$DIR" | awk 'NR==2{print $2}')
case "$FST" in nfs*|cifs|smb*) warn "Data dir is on $FST: ClickHouse needs fast local disk, not network storage";; *) ok "Data dir filesystem: $FST";; esac
NOFILE=$(ulimit -n); [ "$NOFILE" -ge 65536 ] && ok "open files limit: $NOFILE" || warn "open files limit: $NOFILE (compose sets 262144 for the ClickHouse container)"

MMC=$(cat /proc/sys/vm/max_map_count 2>/dev/null || echo 0)
if [ "$MMC" -ge 262144 ]; then ok "vm.max_map_count: $MMC (Elasticsearch needs >= 262144)"
else fail "vm.max_map_count is $MMC, Elasticsearch needs >= 262144. Fix: sudo sysctl -w vm.max_map_count=262144 && echo 'vm.max_map_count=262144' | sudo tee /etc/sysctl.d/99-lumen.conf"; fi

echo "Time"
if have timedatectl && [ "$(timedatectl show -p NTPSynchronized --value 2>/dev/null)" = "yes" ]; then
  ok "Clock is NTP-synchronised"
else
  warn "Clock not confirmed NTP-synchronised: telemetry timestamps and trace ordering depend on it (dnf install chrony; systemctl enable --now chronyd)"
fi

echo "Container runtime"
if have docker; then
  ok "docker: $(docker --version 2>/dev/null)"
  if docker info >/dev/null 2>&1; then
    ok "docker daemon reachable"
    docker info 2>/dev/null | grep -qi 'rootless' && warn "Docker is rootless: works, but ports < 1024 and some networking need extra setup" || ok "docker mode: rootful"
  else fail "docker daemon not reachable (systemctl start docker; is your user in the docker group?)"; fi
  docker compose version >/dev/null 2>&1 && ok "docker compose v2 plugin present" || fail "docker compose v2 plugin missing (dnf install docker-compose-plugin)"
else { have podman && warn "podman found: docker-ce install needs --allow-podman-removal (see install script)"; fail "docker not installed (run deploy/install-rhel-family.sh)"; }; fi

echo "Network"
if have ss; then
  for p in "$PORT" 8123; do
    if ss -ltn "( sport = :$p )" 2>/dev/null | grep -q LISTEN; then
      [ "$p" = 8123 ] && warn "port $p in use (ClickHouse is not published to the host by default, so this is fine unless you changed that)" || fail "port $p already in use"
    else ok "port $p free"; fi
  done
else warn "ss not found, skipped port checks"; fi
if have firewall-cmd && systemctl is-active --quiet firewalld 2>/dev/null; then
  firewall-cmd --query-port="$PORT/tcp" >/dev/null 2>&1 && ok "firewalld: $PORT/tcp open" \
    || warn "firewalld: $PORT/tcp not opened. Docker-published ports are normally reachable regardless of firewalld, so do not rely on it to restrict access: set LUMEN_BIND=127.0.0.1 and use a TLS proxy"
else ok "firewalld not active"; fi
if have getenforce; then
  case "$(getenforce)" in Enforcing) warn "SELinux enforcing: fine, but bind-mounts need :z/:Z (the shipped compose uses named volumes, so it is unaffected)";; *) ok "SELinux: $(getenforce)";; esac
fi
if have curl; then
  curl -fsS -m 5 -o /dev/null https://registry-1.docker.io/v2/ 2>/dev/null || [ "$(curl -s -m 5 -o /dev/null -w '%{http_code}' https://registry-1.docker.io/v2/)" = 401 ] \
    && ok "can reach Docker Hub (needed to pull ClickHouse)" \
    || warn "cannot reach Docker Hub (behind a proxy? configure docker's proxy or pre-load the images)"
fi

echo "Configuration"
if [ -f "$HERE/.env" ]; then
  ok "deploy/.env exists"
  [ "$(stat -c %a "$HERE/.env")" = 600 ] && ok ".env permissions 600" || warn ".env should be chmod 600"
  grep -q '^ELASTIC_PASSWORD=.\+' "$HERE/.env" && ok "Elasticsearch password configured" || fail "ELASTIC_PASSWORD missing in .env (run deploy/gen-env.sh or use dev.sh / install.sh)"
  grep -q '^LUMEN_SECRET_KEY=.\+' "$HERE/.env" && ok "secret key configured" || fail "LUMEN_SECRET_KEY missing in .env (run deploy/gen-env.sh, or dev.sh / install.sh which add it)"
  grep -q '^LUMEN_ADMIN_PASSWORD=.\+' "$HERE/.env" && ok "admin login configured" || warn "no LUMEN_ADMIN_PASSWORD in .env: create a user with: lumen users add NAME --tenant TENANT"
else warn "deploy/.env missing: run deploy/gen-env.sh"; fi

echo
if [ "$FAIL" -gt 0 ]; then printf 'Result: %d FAIL, %d WARN. Fix the failures first.\n' "$FAIL" "$WARN"; exit 1; fi
printf 'Result: ready (%d warnings).\n' "$WARN"
