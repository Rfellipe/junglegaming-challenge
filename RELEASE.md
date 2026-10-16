# Release validation — October 16, 2026

Run `./scripts/check-release.sh` from the repository root with Go 1.27.1, Docker Compose, curl and Python 3 installed. Integration tests use real PostgreSQL, Keycloak and LocalStack; they create isolated databases/queues and kill only their child application processes. The distributed test briefly pauses the local LocalStack container; do not run it against shared infrastructure.

## Validation record

- `gofmt` applied to changed Go files; `git diff --check` passed.
- `go test ./...` passed.
- `go test -race ./...` passed, including concurrent metrics scrapes and log redaction checks.
- `go vet ./...` and `go vet -tags=integration ./...` passed.
- `go test -tags=integration -race -count=1 -timeout=5m ./...` passed; integration package 141.115s. Includes three independent processes, 50 duplicate bets, competing debits, HTTP/SQS overlap, poison/DLQ, commit/claim/publication SIGKILL, stable event IDs, reference recovery and reconciliation.
- Final independent three-process recovery/race rerun passed (77.927s), including assertions across every wallet and zero pending transactions or unpublished/leased outbox rows after recovery.
- Metrics authorization tested with real tokens: 401 without credentials, 403 for provider, 200 for internal service.

- Final focused HTTP/worker lifecycle test passed with race detector (1.965s): server correlation IDs, outcome/duplicate/conflict counters and authenticated metrics gauges.
- Fresh source snapshot without .env, separate Compose project and new PostgreSQL volume passed `docker compose up --build --detach --wait`. Migrations and queue provisioning succeeded, readiness and authenticated metrics returned 200, and SIGTERM completed every lifecycle hook. The temporary project/volume was removed; existing development volumes were preserved and services restored.
- `scripts/check-recovery.sh` passed LocalStack/PostgreSQL outages, readiness recovery and token issuance.

An extra distributed rerun was interrupted by an accidentally overlapping dependency outage check (SQS connection closed). The checks were separated and the distributed suite rerun independently. The release runner executes dependency-affecting work sequentially.

## Scope and production limitations

FAILED is a supported domain/schema state; infrastructure errors roll back and return retryable errors rather than persisting a terminal failure. Independent operations complete synchronously; missing references use the durable reference worker.

Production still requires TLS, managed/rotated credentials, restricted database roles, backup/restore policies and AWS IAM enforcement tests. LocalStack demonstrates queue provisioning and redrive, not AWS authorization. Shared input queues accept trusted producers; IAM does not authenticate providerId inside JSON. External providers need an authenticated gateway or isolated queues.

Publication is at least once, with no financial ordering guarantee across publishers. Consumers must persist eventId deduplication. Outbox retries have no terminal discard; monitor backlog and age. FIFO may suppress republished messages during its deduplication window. JSON logs and process-local metrics are available; retention, collection, alerts, dashboards, tracing and latency percentiles are outside scope. Counters reset on restart and cannot substitute for the durable audit records. Exact money uses int64 minor units; balances cannot exceed 92233720368547758.07 BRL. Deferred ledger sum constraints scale with wallet history.

The release gate does not load-test production throughput or prove resilience outside the tested interruption boundaries. The example credentials and HTTP endpoints are local development settings. Default LocalStack storage is ephemeral; PostgreSQL volumes persist after `docker compose down`.
