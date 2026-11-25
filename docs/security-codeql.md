# Security: SAST with CodeQL

## Scan setup

`.github/workflows/codeql.yml` runs CodeQL advanced setup on every push and
pull request to `main` and `develop`, every Monday at 03:00 UTC, and on manual
dispatch. Results are uploaded to the repository's **Security → Code scanning**
page, one category per language.

| Language | Build mode | Scope |
|---|---|---|
| Go | manual (`go build ./...`) | Server, worker, and every `internal/` package |
| JavaScript | none | SPA modules in `web/static/js/` and `k6/stress.js` |
| GitHub Actions | none | Workflows in `.github/workflows/` |

Every language uses the `security-extended` query suite, which adds
lower-precision and lower-severity security queries to the default suite.

## Triage scan

The findings below come from a local run with the same languages, build
commands, and query suites as the workflow:

| Item | Value |
|---|---|
| Date | 25 November 2026 |
| Commit | `9ffb81f` on `feat/codeql-sast` |
| CodeQL | CLI 2.27.6 (`codeql-bundle-v2.27.6`) |
| Query packs | `codeql/go-queries` 1.6.16, `codeql/javascript-queries` 2.5.2, `codeql/actions-queries` 0.6.41 |
| Go toolchain | 1.26.2 locally; the workflow uses the version in `go.mod` |

```sh
codeql database create db-go --language=go --source-root=. --command="go build ./..."
codeql database create db-js --language=javascript-typescript --build-mode=none --source-root=.
codeql database create db-actions --language=actions --build-mode=none --source-root=.
codeql database analyze db-go codeql/go-queries:codeql-suites/go-security-extended.qls --format=sarif-latest --output=go.sarif
codeql database analyze db-js codeql/javascript-queries:codeql-suites/javascript-security-extended.qls --format=sarif-latest --output=js.sarif
codeql database analyze db-actions codeql/actions-queries:codeql-suites/actions-security-extended.qls --format=sarif-latest --output=actions.sarif
```

### Coverage

| Language | Files analyzed | Not analyzed |
|---|---|---|
| Go | 69 of 134 `.go` files: every non-test file in `cmd/`, `internal/`, and `web/` | 65 `_test.go` files (unit and integration tests), which `go build` does not compile |
| JavaScript | 10 of 10 `.js` files, plus `web/index.html` | None |
| GitHub Actions | 1 of 1 workflow | None |

### Results

GitHub maps the `security-severity` score of a rule to Critical (9.0 and
above), High (7.0–8.9), Medium (4.0–6.9), or Low (below 4.0).

| # | Rule | Severity | Location | Decision | Status |
|---|---|---|---|---|---|
| 1 | `go/sql-injection` (CWE-89) | High (8.8) | `internal/migration/explain.go:69` | Risk accepted | Open; to be dismissed as *Won't fix* |
| 2 | `js/client-side-request-forgery` (CWE-918) | Medium (5.0) | `web/static/js/core.js:29`, source `web/static/js/pages/groups.js:102` | Fix | Open; fix and rescan pending |

The GitHub Actions analysis reported no results.

## Findings

### 1. `go/sql-injection`: database query built from user-controlled sources

**Location.** `internal/migration/explain.go:69` prepares
`prefix + statement`, where the prefix is `EXPLAIN (...)` or
`EXPLAIN ANALYZE (...)`.

**Data flow.** The `sql` field of the `POST
/databases/{databaseID}/migrations/explain` body is decoded by
`httpjson.Decode` (`internal/httpjson/httpjson.go:37-38`) in
`internal/migration/http_preview.go:86-90`. It then reaches
`Service.Explain` and is appended to the `EXPLAIN` prefix.

**Potential impact.** A user can run any SQL text on the target database with
the platform's stored target credentials. With `analyze: true`, the statement
really executes before the transaction is rolled back. It can read anything the
target role can read, use resources until the timeout, and cause effects that
a rollback does not undo, such as sequence increments or functions with
external effects.

**Decision: risk accepted (dismiss as *Won't fix*).** Running the user's SQL is
the purpose of the plan preview in the migration editor, and a statement cannot
be passed as a bind parameter. The endpoint does not grant more than the
migration editor already does:

- The route requires a session, the CSRF token, and View + Edit permission on
  the database (`internal/migration/http.go:49`). The same permission lets the
  user author migration SQL.
- Empty statements, NUL bytes, and statements over 1 MiB are rejected
  (`explain.go:32`).
- Preparing the statement makes the database reject a second statement, so
  user SQL cannot end the `EXPLAIN` and run another command (`explain.go:67-69`).
- `EXPLAIN` without `ANALYZE` plans the statement without executing it, inside
  a read-only transaction (`explain.go:45`).
- The transaction is never committed. A 5-second request timeout applies on
  both DBMS (`explain.go:35`). PostgreSQL also sets `statement_timeout = 5s`
  and `lock_timeout = 1s` (`explain.go:51`).
- `EXPLAIN` accepts only queries and DML (and, on PostgreSQL, transactional
  `CREATE TABLE AS`). DDL cannot be wrapped, so MySQL's implicit DDL commit
  does not apply.
- Every SQL execution is logged with the actor, database, and trace ID.

**Residual risk.** Effects outside the transaction remain possible with
`ANALYZE`. The editor asks for confirmation before running `ANALYZE`, and the
[query plan preview guide](migrations.md#query-plan-preview-explain) documents
the behavior.

### 2. `js/client-side-request-forgery`: request URL depends on a user-provided value

**Location.** The sink is the `fetch` call in `web/static/js/core.js:29`. The
source is the `group` query parameter read from `location.search` in
`web/static/js/pages/groups.js:102`. That value is placed in request paths
without encoding at `groups.js:107` (`/admin/groups/${selectedID}/members`) and
`groups.js:108` (`/admin/groups/${selectedID}/databases`).

**Potential impact.** A crafted link such as
`/manage/groups?group=../users` could make a Platform Admin's browser send
same-origin requests to other API paths with the admin's session cookie. These
two requests are `GET` without a body. The page's state-changing requests
already encode the ID (`groups.js:76`). The impact would therefore be limited
to reading data that the admin can already see.

**Current mitigation.** The requests are sent only when `selectedID` exactly
matches the ID of a group returned by `/admin/groups` (`groups.js:103-105`).
Today no crafted value reaches `fetch`. CodeQL does not treat that comparison
as a sanitizer, and the protection would disappear if the check changed.

**Decision: fix.** The pattern is valid even though it cannot be exploited
today. Build both paths from the matched `group.id` with `encodeURIComponent`,
as `bindGroup` already does. Then rescan to confirm that the alert closes.

## Limitations

- A clean or short result does not mean the application is secure. CodeQL
  reports only patterns covered by its queries.
- The Go analysis covers code compiled by `go build ./...`. Test code is not
  analyzed.
- CodeQL does not follow data through storage. Migration SQL saved through the
  API and later executed by the worker (`internal/worker/executor.go`) is user
  SQL by design, but it is not reported. The review and approval workflow,
  permission revalidation in the worker, and execution logs control that path.
- Results of the GitHub run may differ slightly from this local run because of
  CodeQL and Go version updates. The Security tab on GitHub is the record of
  the final status of each alert.
