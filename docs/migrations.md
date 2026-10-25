# Database migrations

The migration module obtains target connection settings from the database
management registry. Migration history is stored in each target database, so
versions are independent and linear per database.

## Register a PostgreSQL database

Only a Platform Admin can register a target connection, using the browser page
at `/manage/databases` or the database management API. The API accepts
connection fields in JSON; it does not accept a connection URL. The password
is encrypted in the platform registry and is never returned by the API.

```http
POST /admin/databases
Content-Type: application/json
X-CSRF-Token: <session token>

{"name":"development","host":"localhost","port":5432,"database_name":"appdb","username":"user","password":"replace-with-target-password","ssl_mode":"disable"}
```

The response contains the registration UUID at `database.id`. Grant that UUID to
a group through `/admin/groups/{groupID}/databases/{databaseID}`. A member
with `view_only` can read history; `view_edit` can execute migrations. The
effective group permission follows the access module's rule: `view_edit` wins.

Migration connections are opened from that registry using saved, decrypted
credentials. Each migration operation owns and closes its connection pool;
the viewer uses its own managed pool.

## Migration API

- `GET /databases/{databaseID}/migrations` lists history, newest version first.
- `GET /databases/{databaseID}/migrations/{version}` returns migration detail,
  including the SQL text.
- `POST /databases/{databaseID}/migrations` executes a new migration:

```json
{"name":"create accounts","sql":"CREATE TABLE accounts (id bigint generated always as identity primary key)"}
```

The POST requires `X-CSRF-Token`. The server serializes executions per target
database with a PostgreSQL advisory lock. It records a `running` row, executes
the SQL and success update in one transaction, commits on success, and rolls
back SQL changes on error before marking the history row `failed`. Failed
migrations keep their version in the linear history; the next execution gets a
new version. A process interruption can leave a visible `running` record.

The browser history and SQL editor are at
`/workspace/databases/{databaseID}/migrations`. The editor is available only
with `view_edit`; API list and detail responses remain JSON.

The target database account needs permission to create and update the
`platform_migrations` history table and to perform the requested migration.

## Query plan preview (EXPLAIN)

Users with `view_edit` can click **Preview execution plan** below the SQL editor
before running a migration and choose either EXPLAIN or EXPLAIN ANALYZE. The
default is EXPLAIN. Only SQL is required for preview; the migration name
is required when executing a migration. The result shows the plan tree, scan and
join types, relation/index names, estimated rows, and startup/total cost. Costs
are PostgreSQL planner units, not milliseconds. Full JSON is also available.
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

The response is `{"query_plan":{"mode":"explain","plan":[...],"rolled_back":true}}`, retaining
PostgreSQL's JSON plan tree. The endpoint requires a valid session, group-derived
`view_edit` permission, CSRF, and the target PostgreSQL privileges needed to plan
the supplied statement. Platform-admin status alone does not grant preview access.

Without `analyze`, or with `"analyze": false`, preview uses
`EXPLAIN (FORMAT JSON, ANALYZE FALSE)` in a read-only transaction.
To request execution statistics, send `"analyze": true` in the same JSON body;
the response mode is `explain_analyze`. The server then uses
`EXPLAIN (FORMAT JSON, ANALYZE TRUE, BUFFERS TRUE)` in a read-write transaction
and explicitly rolls it back before reporting success. Neither mode creates
history/version records. A prepared statement enforces exactly one SQL statement; trailing
semicolons and semicolons inside SQL literals are handled by PostgreSQL. The
operation has a five-second deadline and statement timeout, plus a one-second
lock timeout while planning.

Use SQL supported by PostgreSQL EXPLAIN, such as SELECT, INSERT, UPDATE, DELETE,
MERGE, or their WITH forms. Ordinary CREATE TABLE and ALTER TABLE statements are
not supported; enter the SQL itself without an EXPLAIN prefix. Empty, invalid,
unsupported, or multiple statements return `400`; target privilege denial returns
`403`, target connection failures `502`, and planning timeouts `504`. Responses
do not expose raw SQL/driver errors.

**ANALYZE executes the supplied SQL**, including functions and triggers. The UI
shows a warning and asks for confirmation before sending an ANALYZE request.
Rollback undoes transactional table changes; it does not undo PostgreSQL sequence
increments (`nextval`/`setval`) or effects outside the database caused by a
function/trigger. Use a disposable target for queries with those effects. This is
an execution preview with rollback, not an isolated sandbox. PostgreSQL documents
[EXPLAIN ANALYZE execution](https://www.postgresql.org/docs/18/sql-explain.html)
and [sequence rollback behavior](https://www.postgresql.org/docs/18/functions-sequence.html).

One integration test, `TestQueryPlanPreviewIntegration`, covers both preview
modes against real PostgreSQL: an UPDATE returns estimates for EXPLAIN, actual
statistics for ANALYZE, preserves the original table data after each request,
and creates no migration history table. It reuses the disposable fixture described
in [integration setup](database-management.md#integration-test).
