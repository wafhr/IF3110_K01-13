# Database migrations

## Milestone 2 implementation status

The `internal/migration` package provides definition and revision APIs,
apply/rollback/forward planning, PostgreSQL history, and integration contracts
for approval and execution. The server initializes the definition repository
at startup.

Production wiring currently connects access checks through `access.Service`.
Production approval providers, the persistent queue, the worker loop, and SPA
integration are pending implementation in their respective modules. Action
submissions with valid payloads return `503` while an approval or queue provider
is unavailable. The submission examples below apply once these dependencies
are connected.

Integration tests use a dedicated test approval provider, a persistent test
queue in the platform database, and a caller of the execution hooks. They
verify the APIs, repositories, and migration transactions. Coverage for approval
by every group leader, concurrent worker job claiming, batching, and process
recovery is planned as part of the workflow and execution modules.

## Connections and access

Platform Admins register targets at `/manage/databases` or through
`POST /admin/databases`, then grant database access to groups. The registration
UUID becomes `databaseID`. Connection passwords are encrypted in the platform
database. Each migration operation opens and owns a target pool through the
registry.

`view_only` permission allows reading definitions, revisions, and history.
`view_edit` permission allows creating and revising definitions and submitting
actions. Every request requires an active session. State-changing requests
also require the session's `X-CSRF-Token` header and
`Content-Type: application/json`. Platform Admins access targets through
group-derived grants.

Definitions can be saved for registered targets. Milestone 2 action planning
and history currently support PostgreSQL. MySQL viewer, plan preview, and
legacy history reads follow the Milestone 1 implementation. The PostgreSQL
target account needs permission to create history metadata in the active
schema and execute the approved SQL.

## Definitions, revisions, and execution identities

| Identity | Meaning and storage |
|---|---|
| `migration.id` | Identifies a definition for one registered target, stored in `migration_definitions` on the platform |
| `revision.number` | Numbers SQL up/down revisions starting at one, stored in `migration_revisions` on the platform |
| `execution.id` | Identifies one committed action, stored in `platform_migration_executions` on the target |
| `execution.sequence` | Orders target history records and may contain gaps after a transaction rolls back |
| `job_id` | Identifies persistent work created by the queue provider |

Editing a draft appends a revision. `expected_revision` prevents an outdated
editor from replacing a newer revision. Earlier revisions remain readable.
Saved SQL preserves its exact content, including whitespace and comments.

After worker validation succeeds and before SQL is attempted, `FreezeRevision`
freezes the definition. A subsequent SQL failure leaves the definition frozen.
Corrections after that point require a new migration definition.

A reversible migration has `sql_up` and `sql_down`. An irreversible migration
sets `irreversible: true`, includes a reason, and has an empty `sql_down`.
Names are limited to 160 bytes and each SQL field to 1 MiB. The complete JSON
request body also has a 1 MiB limit.

## Definition and history APIs

Paths in the following table use the prefix
`/databases/{databaseID}/migrations`.

| Method | Path suffix | Access | Successful response |
|---|---|---|---|
| GET | `/definitions` | view_only | 200 with `migrations` |
| POST | `/definitions` | view_edit | 201 with `migration` and `revision` |
| GET | `/definitions/{migrationID}` | view_only | 200 with the definition and current revision |
| PUT | `/definitions/{migrationID}` | view_edit | 200 with the definition and new revision |
| GET | `/definitions/{migrationID}/revisions/{number}` | view_only | 200 with `revision` |
| POST | `/definitions/{migrationID}/actions` | view_edit | 202 with `job_id` and `execution_id` |
| GET | `/history` | view_only | 200 with `executions` and `legacy_attempts` |
| GET | `/history/{executionID}` | view_only | 200 with `execution` |

### Create a definition

Send the following body to `POST /definitions` using the path prefix above.
The server assigns revision one and takes the actor from the session.

```json
{
  "name": "create accounts",
  "sql_up": "CREATE TABLE public.accounts (id bigint PRIMARY KEY, name text)",
  "sql_down": "DROP TABLE public.accounts"
}
```

To revise a draft, send all definition fields and `expected_revision` to
`PUT /definitions/{migrationID}`.

```json
{
  "name": "create accounts",
  "expected_revision": 1,
  "sql_up": "CREATE TABLE public.accounts (id bigint PRIMARY KEY, name text NOT NULL)",
  "sql_down": "DROP TABLE public.accounts"
}
```

The following irreversible definition includes a reason for review.

```json
{
  "name": "delete expired archives",
  "sql_up": "DELETE FROM public.archives WHERE expired = true",
  "irreversible": true,
  "irreversible_reason": "The archived data backup is no longer available"
}
```

### Submit apply, rollback, or forward

The workflow reviews a snapshot from `Service.PrepareAction` and provides
approval for its actor, revision, action, SQL, and `base_execution_id`.
`ApprovalChecker` verifies approval from every relevant group leader at
submission and before execution. The workflow implementation provides the
endpoint for obtaining `approval_id`.

After obtaining an approval UUID, submit the action to
`POST /definitions/{migrationID}/actions`.

```json
{
  "action": "apply",
  "approval_id": "00000000-0000-4000-8000-000000000001"
}
```

Use `rollback` or `forward` as the action for the corresponding operation.
Replace the example UUID with a matching approval. After the queue confirms
persistent storage, the `202` response contains both identifiers.

```json
{
  "job_id": "queue-generated-job-id",
  "execution_id": "execution-uuid"
}
```

SQL completion is monitored through the job module under development.
The submission API completes validation and queue persistence before returning.

`POST /databases/{databaseID}/migrations` also accepts submissions with
`migration_id`, `action`, and `approval_id`. Milestone 1 payloads containing
`name` and `sql` return `400`. Frontend integration updates the editor to use
definitions, review, action submission, and job monitoring.

### Ordering rules

- Apply uses the current revision of a definition that has never been applied.
- Rollback uses the original revision's SQL down for the last active migration.
- Forward uses the original SQL up and follows linear order.
- Forward creates a new execution ID. Rollback references the execution it
  reverses through `reverts_execution_id`.
- Applying a new migration after rollback closes the previous forward path.
- A history position or revision change after approval requires a plan and
  approval matching the current state.
- Irreversible migrations and legacy records without SQL down reject rollback.

### Read history

`GET /history?hide_rollback=true` hides each original execution and the
successful rollback that references its execution ID. Filtering preserves
stored records and each forward execution's identity. Use `false` or omit the
parameter to read complete history. The API accepts one `hide_rollback`
parameter with a value of `true` or `false`.

`executions` contains committed actions. Failed or running Milestone 1 attempts
appear separately in `legacy_attempts`. The job and logging modules provide
Milestone 2 attempts once integrated.

Milestone 1 history remains readable through `GET /migrations` and
`GET /migrations/{version}`. Opening Milestone 2 history imports successful
legacy records once, with a stable execution ID and `legacy_version`.
Imported records have an empty migration ID and revision zero because their
original up/down definitions are unavailable.

### Error status codes

| Status | Cause |
|---|---|
| 400 | Invalid payload, query filter, action, or SQL, including irreversible rollback |
| 401 | Missing or invalid session |
| 403 | Permission or CSRF denial, including target privilege denial |
| 404 | Database, definition, revision, or execution not found |
| 409 | Outdated revision, frozen definition, invalid action order, or changed approval |
| 413 | Request body exceeds the size limit |
| 415 | Unsupported content type for the new APIs |
| 502 | Target connection or authentication failure |
| 503 | Execution provider or target connection unavailable |
| 504 | Operation deadline exceeded |

## Worker transaction contract

`internal/migration/service.go` provides submission contracts and transaction
hooks. Request and receipt types are in `internal/migration/model.go`.
`ActionQueue` persists the complete `ExecutionRequest` and preserves its
execution ID when a job is redelivered.

The hook caller performs the following steps for a regular migration.

1. Open the target connection through the registry using the job's database ID.
2. Acquire an exclusive PostgreSQL lock using `MigrationLockKey(databaseID)`
   on that connection and hold it until the transaction finishes.
3. Begin a writable `READ COMMITTED` transaction on the same connection, then
   call `Service.PrepareExecution`.
4. Check `PreparedExecution.Committed`. Existing commit evidence is used to
   complete a duplicate job or reconcile an ambiguous commit result.
5. For a new job, execute the SQL from `PreparedExecution.Plan`, then call
   `Record` using the same transaction.
6. Commit after both SQL and history are recorded. Roll back the transaction
   if either step fails. Release the lock once commit or rollback finishes.
7. Update job status using the confirmed target commit result.

`PrepareExecution` checks the connection lock, complete history, active actor
permissions, plan snapshot, and current approval, then freezes the revision.
`Record` becomes commit evidence after its transaction commits successfully.
When a commit returns an uncertain result, the caller checks for the execution
ID on the target before deciding to retry SQL. The execution module's guide
will document the worker loop, batch checkpoints, compensation, and lock
recovery.

## Milestone 2 testing

Unit tests are in `internal/migration/*_test.go`. PostgreSQL integration tests
are in `tests/migration_rollback_test.go` and
`tests/migration_history_test.go`, with shared fixtures in
`tests/fixture_test.go`.

Integration coverage includes API creation and revision, access control,
irreversible migrations, last-active rollback, forward order, immutable SQL,
append-only history, execution-ID-based Hide rollback, legacy imports,
access/approval revocation, outdated plans, duplicate jobs, and transactions
that combine SQL changes with history.

Run unit tests from the repository root.

```powershell
go test ./...
go build ./...
go vet ./...
```

For this module's integration tests, start the disposable PostgreSQL service
from the existing test configuration.

```powershell
docker compose -f compose.test.yaml up -d --wait postgres-test
$env:TEST_DATABASE_URL = 'postgres://if3110_test_admin:if3110-integration-password@localhost:55432/postgres?sslmode=disable'

go test -tags=integration -count=1 ./tests -run '^TestMigration.*Integration$'

docker compose -f compose.test.yaml down -v
```

Fixtures create platform and target databases and a role with the `if3110_it_`
prefix, then clean them up after testing. A dedicated disposable local
PostgreSQL server with permission to create databases and roles can also be
used through `TEST_DATABASE_URL`. Tests read this setting from the process
environment. Setup for the complete PostgreSQL and MySQL suite is documented
in the database-management guide. [Integration setup](database-management.md#integration-test).

## Schema diff and SQL generation

Users with `view_edit` can expand **Compare with desired schema and generate
SQL** on the migration page. The desired definition is JSON with a `tables`
array; each table names a schema and table and lists its columns with a SQL
type and `nullable` flag. For example:

```json
{"tables":[{"schema":"public","name":"products","columns":[{"name":"id","type":"bigint","nullable":false},{"name":"title","type":"varchar(200)","nullable":false}]}]}
```

The comparison checks tables and columns in the named schemas. It reports
missing tables and columns, type/nullability differences, and current columns
or tables omitted from the desired definition. SQL is generated for missing
tables and columns only. Type/nullability changes and omitted objects are
reported for manual review; generated SQL never drops objects automatically.
The supported type names are validated before SQL generation. The resulting
SQL is copied into the migration editor, where it can be reviewed and changed
before execution.

The API endpoint is `POST /databases/{databaseID}/migrations/schema-diff`,
requires `view_edit` and the session CSRF token, and accepts
`{"desired_schema": {"tables": [...]}}`. The response contains `changes`,
`sql`, and explanatory `notes` under `schema_diff`.

## Query plan preview (EXPLAIN)

Users with `view_edit` can click **Preview execution plan** below the SQL editor
before running a migration and choose either EXPLAIN or EXPLAIN ANALYZE. The
default is EXPLAIN. Preview requires the SQL statement. Migration definitions
and action submissions use the Milestone 2 API above. PostgreSQL returns its JSON plan tree;
MySQL returns native JSON for EXPLAIN and a TREE string for EXPLAIN ANALYZE.
These include the database's scan/join choices, estimates, and execution
statistics when supported. Planner costs are database-specific units, not
milliseconds. Full native output is also available.
Editing SQL or changing preview mode hides the previous preview so it is not
mistaken for the new query. ANALYZE additionally shows actual rows per loop,
loop counts, node execution times per loop, and total planning/execution times
in milliseconds. Full JSON includes buffer statistics.

```http
POST /databases/{databaseID}/migrations/explain
Content-Type: application/json
X-CSRF-Token: <session token>

{"sql":"SELECT * FROM public.accounts WHERE id = 1"}
```

The response is `{"query_plan":{"mode":"explain","plan":[...],"rolled_back":true}}` for
PostgreSQL. MySQL's native EXPLAIN JSON is returned as an object; its ANALYZE
TREE output is returned as a string. The endpoint requires a valid session,
group-derived `view_edit` permission, CSRF, and the target database privileges
needed to plan the supplied statement. Platform-admin status alone does not
grant preview access.

Without `analyze`, or with `"analyze": false`, preview uses
`EXPLAIN (FORMAT JSON, ANALYZE FALSE)` on PostgreSQL or `EXPLAIN FORMAT=JSON` on
MySQL in a read-only transaction. To request execution statistics, send
`"analyze": true`; the response mode is `explain_analyze`. PostgreSQL uses
`EXPLAIN (FORMAT JSON, ANALYZE TRUE, BUFFERS TRUE)`. MySQL uses
`EXPLAIN ANALYZE FORMAT=TREE`, which supports SELECT, multi-table UPDATE/DELETE,
and TABLE statements. The server runs ANALYZE in a read-write transaction and
explicitly rolls it back before reporting success. Neither mode creates
history/version records. A prepared statement enforces exactly one SQL
statement. The operation has a five-second deadline; PostgreSQL also applies a
statement timeout and one-second lock timeout while planning.

Use SQL supported by the selected database's EXPLAIN. PostgreSQL supports
SELECT, INSERT, UPDATE, DELETE, MERGE, and their WITH forms; MySQL support varies
by preview mode as described above. Ordinary CREATE TABLE and ALTER TABLE
statements are not supported; enter the SQL itself without an EXPLAIN prefix.
Empty, invalid, unsupported, or multiple statements return `400`; target
privilege denial returns `403`, target connection failures `502`, and planning
timeouts `504`. Responses do not expose raw SQL/driver errors.

**ANALYZE executes the supplied SQL**, including functions and triggers. The UI
shows a warning and asks for confirmation before sending an ANALYZE request.
Rollback undoes transactional table changes; it does not undo PostgreSQL sequence
increments (`nextval`/`setval`), MySQL auto-increment values, or effects outside
the database caused by a function/trigger. MySQL DDL and non-transactional table
engines can also implicitly commit effects. Use a disposable target for queries
with those effects. This is an execution preview with rollback, not an isolated
sandbox. PostgreSQL documents
[EXPLAIN ANALYZE execution](https://www.postgresql.org/docs/18/sql-explain.html)
and [sequence rollback behavior](https://www.postgresql.org/docs/18/functions-sequence.html).

The integration suite covers both preview modes against PostgreSQL and MySQL.
The PostgreSQL test verifies estimates and actual statistics for UPDATE, rollback,
and no migration-history side effect. The MySQL test verifies native JSON/TREE
output, rollback of a supported multi-table UPDATE, permission checks, and that
preview does not create migration history. Both use the disposable fixture
described in [integration setup](database-management.md#integration-test).
