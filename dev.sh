#!/usr/bin/env bash
# Try Lumen on your own machine, reachable from other VMs on the same network. No root, nothing installed
# except what Docker pulls. Needs Docker with the compose plugin.
#
#   ./dev.sh                 start (auto-detects this machine's LAN IP)
#   ./dev.sh --ip 192.168.56.1   use a specific IP (e.g. the host-only adapter your VM can see)
#   Re-running keeps the address and settings already in deploy/.env; --ip changes the address.
#   ./dev.sh status | stop | reset      (reset also deletes all stored data and secrets)
set -euo pipefail
cd "$(dirname "$0")"
COMPOSE=(docker compose -f deploy/docker-compose.yml --env-file deploy/.env)
die() { echo "error: $*" >&2; exit 1; }
IP=""; CMD=start
while [ $# -gt 0 ]; do
  case "$1" in
    --ip) [ $# -ge 2 ] || die "--ip needs a value"; IP="$2"; shift 2;;
    start|stop|status|reset) CMD="$1"; shift;;
    -h|--help) sed -n '2,10p' "$0"; exit 0;;
    *) die "unknown option: $1";;
  esac
done
check_docker() {
  command -v docker >/dev/null 2>&1 || die "Docker is not installed.
  Rocky/Alma/RHEL:  sudo ./deploy/install-rhel-family.sh --allow-podman-removal
  Mac/Windows:      install Docker Desktop (it includes compose)
  Ubuntu/Debian:    follow https://docs.docker.com/engine/install/ and install docker-compose-plugin"
  docker compose version >/dev/null 2>&1 || die "Docker is installed but the compose plugin is missing (the old 'docker-compose' command or a podman 'docker' shim do not work).
  Rocky/Alma/RHEL:  sudo ./deploy/install-rhel-family.sh --allow-podman-removal
  or add Docker's repo and: sudo dnf install docker-compose-plugin"
  docker info >/dev/null 2>&1 || die "Cannot talk to the Docker daemon.
  Start it:            sudo systemctl start docker
  Or allow your user:  sudo usermod -aG docker \$USER   (then log out and back in), or run this script with sudo"
}
check_docker

case "$CMD" in
  stop)   "${COMPOSE[@]}" down; exit 0;;
  reset)  read -r -p "Delete all Lumen data and secrets? [y/N] " a; [ "$a" = y ] || exit 0
          "${COMPOSE[@]}" down -v; rm -f deploy/.env; exit 0;;
  status) "${COMPOSE[@]}" ps; curl -fsS -m 3 http://127.0.0.1:4318/healthz && echo " (healthy)"; exit 0;;
esac

# Elasticsearch needs vm.max_map_count >= 262144 on a Linux host (Docker Desktop on Mac/Windows handles this itself)
if [ -r /proc/sys/vm/max_map_count ] && [ -z "${LUMEN_SKIP_SYSCTL_CHECK:-}" ] && [ "$(cat /proc/sys/vm/max_map_count)" -lt 262144 ]; then
  echo "Elasticsearch needs vm.max_map_count=262144 (now $(cat /proc/sys/vm/max_map_count)). Setting it (sudo may ask for your password):"
  echo "  sudo sysctl -w vm.max_map_count=262144"
  echo "  echo 'vm.max_map_count=262144' | sudo tee /etc/sysctl.d/99-lumen.conf"
  if sudo sysctl -w vm.max_map_count=262144 && echo 'vm.max_map_count=262144' | sudo tee /etc/sysctl.d/99-lumen.conf >/dev/null; then
    echo "  done."
  else
    die "could not set vm.max_map_count. Run the two commands above as root, then run ./dev.sh again."
  fi
fi

# ---- pick the address other machines will use ----
detect_ip() {
  if command -v hostname >/dev/null 2>&1 && hostname -I >/dev/null 2>&1; then
    for a in $(hostname -I); do case "$a" in 172.17.*|127.*|*:*) ;; *) echo "$a"; return;; esac; done
  fi
  for i in en0 en1; do ipconfig getifaddr "$i" 2>/dev/null && return; done
  return 1
}
# An address already set in deploy/.env is kept; --ip is the explicit way to change it.
EXISTING_URL="$(grep '^LUMEN_PUBLIC_URL=' deploy/.env 2>/dev/null | head -n1 | cut -d= -f2- || true)"
if [ -n "$IP" ]; then
  printf '%s' "$IP" | grep -Eq '^[0-9A-Za-z.-]+$' || die "invalid --ip"
  URL="http://$IP:4318"
elif [ -n "$EXISTING_URL" ]; then
  URL="$EXISTING_URL"
  echo "Keeping the existing LUMEN_PUBLIC_URL=$URL (use --ip to change it)."
else
  IP="$(detect_ip || true)"
  [ -n "$IP" ] || die "could not detect this machine's IP. Re-run with: ./dev.sh --ip <address your VM can reach>"
  URL="http://$IP:4318"
fi

# ---- secrets + settings ----
[ -f deploy/.env ] || bash deploy/gen-env.sh dev >/dev/null
set_env() { if grep -q "^$1=" deploy/.env; then sed -i.bak "s#^$1=.*#$1=$2#" deploy/.env && rm -f deploy/.env.bak; else echo "$1=$2" >> deploy/.env; fi; }
TENANT=dev
if ! grep -q '^LUMEN_ADMIN_PASSWORD=' deploy/.env; then
  printf 'LUMEN_ADMIN_USER=admin\nLUMEN_ADMIN_PASSWORD=%s\nLUMEN_ADMIN_TENANT=%s\n' "$(openssl rand -hex 12)" "${TENANT:-main}" >> deploy/.env
fi
grep -q '^ELASTIC_PASSWORD=' deploy/.env || printf 'ELASTIC_PASSWORD=%s\n' "$(openssl rand -hex 16)" >> deploy/.env
grep -q '^LUMEN_SECRET_KEY=' deploy/.env || printf 'LUMEN_SECRET_KEY=%s\n' "$(openssl rand -hex 32)" >> deploy/.env
ADMIN_USER="$(grep '^LUMEN_ADMIN_USER=' deploy/.env | cut -d= -f2-)"; ADMIN_PW="$(grep '^LUMEN_ADMIN_PASSWORD=' deploy/.env | cut -d= -f2-)"
[ "$URL" = "$EXISTING_URL" ] || set_env LUMEN_PUBLIC_URL "$URL"
grep -q '^LUMEN_BIND=' deploy/.env || set_env LUMEN_BIND "0.0.0.0"   # a value you set (for example 127.0.0.1 behind a proxy) is kept
KEY="$(grep '^LUMEN_API_KEYS=' deploy/.env | cut -d= -f2- | cut -d, -f1 | cut -d: -f1)"

echo "Starting Lumen (first start builds the image and can take a few minutes)..."
"${COMPOSE[@]}" up -d --build
for i in $(seq 1 60); do
  curl -fsS -m 3 http://127.0.0.1:4318/healthz >/dev/null 2>&1 && break
  [ "$i" = 60 ] && { "${COMPOSE[@]}" logs --tail 30; die "Lumen did not become healthy"; }
  sleep 3
done
curl -fsS -m 3 "$URL/healthz" >/dev/null 2>&1 && REACH="reachable on $URL" || REACH="NOT reachable on $URL from this machine (wrong --ip?)"

cat <<MSG

Lumen is running ($REACH).

  Open in a browser:  $URL/
  Log in with:        $ADMIN_USER  /  $ADMIN_PW      (change the password in the UI)
  Agent key:          $KEY     (or create keys in the UI under "Add a machine")

Test from your other VM (should print: ok):
  curl -s $URL/healthz

Install an agent on that VM:
  Linux:    curl -fsSL $URL/install/agent.sh | sudo sh -s -- --key $KEY
  Windows:  & ([scriptblock]::Create((irm $URL/install/agent.ps1))) -Key $KEY
  Docker:   see "Add a machine" in the web UI

If the VM cannot connect:
  * Use an IP the VM can reach. Bridged networking: your LAN IP. NAT/host-only: the host address of that virtual network
    (VirtualBox host-only is often 192.168.56.1); pass it with --ip.
  * Allow inbound TCP 4318 on this machine's firewall (Docker-published ports are normally open on Linux;
    Windows/macOS firewalls may ask or need a rule).
Stop with ./dev.sh stop
MSG
