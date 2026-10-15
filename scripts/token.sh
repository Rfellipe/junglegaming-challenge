#!/bin/sh
set -eu
client=${1:-provider-a}
case "$client" in
  provider-a|provider-b|internal-service) ;;
  *) echo 'usage: scripts/token.sh [provider-a|provider-b|internal-service]' >&2; exit 1 ;;
esac
response=$(curl --fail --silent --show-error --max-time 10 \
  "${OIDC_ISSUER_URL:-http://localhost:8081/realms/jungle}/protocol/openid-connect/token" \
  --data-urlencode grant_type=client_credentials \
  --data-urlencode "client_id=$client" \
  --data-urlencode "client_secret=$client-local")
printf '%s\n' "$response" |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])'
