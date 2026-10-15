#!/usr/bin/env bash
# Installs Docker Engine + compose plugin + chrony on Ubuntu or Debian.
# Run as root:  sudo ./install-debian-family.sh [--open-port]
#
# It uses Docker's own apt repository. If Docker has not published packages for your release yet (a brand new
# Ubuntu release, for example), Ubuntu's own docker.io packages are used instead.
set -euo pipefail
[ "$(id -u)" -eq 0 ] || { echo "run as root (sudo)"; exit 1; }
. /etc/os-release
case "$ID" in
  ubuntu) CODENAME="${UBUNTU_CODENAME:-$VERSION_CODENAME}";;
  debian) CODENAME="$VERSION_CODENAME";;
  *) echo "this script targets Ubuntu and Debian, found: $ID"; exit 1;;
esac
OPEN_PORT=0
for a in "$@"; do
  case "$a" in
    --open-port) OPEN_PORT=1;;
    --allow-podman-removal) ;; # only meaningful on the RHEL family; accepted so install.sh can pass its flags on
    *) echo "unknown option: $a"; exit 1;;
  esac
done

export DEBIAN_FRONTEND=noninteractive
apt-get update -y
apt-get install -y ca-certificates curl gnupg openssl

if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
  # Packages that conflict with Docker's own (the Docker docs list these). Only touch them if they are installed.
  OLD="$(dpkg --get-selections docker.io docker-compose docker-compose-v2 docker-doc podman-docker containerd runc 2>/dev/null | awk '{print $1}' | tr '\n' ' ')"
  install -m 0755 -d /etc/apt/keyrings
  curl -fsSL "https://download.docker.com/linux/${ID}/gpg" -o /etc/apt/keyrings/docker.asc
  chmod a+r /etc/apt/keyrings/docker.asc
  cat > /etc/apt/sources.list.d/docker.sources <<EOF
Types: deb
URIs: https://download.docker.com/linux/${ID}
Suites: ${CODENAME}
Components: stable
Signed-By: /etc/apt/keyrings/docker.asc
EOF
  apt-get update -y || true
  if apt-cache policy docker-ce 2>/dev/null | grep -q 'Candidate: [0-9]'; then
    if [ -n "$OLD" ]; then echo "Removing packages that conflict with Docker's own: $OLD"; apt-get remove -y $OLD; fi
    apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
  else
    echo "Docker has no packages for '${CODENAME}' yet. Falling back to the distribution's own packages."
    rm -f /etc/apt/sources.list.d/docker.sources
    apt-get update -y
    if [ "$ID" = ubuntu ]; then apt-get install -y docker.io docker-compose-v2 docker-buildx
    else echo "No fallback for ${ID} ${CODENAME}: install Docker Engine by hand, see https://docs.docker.com/engine/install/"; exit 1; fi
  fi
fi
systemctl enable --now docker
# Elasticsearch needs a larger memory-map limit on the host
echo 'vm.max_map_count=262144' > /etc/sysctl.d/99-lumen.conf
sysctl -w vm.max_map_count=262144 >/dev/null
apt-get install -y chrony && systemctl enable --now chrony

if [ "$OPEN_PORT" = 1 ] && command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q 'Status: active'; then
  ufw allow 4318/tcp
  echo "opened 4318/tcp in ufw"
fi
docker --version; docker compose version
echo "Done. Next: ./deploy/gen-env.sh && ./deploy/preflight.sh && make up (or run ./install.sh)"
