# Database migrations

The migration module obtains target connection settings from the database
management registry. Migration history is stored in each target database, so
versions are independent and linear per database.

## Register a target database

Only a Platform Admin can register a target connection, using the browser page
at `/manage/databases` or the database management API. The API accepts
connection fields in JSON; it does not accept a connection URL. The password
is encrypted in the platform registry and is never returned by the API.

```http
POST /admin/databases
Content-Type: application/json
X-CSRF-Token: <session token>

{"name":"development","dbms":"mysql","host":"localhost","port":3306,"database_name":"appdb","username":"user","password":"replace-with-target-password","ssl_mode":"disable"}
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
database with a PostgreSQL advisory lock or a MySQL named lock. It records a
`running` row, executes the SQL, then records success. PostgreSQL runs the SQL
and success update in one transaction, so failures roll back SQL changes. MySQL
rolls back transactional DML on error, but implicitly commits many DDL
statements (`CREATE`, `ALTER`, and `DROP`); those effects cannot be rolled back
by this application. Statements that take control of the runner's transaction
(including explicit transaction commands and MySQL `autocommit` changes) are
rejected. Failed migrations keep their version in the linear history; the next
execution gets a new version. A process interruption can leave a visible
`running` record.

The browser history and SQL editor are at
`/workspace/databases/{databaseID}/migrations`. The editor is available only
with `view_edit`; API list and detail responses remain JSON.

The target database account needs permission to create and update the
`platform_migrations` history table and to perform the requested migration.
MySQL targets should use a transactional table engine such as InnoDB when
rollback of DML is required.

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
default is EXPLAIN. Only SQL is required for preview; the migration name
is required when executing a migration. PostgreSQL returns its JSON plan tree;
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
