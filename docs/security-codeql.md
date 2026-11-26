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
| 1 | `go/sql-injection` (CWE-89) | High (8.8) | `internal/migration/explain.go:69` | Risk accepted | Still reported by the rescan; dismiss on GitHub as *Won't fix* (see finding 1) |
| 2 | `js/client-side-request-forgery` (CWE-918) | Medium (5.0) | `web/static/js/core.js:29`, source `web/static/js/pages/groups.js:102` | Fix | Fixed in `451e33d`; the rescan reports no JavaScript results |

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

**Dismissal on GitHub.** Once the alert appears in **Security → Code
scanning**, dismiss it with the reason *Won't fix* and this comment (232 of
the 280 allowed characters):

```text
By design: EXPLAIN preview plans the editor's own SQL. Needs View+Edit and CSRF; one prepared statement; read-only or always rolled-back tx; 5s timeout. The same permission already authors migration SQL. See docs/security-codeql.md.
```

A user with write access can also dismiss it through the API, where
`ALERT_NUMBER` is the number shown on the alert page:

```sh
gh api -X PATCH repos/wafhr/IF3110_K01-13/code-scanning/alerts/ALERT_NUMBER \
  -f state=dismissed -f dismissed_reason="won't fix" \
  -f dismissed_comment="By design: EXPLAIN preview plans the editor's own SQL. Needs View+Edit and CSRF; one prepared statement; read-only or always rolled-back tx; 5s timeout. The same permission already authors migration SQL. See docs/security-codeql.md."
```

The rule stays enabled, so a new SQL injection elsewhere still raises a new
alert.

### 2. `js/client-side-request-forgery`: request URL depends on a user-provided value

**Location.** The sink is the `fetch` call in `web/static/js/core.js:29`. The
source is the `group` query parameter read from `location.search` in
`web/static/js/pages/groups.js:102`. Before the fix, that value was placed in
request paths without encoding at `groups.js:107`
(`/admin/groups/${selectedID}/members`) and `groups.js:108`
(`/admin/groups/${selectedID}/databases`).

**Potential impact.** A crafted link such as
`/manage/groups?group=../users` could make a Platform Admin's browser send
same-origin requests to other API paths with the admin's session cookie. These
two requests are `GET` without a body. The page's state-changing requests
already encode the ID (`groups.js:76`). The impact would therefore be limited
to reading data that the admin can already see.

**Mitigation before the fix.** The requests were sent only when `selectedID`
exactly matched the ID of a group returned by `/admin/groups`
(`groups.js:103-105`), so no crafted value reached `fetch`. CodeQL does not
treat that comparison as a sanitizer, and the protection would have
disappeared if the check changed.

**Decision: fix.** The pattern is valid even though it could not be exploited.

**Fix.** Commit `451e33d` builds both paths from the matched `group.id` with
`encodeURIComponent`, as `bindGroup` already does, and binds the group actions
only when a group matched. The page was checked in a browser against a stubbed
API:

| URL | Requests after the group, user, and database list calls |
|---|---|
| `/manage/groups?group=<existing ID>` | `GET .../members` and `GET .../databases` with the encoded ID; *Save leader* sends `PUT .../leader` |
| `/manage/groups` | The same two requests for the first group |
| `/manage/groups?group=../users` | None; the page shows "Select a group" |

## Fix and rescan

The rescan ran on commit `451e33d` with the same CodeQL version, commands, and
query suites as the triage scan.

| Language | Results before | Results after |
|---|---|---|
| Go | 1 (`go/sql-injection`) | 1 (`go/sql-injection`, risk accepted) |
| JavaScript | 1 (`js/client-side-request-forgery`) | 0 |
| GitHub Actions | 0 | 0 |

## Evidence on GitHub

After this branch is pushed and a pull request to `develop` or `main` is
opened, or after it is merged, the workflow uploads the results:

- **Actions → CodeQL** shows a successful run for the analyzed commit, with
  one job per language.
- **Security → Code scanning** (tool: CodeQL) lists the `go/sql-injection`
  alert, which is then dismissed as described in finding 1. The JavaScript
  alert does not appear, because the fix is in the analyzed code. If an
  earlier commit was analyzed, the alert appears as closed.

Alert pages show the rule, severity, location, and data flow described above.

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
