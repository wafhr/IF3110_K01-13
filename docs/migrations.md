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
