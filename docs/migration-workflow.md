# Migration workflow and approval

Migration definitions are drafts until an editor submits an action for review.
Review snapshots bind the database registration, migration ID, revision, action,
SQL plan, rollback/forward position, and batch settings. Editing a definition
creates a new immutable revision; an earlier review stays in the record but is
reported as `draft` and cannot authorize the changed plan.

Every group with a grant to the database must approve, including `view_only`
groups. Each group has one leader. A leader must be an active user who is a
member of that group. If one user leads several groups, one approval request by
that user records approval for each of those groups. A group without a valid
leader remains outstanding and prevents the workflow from becoming approved.

## States and transitions

| State | Meaning | Transition |
|---|---|---|
| `draft` | No current review exists, or the definition has advanced beyond the latest submitted revision | An editor submits an action for review |
| `pending_review` | A snapshot exists and at least one current required leader approval is missing | Current group leaders approve; policy changes require a new review |
| `approved` | Every group currently granted access has approved the exact snapshot | The editor submits the matching action to the migration queue |

The normal path is `draft → pending_review → approved → queued`. Approval does
not itself enqueue or execute SQL. The existing action endpoint remains the
queue boundary and accepts the review ID as `approval_id`. The worker validates
the plan, permission, policy epoch, leader set, and every required approval
again while holding the target database lock before executing SQL.

## Access policy changes

The platform stores a monotonic approval policy epoch for each database
registration. Changing a database grant, group membership, or leader advances
the epoch in the same transaction as the access change. Deactivating a leader
clears the leader assignment and advances epochs for affected databases.
Therefore, any such change invalidates all outstanding approvals for that
database, even if the previous grant or leader is later restored. Submit a new
review after the access policy is stable.

Changing a draft creates a new revision through the existing revision API. The
new revision is a draft, and the old workflow cannot authorize it. A definition
that has been frozen by a migration attempt cannot be revised; create a new
migration instead.

## Group leader administration

Platform Admins set a group leader through the existing admin API. The selected
user must be active and already belong to the group. Send an empty `user_id` to
clear the assignment.

```http
PUT /admin/groups/{groupID}/leader
Content-Type: application/json
X-CSRF-Token: <session token>

{"user_id":"51e8..."}
```

The group listing includes `leader_id`. Removing the leader from the group or
deactivating that user clears the assignment. Attempts to assign a non-member
or inactive user return `400`.

## Review and approval API

An editor with `view_edit` submits a review for one action. The plan is computed
by the migration service; clients cannot supply SQL or alter its snapshot.

```http
POST /databases/{databaseID}/migrations/definitions/{migrationID}/review
Content-Type: application/json
X-CSRF-Token: <session token>

{"action":"apply"}
```

The response is `201` with a workflow containing its ID, state, plan hash, and
the required group approvals. Actions are `apply`, `rollback`, and `forward`.

Read the latest state for a definition, including its implicit draft state:

```http
GET /databases/{databaseID}/migrations/definitions/{migrationID}/workflow
```

Read a particular submitted snapshot and its approvals:

```http
GET /databases/{databaseID}/migrations/workflows/{workflowID}
```

Any user with at least `view_only` access can read workflow details. A current
leader with access to the database approves through:

```http
POST /databases/{databaseID}/migrations/workflows/{workflowID}/approvals
X-CSRF-Token: <session token>
```

The response is `200` with the updated workflow. Non-leaders receive `403`;
stale policy or plan snapshots receive `409`. The approval action has no body.

After all required leaders approve, the editor submits the existing action:

```http
POST /databases/{databaseID}/migrations/definitions/{migrationID}/actions
Content-Type: application/json
X-CSRF-Token: <session token>

{"action":"apply","approval_id":"<workflowID>"}
```

The API returns `202` with a job ID. The worker repeats all approval checks;
approval is not treated as a long-lived permission grant.

## Storage

`migration_workflows` stores each immutable plan snapshot, its SHA-256 hash,
submitter, state, and the access policy epoch at submission. `migration_approvals`
stores one decision per workflow and group, tied to the exact leader user ID.
The access package owns `access_database_approval_epochs`. These tables live in
the platform PostgreSQL database and are initialized by the existing access and
migration schema initialization paths.

## Verification

Unit tests cover stable snapshot hashing and unique workflow IDs. PostgreSQL
integration coverage exercises leader assignment, view-only group approval,
workflow state changes, action submission through the queue, and invalidation
after a grant changes.
