#!/bin/sh
# Runs INSIDE the agent container (alpine): downloads the checksum-verified agent and starts it.
# Configuration comes from LUMEN_AGENT_* environment variables.
set -eu
LUMEN_URL="${LUMEN_AGENT_URL:-__LUMEN_URL__}"
case "$(uname -m)" in
  x86_64) ARCH=amd64;; aarch64) ARCH=arm64;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1;;
esac
NAME="lumen-agent-linux-$ARCH"
wget -qO /tmp/lumen-agent "$LUMEN_URL/download/$NAME"
wget -qO /tmp/SHA256SUMS "$LUMEN_URL/download/SHA256SUMS"
WANT="$(grep " $NAME\$" /tmp/SHA256SUMS | cut -d' ' -f1)"
[ -n "$WANT" ] || { echo "no checksum for $NAME" >&2; exit 1; }
echo "$WANT  /tmp/lumen-agent" | sha256sum -c - >/dev/null || { echo "checksum mismatch, refusing to start" >&2; exit 1; }
chmod +x /tmp/lumen-agent
exec /tmp/lumen-agent
