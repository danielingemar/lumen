#!/bin/sh
# Lumen agent installer for Linux (systemd).
#
#   curl -fsSL __LUMEN_URL__/install/agent.sh | sudo sh -s -- --key YOUR_API_KEY
#
# Options:
#   --key KEY         API key (or set LUMEN_KEY)          [required]
#   --logs GLOB       also ship log files, e.g. '/var/log/*.log' (repeatable)
#   --docker          report Docker containers (up/down): adds the agent to the "docker" group, which is root-equivalent
#   --docker-logs     also ship Docker container logs
#   --no-host-metrics do not collect CPU/memory/disk of this machine
#   --url URL         override the Lumen URL (default: __LUMEN_URL__)
#   Nextcloud monitoring (polls status.php + the serverinfo API of one instance):
#   --nextcloud URL --nextcloud-token TOKEN [--nextcloud-name NAME] [--nextcloud-log /path/nextcloud.log]
#   (or --nextcloud-user USER --nextcloud-password APP_PASSWORD instead of a token)
#   --uninstall       remove the agent
set -eu

LUMEN_URL="__LUMEN_URL__"
BIN_DIR="${LUMEN_BIN_DIR:-/usr/local/bin}"
CONF_DIR="${LUMEN_CONF_DIR:-/etc/lumen-agent}"
NO_SERVICE="${LUMEN_NO_SERVICE:-0}"   # for containers/tests: skip user + systemd
KEY="${LUMEN_KEY:-}"; LOGS=""; DOCKER=0; DOCKERGRP=0; UNINSTALL=0
HOSTM=true
NC_URL=""; NC_TOKEN=""; NC_USER=""; NC_PASS=""; NC_NAME="nextcloud"; NC_LOG=""

die() { echo "error: $*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --key) [ $# -ge 2 ] || die "--key needs a value"; KEY="$2"; shift 2;;
    --logs) [ $# -ge 2 ] || die "--logs needs a value"; LOGS="$LOGS
$2"; shift 2;;
    --docker-logs) DOCKER=1; shift;;
    --docker) DOCKERGRP=1; shift;;
    --no-host-metrics) HOSTM=false; shift;;
    --nextcloud) [ $# -ge 2 ] || die "--nextcloud needs a URL"; NC_URL="$2"; shift 2;;
    --nextcloud-token) [ $# -ge 2 ] || die "--nextcloud-token needs a value"; NC_TOKEN="$2"; shift 2;;
    --nextcloud-user) [ $# -ge 2 ] || die "--nextcloud-user needs a value"; NC_USER="$2"; shift 2;;
    --nextcloud-password) [ $# -ge 2 ] || die "--nextcloud-password needs a value"; NC_PASS="$2"; shift 2;;
    --nextcloud-name) [ $# -ge 2 ] || die "--nextcloud-name needs a value"; NC_NAME="$2"; shift 2;;
    --nextcloud-log) [ $# -ge 2 ] || die "--nextcloud-log needs a path"; NC_LOG="$2"; shift 2;;
    --url) [ $# -ge 2 ] || die "--url needs a value"; LUMEN_URL="$2"; shift 2;;
    --uninstall) UNINSTALL=1; shift;;
    -h|--help) sed -n '2,15p' "$0" 2>/dev/null || true; exit 0;;
    *) die "unknown option: $1";;
  esac
done

[ "$(id -u)" -eq 0 ] || die "run as root, e.g. ... | sudo sh -s -- --key KEY"

if [ "$UNINSTALL" = 1 ]; then
  if [ "$NO_SERVICE" != 1 ] && command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now lumen-agent 2>/dev/null || true
    rm -f /etc/systemd/system/lumen-agent.service; systemctl daemon-reload
    userdel lumen-agent 2>/dev/null || true
  fi
  rm -f "$BIN_DIR/lumen-agent"; rm -rf "$CONF_DIR"
  echo "Lumen agent removed."; exit 0
fi

[ -n "$KEY" ] || die "an API key is required (--key KEY)"
printf '%s' "$KEY" | grep -Eq '^[A-Za-z0-9_.-]+$' || die "the API key contains unexpected characters"
printf '%s' "$LUMEN_URL" | grep -Eq '^https?://[A-Za-z0-9.:-]+$' || die "invalid Lumen URL: $LUMEN_URL"
if [ -n "$NC_URL" ]; then
  printf '%s' "$NC_URL" | grep -Eq '^https?://[A-Za-z0-9.:/_-]+$' || die "invalid --nextcloud URL"
  for v in "$NC_TOKEN" "$NC_USER" "$NC_PASS" "$NC_NAME"; do
    [ -z "$v" ] || printf '%s' "$v" | grep -Eq '^[A-Za-z0-9_.-]+$' || die "nextcloud token/user/password/name may only contain letters, digits, _ . -"
  done
  case "$NC_LOG" in *\"*|*\\*) die "nextcloud log path must not contain quotes or backslashes";; esac
  [ -n "$NC_TOKEN" ] || [ -n "$NC_USER" ] || echo "note: no --nextcloud-token/--nextcloud-user given, only availability (status.php) will be monitored" >&2
fi
command -v curl >/dev/null 2>&1 || die "curl is required"
[ "$NO_SERVICE" = 1 ] || command -v systemctl >/dev/null 2>&1 || die "systemd is required"

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64;;
  aarch64|arm64) ARCH=arm64;;
  *) die "unsupported CPU architecture: $(uname -m)";;
esac
NAME="lumen-agent-linux-$ARCH"

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
echo "Downloading $NAME from $LUMEN_URL ..."
curl -fsSL "$LUMEN_URL/download/$NAME" -o "$TMP/$NAME" || die "download failed (is $LUMEN_URL reachable from this machine?)"
curl -fsSL "$LUMEN_URL/download/SHA256SUMS" -o "$TMP/SHA256SUMS" || die "could not download checksums"
WANT="$(grep " $NAME\$" "$TMP/SHA256SUMS" | awk '{print $1}')"
HAVE="$(sha256sum "$TMP/$NAME" | awk '{print $1}')"
[ -n "$WANT" ] && [ "$WANT" = "$HAVE" ] || die "checksum mismatch: the download is corrupt, refusing to install"

# stop a running agent so the binary can be replaced (upgrade)
if [ "$NO_SERVICE" != 1 ]; then systemctl stop lumen-agent 2>/dev/null || true; fi
mkdir -p "$BIN_DIR" "$CONF_DIR"
install -m 0755 "$TMP/$NAME" "$BIN_DIR/lumen-agent"

if [ "$NO_SERVICE" != 1 ]; then
  id lumen-agent >/dev/null 2>&1 || useradd --system --no-create-home --shell "$(command -v nologin || echo /sbin/nologin)" lumen-agent
fi

# ---- config ----
LOGS_JSON=""
add_log() { # $1 glob $2 service $3 format
  case "$1" in *\"*|*\\*) die "log path must not contain quotes or backslashes: $1";; esac
  LOGS_JSON="${LOGS_JSON}${LOGS_JSON:+,}{\"paths\":[\"$1\"],\"service\":\"$2\",\"format\":\"$3\"}"
}
# globbing must stay off here: '/var/log/*.log' is a pattern for the agent to expand later, not now
set -f
OLDIFS="$IFS"; IFS='
'
for g in $LOGS; do [ -n "$g" ] && add_log "$g" "system-logs" "text"; done
IFS="$OLDIFS"
set +f
[ "$DOCKER" = 1 ] && add_log "/var/lib/docker/containers/*/*-json.log" "docker" "docker"
NC_JSON=""
if [ -n "$NC_URL" ]; then
  NC_JSON="{\"url\":\"$NC_URL\",\"service\":\"$NC_NAME\",\"token\":\"$NC_TOKEN\",\"username\":\"$NC_USER\",\"password\":\"$NC_PASS\",\"log_path\":\"$NC_LOG\"}"
fi
umask 077
cat > "$CONF_DIR/config.json" <<CFG
{
  "url": "$LUMEN_URL",
  "api_key": "$KEY",
  "interval_seconds": 15,
  "host_metrics": $HOSTM,
  "self_metrics": true,
  "logs": [${LOGS_JSON}],
  "nextcloud": [${NC_JSON}]
}
CFG
if [ "$NO_SERVICE" != 1 ]; then chown root:lumen-agent "$CONF_DIR/config.json"; chmod 640 "$CONF_DIR/config.json"; fi

if [ "$NO_SERVICE" = 1 ]; then echo "Installed to $BIN_DIR (service setup skipped)."; exit 0; fi

# Log paths can be added later in the Lumen web UI, so the agent always gets the read-only capability to read other
# users' files (it cannot write). The agent itself refuses paths outside /var/log, Docker's data dirs, /var/www,
# /srv, /mnt and /opt that come from the server (see allowed_log_dirs).
CAP="AmbientCapabilities=CAP_DAC_READ_SEARCH
CapabilityBoundingSet=CAP_DAC_READ_SEARCH"
if [ "$DOCKERGRP" = 1 ]; then
  getent group docker >/dev/null 2>&1 || die "--docker needs Docker installed (no 'docker' group found)"
  CAP="$CAP
SupplementaryGroups=docker"
fi
cat > /etc/systemd/system/lumen-agent.service <<UNIT
[Unit]
Description=Lumen agent
After=network-online.target
Wants=network-online.target

[Service]
User=lumen-agent
ExecStart=$BIN_DIR/lumen-agent -config $CONF_DIR/config.json
Restart=always
RestartSec=5
NoNewPrivileges=true
ProtectSystem=strict
PrivateTmp=true
$CAP

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now lumen-agent
sleep 2
if systemctl is-active --quiet lumen-agent; then
  echo "Lumen agent installed and running. Data should appear in Lumen within a minute."
  echo "Logs: journalctl -u lumen-agent -f    Remove: re-run with --uninstall"
else
  echo "The agent did not start. Check: journalctl -u lumen-agent -n 50" >&2; exit 1
fi
