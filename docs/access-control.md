# Access control

The access-control module stores platform users, groups, database grants, and
sessions in the PostgreSQL platform database. A database grant refers to the
UUID used by the database-management module; this module does not store target
database connection details.

## Configuration and bootstrap

Set `PLATFORM_DATABASE_URL` to the platform database connection string. On an
empty access-control database, also set `BOOTSTRAP_ADMIN_USERNAME` and
`BOOTSTRAP_ADMIN_PASSWORD`. The first startup creates that account as the first
Platform Admin. Later starts do not reset or promote accounts from these
environment variables. The bootstrap password must contain at least 12
characters. Remove the bootstrap secret from the runtime environment after the
first successful startup.

Set `APP_ENV=production` to enable secure session cookies. When HTTPS terminates
at a reverse proxy, also set `COOKIE_SECURE=true` so the browser receives the
cookie with the Secure attribute.

## Session policy

- Cookie name: `platform_session`; it is HttpOnly, SameSite=Lax, and Secure in
  production/HTTPS.
- Idle timeout: 30 minutes.
- Absolute lifetime: 12 hours; rotation does not extend it.
- Rotation interval: 15 minutes, with a 30 second acceptance window for the
  immediately previous identifier.
- Session identifiers are random 256-bit values. PostgreSQL stores their
  SHA-256 hashes. For up to 30 seconds after rotation, the new identifier is
  temporarily stored so a request using the previous identifier can receive
  the updated cookie. It is no longer accepted after 30 seconds and is cleared
  on the next authenticated request for that session or during a later login's
  cleanup.
- Login and privilege increases rotate the identifier. The previous identifier
  works for the grace window. Logout, user disablement, and access reductions
  revoke affected sessions immediately.
- An admin cannot disable or demote the current account or the last active
  Platform Admin.
- Passwords use PBKDF2-HMAC-SHA256 with a per-password random salt and 600,000
  iterations.

## HTTP API

JSON request and response bodies are used. Login accepts credentials in the
request body; it never returns the session identifier in JSON. The browser gets
it only through `Set-Cookie`.

| Method and path | Purpose |
|---|---|
| `POST /auth/login` | Sign in with `{"username":"...","password":"..."}` |
| `GET /auth/me` | Return the signed-in user's public profile |
| `GET /auth/csrf` | Return the session-bound CSRF token |
| `POST /auth/logout` | Revoke the current session |
| `GET /admin/users` | List users (Platform Admin) |
| `POST /admin/users` | Create a user with username, initial password, and optional `platform_admin` |
| `PATCH /admin/users/{userID}` | Set both `active` and `platform_admin` flags |
| `GET /admin/groups` | List groups (Platform Admin) |
| `POST /admin/groups` | Create a group with `{"name":"..."}` |
| `DELETE /admin/groups/{groupID}` | Delete a group and revoke its members' sessions |
| `GET /admin/groups/{groupID}/members` | List group members |
| `PUT /admin/groups/{groupID}/members/{userID}` | Add a user to a group |
| `DELETE /admin/groups/{groupID}/members/{userID}` | Remove a user from a group |
| `GET /admin/groups/{groupID}/databases` | List a group's database grants |
| `PUT /admin/groups/{groupID}/databases/{databaseID}` | Set `{"permission":"view_only"}` or `view_edit` |
| `DELETE /admin/groups/{groupID}/databases/{databaseID}` | Remove a database grant |
| `GET /databases/{databaseID}/access` | Return the signed-in user's effective permission |

After login, clients fetch `/auth/csrf` and send the returned token in
`X-CSRF-Token` on every state-changing authenticated request. Browser requests
with a cross-site Fetch Metadata value or mismatched `Origin` are rejected.

`View + Edit` wins when several groups grant different levels for the same
database. Platform Admin status controls platform-management endpoints only;
it does not itself grant access to target database contents.

Database viewer and migration handlers should use
`HTTPHandler.RequireDatabaseAccess(access.ViewOnly, next)` for read operations
and `HTTPHandler.RequireDatabaseAccess(access.ViewEdit, next)` for edits or
migrations. The wrapped route must have a `{databaseID}` path value. This keeps
the authorization check on the backend for each operation.
