#!/usr/bin/env bash
# Installs Docker Engine + compose plugin + chrony on Rocky Linux / AlmaLinux / RHEL 9.
# Run as root:  sudo ./install-rhel-family.sh [--allow-podman-removal] [--open-port]
#
# Rocky/Alma/RHEL ship podman + runc. Docker's containerd.io replaces runc, so dnf must remove
# podman/buildah to proceed. That is destructive if you use podman, so it needs an explicit flag.
set -euo pipefail
[ "$(id -u)" -eq 0 ] || { echo "run as root (sudo)"; exit 1; }
. /etc/os-release
case "$ID" in
  rocky|almalinux|centos) REPO=centos;;
  rhel) REPO=rhel;;
  *) echo "this script targets the RHEL family, found: $ID"; exit 1;;
esac
ALLOW_ERASE=""; OPEN_PORT=0
for a in "$@"; do
  case "$a" in
    --allow-podman-removal) ALLOW_ERASE="--allowerasing";;
    --open-port) OPEN_PORT=1;;
    *) echo "unknown option: $a"; exit 1;;
  esac
done

dnf -y install dnf-plugins-core curl openssl
if ! command -v docker >/dev/null; then
  if rpm -q podman >/dev/null 2>&1 && [ -z "$ALLOW_ERASE" ]; then
    echo "podman is installed and conflicts with docker-ce's containerd.io (runc)."
    echo "Re-run with --allow-podman-removal to let dnf remove podman/buildah,"
    echo "or keep podman and run Lumen with podman-compose instead."
    exit 1
  fi
  dnf config-manager --add-repo "https://download.docker.com/linux/${REPO}/docker-ce.repo"
  dnf -y install $ALLOW_ERASE docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
fi
systemctl enable --now docker
# Elasticsearch needs a larger memory-map limit on the host
echo 'vm.max_map_count=262144' > /etc/sysctl.d/99-lumen.conf
sysctl -w vm.max_map_count=262144 >/dev/null
dnf -y install chrony && systemctl enable --now chronyd

if [ "$OPEN_PORT" = 1 ] && systemctl is-active --quiet firewalld; then
  firewall-cmd --permanent --add-port=4318/tcp && firewall-cmd --reload
  echo "opened 4318/tcp in firewalld"
fi
docker --version; docker compose version
echo "Done. Next: ./deploy/gen-env.sh && ./deploy/preflight.sh && make up"
