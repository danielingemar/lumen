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
#   --update          update the agent to this server's version and keep its settings (no key needed)
#   --uninstall       remove the agent
set -eu

LUMEN_URL="__LUMEN_URL__"
BIN_DIR="${LUMEN_BIN_DIR:-/usr/local/bin}"
CONF_DIR="${LUMEN_CONF_DIR:-/etc/lumen-agent}"
LIB_DIR="${LUMEN_LIB_DIR:-/usr/local/lib/lumen-agent}"      # the update helper (root-owned)
STATE_DIR="${LUMEN_STATE_DIR:-/var/lib/lumen-agent}"        # where the agent may leave an update request (systemd creates it)
SYSTEMD_DIR="${LUMEN_SYSTEMD_DIR:-/etc/systemd/system}"
UPDATE=0
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
    --update) UPDATE=1; shift;;
    --uninstall) UNINSTALL=1; shift;;
    -h|--help) sed -n '2,15p' "$0" 2>/dev/null || true; exit 0;;
    *) die "unknown option: $1";;
  esac
done

[ "$(id -u)" -eq 0 ] || die "run as root, e.g. ... | sudo sh -s -- --key KEY"

if [ "$UNINSTALL" = 1 ]; then
  if [ "$NO_SERVICE" != 1 ] && command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now lumen-agent-update.path 2>/dev/null || true
    systemctl disable --now lumen-agent 2>/dev/null || true
    rm -f "$SYSTEMD_DIR/lumen-agent.service" "$SYSTEMD_DIR/lumen-agent-update.path" "$SYSTEMD_DIR/lumen-agent-update.service"; systemctl daemon-reload
    userdel lumen-agent 2>/dev/null || true
  fi
  rm -f "$BIN_DIR/lumen-agent" "$BIN_DIR/lumen-agent.previous"; rm -rf "$CONF_DIR" "$LIB_DIR" "$STATE_DIR"
  echo "Lumen agent removed."; exit 0
fi

if [ "$UPDATE" = 1 ]; then
  [ -f "$CONF_DIR/config.json" ] || die "there is no agent installed here to update ($CONF_DIR/config.json is missing): do a first install with --key KEY"
  [ -z "$KEY" ] || die "--update keeps the existing settings and key; use it without --key (to change the key, install again with --key)"
else
  [ -n "$KEY" ] || die "an API key is required (--key KEY)"
  printf '%s' "$KEY" | grep -Eq '^[A-Za-z0-9_.-]+$' || die "the API key contains unexpected characters"
fi
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

# the update helper: root-owned, started by systemd when the agent (which runs unprivileged and cannot replace its own
# file) leaves a request. It installs only what this Lumen server offers, checked against the server's checksums.
install_updater() {
  mkdir -p "$LIB_DIR"
  cat > "$LIB_DIR/apply-update.conf" <<CONFIG
BIN_DIR="$BIN_DIR"
CONF_DIR="$CONF_DIR"
STATE_DIR="$STATE_DIR"
CONFIG
  cat > "$LIB_DIR/apply-update.sh" <<'HELPER'
#!/bin/sh
# Lumen agent update helper. Runs as root, started by systemd when the agent asks for an update.
# It trusts nothing from the agent except "please update": what gets installed is whatever the Lumen server in the
# (root-owned) config offers, and only if it matches the server's checksums. If the new version does not stay up,
# the old one is put back.
set -eu
. "$(dirname "$0")/apply-update.conf"
FLAG="$STATE_DIR/update-requested"
log() { echo "lumen-agent-update: $*"; if command -v logger >/dev/null 2>&1; then logger -t lumen-agent-update -- "$*" || true; fi; }
fail() { log "FAILED: $*"; exit 1; }
[ -f "$FLAG" ] || exit 0
rm -f "$FLAG"   # first of all, so that the path unit does not start us again
URL="$(sed -n 's/.*"url": *"\([^"]*\)".*/\1/p' "$CONF_DIR/config.json" | head -1)"
printf '%s' "$URL" | grep -Eq '^https?://[A-Za-z0-9.:-]+$' || fail "cannot read a valid Lumen address from $CONF_DIR/config.json"
case "$(uname -m)" in x86_64|amd64) ARCH=amd64;; aarch64|arm64) ARCH=arm64;; *) fail "unsupported CPU architecture";; esac
NAME="lumen-agent-linux-$ARCH"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
curl -fsSL --max-time 300 "$URL/download/$NAME" -o "$TMP/$NAME" || fail "download from $URL failed"
curl -fsSL --max-time 60 "$URL/download/SHA256SUMS" -o "$TMP/SHA256SUMS" || fail "could not download the checksums"
WANT="$(grep " $NAME\$" "$TMP/SHA256SUMS" | awk '{print $1}')"
HAVE="$(sha256sum "$TMP/$NAME" | awk '{print $1}')"
[ -n "$WANT" ] && [ "$WANT" = "$HAVE" ] || fail "checksum mismatch: the download is corrupt, refusing to install"
CUR="$(sha256sum "$BIN_DIR/lumen-agent" 2>/dev/null | awk '{print $1}')"
if [ "$CUR" = "$HAVE" ]; then log "this is already the version the server offers: nothing to do"; exit 0; fi
chmod +x "$TMP/$NAME"
NEWV="$("$TMP/$NAME" -version 2>/dev/null)" || fail "the downloaded agent does not run on this machine"
OLDV="$("$BIN_DIR/lumen-agent" -version 2>/dev/null || echo unknown)"
cp -p "$BIN_DIR/lumen-agent" "$BIN_DIR/lumen-agent.previous" 2>/dev/null || true
install -m 0755 "$TMP/$NAME" "$BIN_DIR/lumen-agent.new"
mv -f "$BIN_DIR/lumen-agent.new" "$BIN_DIR/lumen-agent"
log "installed $NEWV (was $OLDV); restarting the agent"
${LUMEN_APPLY_RESTART_CMD:-systemctl restart lumen-agent} || true
healthy() {
  if [ -n "${LUMEN_APPLY_CHECK_CMD:-}" ]; then sh -c "$LUMEN_APPLY_CHECK_CMD"; return; fi
  systemctl is-active --quiet lumen-agent
}
stamp() { systemctl show -p ActiveEnterTimestampMonotonic --value lumen-agent 2>/dev/null || echo 0; }
ok=0; i=0
while [ "$i" -lt 30 ]; do if healthy; then ok=1; break; fi; i=$((i+1)); sleep 1; done
if [ "$ok" = 1 ]; then
  S1="$(stamp)"; sleep "${LUMEN_APPLY_WAIT:-12}"
  if ! healthy || [ "$(stamp)" != "$S1" ]; then ok=0; fi   # up, but restarting again and again, counts as not staying up
fi
if [ "$ok" != 1 ]; then
  if [ -f "$BIN_DIR/lumen-agent.previous" ]; then
    mv -f "$BIN_DIR/lumen-agent.previous" "$BIN_DIR/lumen-agent"
    ${LUMEN_APPLY_RESTART_CMD:-systemctl restart lumen-agent} || true
    fail "the new version ($NEWV) did not stay up; the previous version ($OLDV) was put back"
  fi
  fail "the new version ($NEWV) did not stay up and there is no previous version to put back"
fi
log "the agent runs $NEWV"
HELPER
  chmod 0755 "$LIB_DIR/apply-update.sh"; chmod 0644 "$LIB_DIR/apply-update.conf"
  cat > "$SYSTEMD_DIR/lumen-agent-update.path" <<UNIT
[Unit]
Description=Lumen agent update request

[Path]
PathExists=$STATE_DIR/update-requested
Unit=lumen-agent-update.service

[Install]
WantedBy=multi-user.target
UNIT
  cat > "$SYSTEMD_DIR/lumen-agent-update.service" <<UNIT
[Unit]
Description=Lumen agent update

[Service]
Type=oneshot
ExecStart=$LIB_DIR/apply-update.sh
UNIT
}

if [ "$UPDATE" = 1 ]; then
  UNIT="$SYSTEMD_DIR/lumen-agent.service"
  if [ "$NO_SERVICE" != 1 ]; then
    [ -f "$UNIT" ] || die "$UNIT is missing: the agent was not installed by this script, so it is not updated here"
    # bring an older install up to date without touching anything else in its unit (the docker group, for example)
    grep -q '^StateDirectory=' "$UNIT" || sed -i '/^Restart=always/a StateDirectory=lumen-agent' "$UNIT"
    grep -q '^Environment=LUMEN_AGENT_SELF_UPDATE=' "$UNIT" || sed -i "/^Restart=always/a Environment=LUMEN_AGENT_SELF_UPDATE=systemd:$STATE_DIR" "$UNIT"
  fi
  if [ "$NO_SERVICE" != 1 ] || [ "${LUMEN_TEST_UPDATER:-0}" = 1 ]; then install_updater; fi
  if [ "$NO_SERVICE" = 1 ]; then echo "Updated $BIN_DIR/lumen-agent (service steps skipped)."; exit 0; fi
  systemctl daemon-reload
  systemctl enable --now lumen-agent-update.path
  systemctl restart lumen-agent
  sleep 2
  if systemctl is-active --quiet lumen-agent; then
    echo "Lumen agent updated to $("$BIN_DIR/lumen-agent" -version 2>/dev/null || echo the new version) and running. Its settings were kept."
    echo "From now on it can be updated with one click in Lumen (Hosts, the update label)."
    exit 0
  fi
  echo "The agent did not start. Check: journalctl -u lumen-agent -n 50" >&2; exit 1
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

if [ "$NO_SERVICE" = 1 ]; then
  if [ "${LUMEN_TEST_UPDATER:-0}" = 1 ]; then install_updater; fi
  echo "Installed to $BIN_DIR (service setup skipped)."; exit 0
fi

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
cat > "$SYSTEMD_DIR/lumen-agent.service" <<UNIT
[Unit]
Description=Lumen agent
After=network-online.target
Wants=network-online.target

[Service]
User=lumen-agent
ExecStart=$BIN_DIR/lumen-agent -config $CONF_DIR/config.json
Restart=always
RestartSec=5
StateDirectory=lumen-agent
Environment=LUMEN_AGENT_SELF_UPDATE=systemd:$STATE_DIR
NoNewPrivileges=true
ProtectSystem=strict
PrivateTmp=true
$CAP

[Install]
WantedBy=multi-user.target
UNIT
install_updater
systemctl daemon-reload
systemctl enable --now lumen-agent-update.path
systemctl enable --now lumen-agent
sleep 2
if systemctl is-active --quiet lumen-agent; then
  echo "Lumen agent installed and running. Data should appear in Lumen within a minute."
  echo "Logs: journalctl -u lumen-agent -f    Remove: re-run with --uninstall"
else
  echo "The agent did not start. Check: journalctl -u lumen-agent -n 50" >&2; exit 1
fi
