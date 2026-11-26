# Frontend SPA

The browser application is a single HTML shell at `/` and `/login`, loaded as
native ES modules without a build step:

- `web/static/js/spa.js` is the client-side router. It maps paths to pages and
  uses the History API.
- `web/static/js/core.js` holds shared state, the JSON API client with CSRF
  handling, navigation, notices, and the page shell and small HTML builders.
- `web/static/js/pages/*.js` holds one module per screen: `login`, `home`,
  `users`, `groups`, `databases` (target registrations), `workspace` (schema,
  table, and data viewer), and `migrations`.

Pages fetch JSON from the Go APIs. Links between workspace pages do not reload the shell;
direct links and browser back/forward navigation restore the requested route.
The shell keeps the existing server-side session cookie and gets the session
CSRF token from `GET /auth/csrf` for state-changing requests.

## Screens

- Sign in and sign out, including session restoration after a reload.
- Workspace database selection and the responsive schema, table, and data
  viewer with paginated rows.
- Platform admin user management, group membership and leader management,
  database grants, and target database registration/edit/test/delete.
- Migration definition and revision editor with up/down SQL, irreversible
  reasons, optional key-range batch settings, schema diff generation, and
  `EXPLAIN`/`EXPLAIN ANALYZE` preview.
- Review and approval status for every group with database access, queued
  apply/rollback/forward actions, migration history with committed executions
  and failed or pending attempts, Hide rollback,
  job status, batch progress, resume, and compensation of partial jobs.

Migration actions use the review workflow API before queue submission. The
editor saves an immutable revision, creates a review snapshot, waits for all current
group leaders, then submits the approved workflow ID as `approval_id`. Job
status is refreshed every three seconds while active jobs are present, and the
history refreshes when a job finishes. Leaders of View Only groups review and
approve from the same page.

The layout uses flexible forms and horizontally scrollable tables so the
workspace remains usable on narrow screens. SQL editors keep their own scroll
area rather than widening the page.

## API dependencies

The screens use the APIs mounted by `cmd/server`: `/auth`, `/admin`,
`/admin/databases`, `/databases`, database schema/data routes, migration
definitions/workflows/history, and job status/resume/cancel routes. The SPA
displays API errors in the current view; the server remains authoritative for
authentication, permission, CSRF, approval, and migration-state validation.

Start the server and worker as described in the [README](../README.md). A
queued migration remains pending until a worker is running. Migrations run on
both PostgreSQL and MySQL targets.
