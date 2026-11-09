# Activity Logging and Tracing

## Runtime setup

`cmd/server/main.go` installs the JSON logger before loading configuration, then
initializes the OpenTelemetry provider before opening database connections.
Application logs use stdout, one JSON object per line, with minimum level INFO
and UTC timestamps. Console spans use stderr through a batch exporter. The
server resource has `service.name=server`.

The provider uses parent-based sampling with sampling enabled for root spans.
An incoming unsampled parent retains correlation IDs in logs. Completed sampled
spans are flushed during provider shutdown with a fresh context bounded by
`SHUTDOWN_TIMEOUT`.

The standard startup configuration from the README is sufficient. To capture
runtime streams separately from build output, use PowerShell from the repository
root after configuring `.env`.

```powershell
go build -o server.exe ./cmd/server
.\server.exe 1> application.jsonl 2> spans.jsonl
```

## Log fields

| Field | Meaning |
|---|---|
| `time`, `level`, `event`, `service` | UTC timestamp, severity, event name, and process identity |
| `actor` | Verified user ID, `anonymous` during unauthenticated requests, or `system` for background work |
| `status` | `running`, `success`, `failed`, or a driver fallback marked `skipped` |
| `trace_id`, `span_id` | Active OpenTelemetry context when a valid span is available |
| `database` | Physical database name on SQL records |
| `database_id`, `migration_id`, `revision` | Registry and immutable migration identifiers when available |
| `job_id`, `execution_id`, `attempt`, `batch` | Scope supplied by the queued request and executor |
| `duration_ms` | Elapsed operation time in milliseconds |

`WithLogAttrs` creates an operation scope whose metadata is inherited by SQL
logs and spans. Explicit record or bound logger attributes take precedence over
context attributes. Attributes follow the logger's active slog group.
`WithActor` propagates the verified identity to HTTP completion logging across
request copies.

## Activity and HTTP events

Domain operations emit `<operation>_started` and `<operation>_completed`.
Completion uses the returned error after the operation and its deferred cleanup.
Creation events add generated IDs once available. The service/repository layer
also covers operations invoked by browser forms.

| Area | Representative operations |
|---|---|
| Authentication | `auth_login`, `session_validate`, `auth_logout` |
| Administration | `user_create`, `user_update`, `group_create`, `group_membership_set`, `database_permission_set` |
| Registry and viewer | `database_register`, `database_update`, `database_connection_check`, `schemas_list`, `tables_list`, `table_inspect`, `table_data_read` |
| Migration | `migration_define`, `migration_revise`, `migration_submit`, `migration_enqueue`, `migration_execution_prepare`, `migration_history_stage` |
| Attempt and lock | `worker_attempt`, `migration_lock_acquire`, `migration_lock_release` |

`HTTPHandler` creates a server span, extracts W3C trace context, and returns
`X-Trace-ID` when the span context is valid. `http_request_completed` includes
method, registered router pattern, response status when available, and duration.
Router patterns can be mount prefixes in nested middleware. Missing routes use
the HTTP method as the span name. Request URL values, query strings, headers,
and bodies are excluded from this middleware's attributes.

HTTP 4xx responses produce WARN completion records. HTTP 5xx responses,
cancellation, and panic produce ERROR records and error span status. Access
checks emit `access_denied` with a fixed reason such as
`database_permission_required`, `invalid_session`, or `invalid_csrf`.

Activity and HTTP instrumentation record generic panic categories and propagate
the panic through the caller's existing handling.

## SQL observations

`SQLConnector` wraps the platform PostgreSQL connector and the PostgreSQL/MySQL
target connectors. Instrumentation covers direct and prepared execution,
connection establishment, ping, begin, commit, rollback, and result streaming.
Each operation has separate started and completed events, such as
`sql_exec_started`, `sql_exec_completed`, and `sql_commit_completed`.

SQL records include `database_role` (`platform` or `target`), `db_system`,
`operation`, `query_template`, `parameters`, `parameter_count`, and duration as
applicable. The template retains SQL structure with literals and identifiers
replaced by `?`. Comments are removed. Unrecognized tokens cause full query
redaction. Each SQL record stores `[REDACTED]` in `parameters` and the number
of supplied values in `parameter_count`.

Successful Exec operations include `rows_affected` when supported. Query
operations include `rows_read` and `rows_complete`. The query span lasts until
the result is closed, so streaming and close failures are captured. Partial
reads retain their observed count with `rows_complete=false`.

Errors use safe categories (`sql_error`, `driver_error`, `connection_error`,
`canceled`, or `timeout`) and a validated five-character `sqlstate` when
available. The original driver error continues to the caller. Driver fallback
through `ErrSkip` emits `status=skipped` and allows prepared execution to proceed.
Each `ErrBadConn` retry is observable through its own driver attempt.

`migration_history_stage_completed` includes `commit_pending=true`. The executor
establishes the final commit outcome. A duplicate execution can report
`commit_proof=true` after the preparation hook finds an existing committed record.

## Queue propagation and worker integration

`ExecutionRequest.trace_context` carries W3C `traceparent` and `tracestate`.
`migration_enqueue` is a PRODUCER span. Its context is captured after approval
validation and serialized with the full approved execution snapshot. The queue
adapter persists that payload before returning a job ID.

`worker.TraceAttempt(workerCtx, request, jobID, attempt, run)` restores the
serialized parent and creates a CONSUMER span. The callback receives actor,
database, migration, revision, execution, job, and attempt metadata. Lock, batch,
SQL, history, and commit/rollback work use that callback context. A batch can add
its number through `WithLogAttrs` or `StartActivity`.

The caller supplies a process-owned worker context and a positive attempt
number. The callback returns nil after its outcome and commit evidence are
established. Retries retain the stored producer parent and execution ID while
creating distinct attempt spans. The worker context supplies cancellation and
deadlines for execution.
Payloads with missing or invalid trace context start a fresh trace. W3C
propagation excludes baggage from the serialized carrier.

```mermaid
flowchart LR
    H[HTTP request] --> S[migration_submit]
    S --> P[migration_enqueue PRODUCER]
    P --> Q[ExecutionRequest JSONB]
    Q --> W[TraceAttempt CONSUMER]
    W --> L[Executor lock and batch scope]
    L --> D[SQL and history transaction]
    D --> C[Commit or rollback]
```

### Current integration boundary

The server's production approval and queue dependencies await their module
implementations. The job repository, polling loop, batch execution, retry
scheduler, and `cmd/worker` bootstrap are scaffolds in this checkout. Module 4
owns their runtime wiring, including logger/provider initialization with
`service.name=worker` and provider shutdown.

The integration suite exercises the diagram using real PostgreSQL transactions,
a JSONB test queue, a fixture approval adapter, and `TraceAttempt` callbacks. The
fixture callback assigns a batch scope around a transaction. Production batch
selection and recovery belong to the executor implementation.

## Redaction

The logger and console exporter apply shared rules to sensitive field names,
nested structures, credential-pattern text, and configured secret literals.
Server setup registers the platform connection URL, database encryption key,
and bootstrap password. URL passwords are also recognized in decoded form.

- Password, username, token, session, cookie, authorization, encryption-key,
  connection-string, body, and raw SQL/parameter fields are masked.
- Free text containing credential assignments, credential URLs, or authorization
  patterns is fully redacted.
- SQL templates mask all literals and identifiers, and SQL parameters are fully
  redacted. Result row values are excluded from telemetry.
- Driver error messages use safe categories and SQLSTATE. Activity error and
  panic messages use safe categories.
- Trace names, attributes, events, links, status descriptions, resource
  attributes, and instrumentation scopes pass through redaction.

Logging additional application data requires appropriate sensitive field names
or registration of known secret literals. Raw unlabeled values need explicit
handling at their call site.

## Example output

The following illustrative records use fictional IDs and selected fields from
JSON log and console span output.

```json
{"time":"2026-11-09T05:00:00Z","level":"INFO","event":"sql_exec_completed","service":"worker","database":"app","database_role":"target","operation":"exec","query_template":"UPDATE ? SET ? = ? WHERE ? = ?","parameters":"[REDACTED]","parameter_count":2,"rows_affected":1,"status":"success","actor":"00000000-0000-4000-8000-000000000001","execution_id":"00000000-0000-4000-8000-000000000002","job_id":"00000000-0000-4000-8000-000000000003","attempt":1,"batch":1,"trace_id":"11111111111111111111111111111111","span_id":"2222222222222222"}
```

```json
{
  "Name": "db.exec",
  "SpanContext": {
    "TraceID": "11111111111111111111111111111111",
    "SpanID": "2222222222222222",
    "TraceFlags": "01",
    "TraceState": "",
    "Remote": false
  },
  "Attributes": [
    {"Key":"db.operation.name","Value":{"Type":"STRING","Value":"exec"}},
    {"Key":"db.query.summary","Value":{"Type":"STRING","Value":"UPDATE ? SET ? = ? WHERE ? = ?"}}
  ]
}
```

## Verification

Unit tests reside beside their implementations.

```powershell
go test ./...
```

Use the dedicated disposable PostgreSQL service for the telemetry integration
suite. `TEST_DATABASE_URL` is read from the test process environment.

```powershell
docker compose -f compose.test.yaml up -d --wait postgres-test
$env:TEST_DATABASE_URL = 'postgres://if3110_test_admin:if3110-integration-password@localhost:55432/postgres?sslmode=disable'
go test -tags=integration -count=1 ./tests -run '^TestTelemetry.*Integration$'
docker compose -f compose.test.yaml stop postgres-test
```

`tests/telemetry_test.go` verifies HTTP/domain/platform SQL correlation,
read-only queries, access denial, privacy, JSONB propagation across server and
worker provider identities, SQLSTATE, rollback, commit, lock spans, retry scopes,
and duplicate commit evidence. Tests restore process-wide logging/tracing state
and use synchronized capture buffers. The fixture follows the server's HTTP and
platform SQL instrumentation entry points.

The complete PostgreSQL/MySQL integration instructions are in the database
management guide. [Database management](database-management.md#integration-test).
