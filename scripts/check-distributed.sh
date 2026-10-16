#!/bin/sh
# Creates isolated databases and queues; kills only its own child processes.
set -eu
docker compose up -d --wait postgres keycloak localstack
go test -tags=integration -race -count=1 -timeout=5m ./integration -run '^TestThreeProcessRecovery$' -v
