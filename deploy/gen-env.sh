#!/usr/bin/env bash
# Generates deploy/.env with random secrets. Refuses to overwrite an existing file.
# Usage: ./gen-env.sh [tenant-name]      (default tenant: main)
set -euo pipefail
cd "$(dirname "$0")"
[ -e .env ] && { echo ".env already exists, not overwriting"; exit 1; }
TENANT="${1:-main}"
[[ "$TENANT" =~ ^[A-Za-z0-9_-]+$ ]] || { echo "tenant must match [A-Za-z0-9_-]+"; exit 1; }
KEY="lmn_$(openssl rand -hex 24)"
ADMIN_PW="$(openssl rand -hex 12)"
ES_PW="$(openssl rand -hex 16)"
umask 077
cat > .env <<ENV
CLICKHOUSE_PASSWORD=$(openssl rand -hex 24)
LUMEN_API_KEYS=${KEY}:${TENANT}
ELASTIC_PASSWORD=${ES_PW}
# encrypts the Nextcloud tokens stored in Elasticsearch; keep a copy of this file: without the key they cannot be read
LUMEN_SECRET_KEY=$(openssl rand -hex 32)
LUMEN_ADMIN_USER=admin
LUMEN_ADMIN_PASSWORD=${ADMIN_PW}
LUMEN_ADMIN_TENANT=${TENANT}
# bind address for the Lumen port: 127.0.0.1 when a TLS reverse proxy runs on this host
LUMEN_BIND=0.0.0.0
ENV
echo "Wrote deploy/.env (chmod 600)."
echo "Web UI login (created on first start; change the password in the UI):"
echo "  username: admin"
echo "  password: $ADMIN_PW"
echo "API key for tenant '$TENANT' (shown once, also stored in .env):"
echo "  $KEY"
echo "Add more tenants by appending ',key:tenant' to LUMEN_API_KEYS."
