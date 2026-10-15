#!/bin/sh
# Run only against this project's disposable local Compose stack.
set -eu
base=http://localhost:8080
restore() { docker compose start postgres localstack > /dev/null 2>&1 || true; }
trap restore EXIT
trap 'exit 1' INT TERM
wait_status() {
  path=$1
  expected=$2
  attempt=0
  while [ "$attempt" -lt 45 ]; do
    actual=$(curl --silent --output /dev/null --write-out '%{http_code}' --max-time 5 "$base$path" || true)
    if [ "$actual" = "$expected" ]; then return 0; fi
    attempt=$((attempt + 1))
    sleep 1
  done
  echo "$path: expected HTTP $expected, got $actual" >&2
  return 1
}
wait_token() {
  attempt=0
  while [ "$attempt" -lt 45 ]; do
    if ./scripts/token.sh provider-a > /dev/null 2>&1; then return 0; fi
    attempt=$((attempt + 1))
    sleep 1
  done
  echo 'Keycloak token issuance did not recover' >&2
  return 1
}
wait_status /health/ready 200
wait_token
for service in localstack postgres; do
  docker compose stop "$service"
  wait_status /health/ready 503
  wait_status /health/live 200
  docker compose start "$service"
  wait_status /health/ready 200
  wait_token
  echo "$service outage, readiness recovery, and token issuance verified"
done
