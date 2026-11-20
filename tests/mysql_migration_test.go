//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"
	"github.com/wafhr/IF3110_K01-13/internal/database"
	"github.com/wafhr/IF3110_K01-13/internal/job"
	"github.com/wafhr/IF3110_K01-13/internal/migration"
	"github.com/wafhr/IF3110_K01-13/internal/targetdb/mysql"
	"github.com/wafhr/IF3110_K01-13/internal/worker"
)

// mysqlTestSettings reads the disposable MySQL server from TEST_MYSQL_DSN.
func mysqlTestSettings(t *testing.T) database.ConnectionConfig {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Fatal("TEST_MYSQL_DSN is required; see docs/database-management.md#integration-test")
	}
	config, err := drivermysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("TEST_MYSQL_DSN is not a valid MySQL connection string")
	}
	host, portText, err := net.SplitHostPort(config.Addr)
	if err != nil {
		t.Fatal("TEST_MYSQL_DSN must use a TCP address")
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal("TEST_MYSQL_DSN must include a numeric TCP port")
	}
	return database.ConnectionConfig{DBMS: database.MySQL, Host: host, Port: port,
		DatabaseName: config.DBName, Username: config.User, Password: config.Passwd, SSLMode: "disable"}
}

// queueActionOn mirrors queueAction for a database other than the fixture's
// PostgreSQL target. The test approval provider stands in for leader review.
func (f *migrationFixture) queueActionOn(t *testing.T, databaseID string, definition migration.Migration, action migration.Action) migration.ExecutionRequest {
	t.Helper()
	plan, err := f.migrations.PrepareAction(context.Background(), databaseID, definition.ID, action)
	if err != nil {
		t.Fatal(err)
	}
	approvalID := f.approve(plan)
	var receipt migration.QueuedAction
	decode(t, f.editor.do(t, "POST", "/databases/"+databaseID+"/migrations/definitions/"+definition.ID+"/actions", map[string]any{
		"action": action, "approval_id": approvalID,
	}, 202), &receipt)
	queued := f.jobFor(t, receipt.ExecutionID)
	if queued.Request.Plan != plan {
		t.Fatal("queued plan differs from the approved snapshot")
	}
	return queued.Request
}

func (f *migrationFixture) approve(plan migration.ActionPlan) string {
	f.approvals.mu.Lock()
	defer f.approvals.mu.Unlock()
	f.approvals.next++
	approvalID := fmt.Sprintf("00000000-0000-4000-8000-%012d", f.approvals.next)
	f.approvals.snapshots[approvalID] = migrationTestApproval{actorID: f.actorID, plan: plan}
	return approvalID
}

func (f *migrationFixture) createMigrationOn(t *testing.T, databaseID, name, up, down string) migration.Migration {
	t.Helper()
	var result struct {
		Migration migration.Migration `json:"migration"`
	}
	decode(t, f.editor.do(t, "POST", "/databases/"+databaseID+"/migrations/definitions", map[string]any{
		"name": name, "sql_up": up, "sql_down": down,
	}, 201), &result)
	return result.Migration
}

func TestMySQLMigrationExecutionIntegration(t *testing.T) {
	f := newMigrationFixture(t)
	settings := mysqlTestSettings(t)
	target, err := mysql.NewConnector().Open(context.Background(), settings)
	if err != nil {
		t.Fatalf("connect to disposable MySQL target: %v", err)
	}
	suffixBytes := make([]byte, 5)
	if _, err := rand.Read(suffixBytes); err != nil {
		t.Fatal(err)
	}
	table := "if3110_it_mysql_exec_" + hex.EncodeToString(suffixBytes)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = target.ExecContext(ctx, "DROP TABLE IF EXISTS `"+table+"`, `platform_migration_executions`, `platform_migrations`")
		_ = target.Close()
	})

	var registered struct {
		Database database.Database `json:"database"`
	}
	decode(t, f.platformAdmin.do(t, "POST", "/admin/databases", registration("MySQL migration target", settings), 201), &registered)
	databaseID := registered.Database.ID
	f.platformAdmin.do(t, "PUT", "/admin/groups/"+f.editorGroupID+"/databases/"+databaseID, map[string]any{"permission": "view_edit"}, 204)
	f.editor = f.login(t, "migration_view_edit", memberPassword)

	executor, err := worker.NewExecutor(f.openTarget, worker.MigrationPreparer(f.migrations))
	if err != nil {
		t.Fatal(err)
	}
	tableCount := "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = '" + table + "'"
	history := func(t *testing.T) migration.ExecutionHistory {
		t.Helper()
		var result migration.ExecutionHistory
		decode(t, f.editor.do(t, "GET", "/databases/"+databaseID+"/migrations/history", nil, 200), &result)
		return result
	}

	definition := f.createMigrationOn(t, databaseID, "create mysql table",
		"CREATE TABLE `"+table+"` (id INT PRIMARY KEY, note VARCHAR(40) NOT NULL) ENGINE=InnoDB",
		"DROP TABLE `"+table+"`")

	t.Run("apply, rollback, and forward commit with history", func(t *testing.T) {
		for _, step := range []struct {
			action migration.Action
			tables int
		}{{migration.Apply, 1}, {migration.Rollback, 0}, {migration.Forward, 1}} {
			request := f.queueActionOn(t, databaseID, definition, step.action)
			done := f.processJob(t, executor, request)
			if done.Status != job.Succeeded || done.Attempts != 1 {
				t.Fatalf("%s job = %+v", step.action, done)
			}
			assertCount(t, target, tableCount, step.tables)
			recorded := history(t).Executions
			if len(recorded) == 0 || recorded[0].ID != request.ExecutionID || recorded[0].Action != step.action ||
				recorded[0].Status != migration.Success || recorded[0].ExecutedBy != f.actorID || recorded[0].SQL != request.Plan.SQL ||
				recorded[0].FinishedAt == nil || recorded[0].Revision != 1 {
				t.Fatalf("%s history = %+v", step.action, recorded)
			}
		}
		if got := len(history(t).Executions); got != 3 {
			t.Fatalf("history has %d executions, want 3", got)
		}
		var detail struct {
			Execution migration.Execution `json:"execution"`
		}
		latest := history(t).Executions[0]
		decode(t, f.editor.do(t, "GET", "/databases/"+databaseID+"/migrations/history/"+latest.ID, nil, 200), &detail)
		if detail.Execution.SQL != latest.SQL || detail.Execution.Sequence != latest.Sequence {
			t.Fatalf("execution detail = %+v", detail.Execution)
		}
	})

	t.Run("failed SQL rolls back data and keeps history unchanged", func(t *testing.T) {
		before := len(history(t).Executions)
		broken := f.createMigrationOn(t, databaseID, "insert then fail",
			"INSERT INTO `"+table+"` (id, note) VALUES (1, 'kept only on success'); INSERT INTO if3110_missing_table VALUES (1)",
			"DELETE FROM `"+table+"` WHERE id = 1")
		done := f.processJob(t, executor, f.queueActionOn(t, databaseID, broken, migration.Apply))
		if done.Status != job.Failed || done.Attempts != 1 || !strings.HasPrefix(done.LastError, "MySQL error 1146") {
			t.Fatalf("failed job = %+v", done)
		}
		assertCount(t, target, "SELECT COUNT(*) FROM `"+table+"`", 0)
		if after := len(history(t).Executions); after != before {
			t.Fatalf("history grew from %d to %d after a failed attempt", before, after)
		}
	})

	t.Run("transaction control is rejected before queueing", func(t *testing.T) {
		for name, statement := range map[string]string{
			"autocommit":         "SET autocommit = 0; UPDATE `" + table + "` SET note = 'x'",
			"executable comment": "/*!40101 COMMIT */; UPDATE `" + table + "` SET note = 'x'",
			"lock tables":        "LOCK TABLES `" + table + "` WRITE",
			"hidden after hash":  "UPDATE `" + table + "` SET note = 'x' # comment\n; COMMIT",
		} {
			t.Run(name, func(t *testing.T) {
				rejected := f.createMigrationOn(t, databaseID, "reject "+name, statement, "SELECT 1")
				plan, err := f.migrations.PrepareAction(context.Background(), databaseID, rejected.ID, migration.Apply)
				if err != nil {
					t.Fatal(err)
				}
				f.editor.do(t, "POST", "/databases/"+databaseID+"/migrations/definitions/"+rejected.ID+"/actions", map[string]any{
					"action": migration.Apply, "approval_id": f.approve(plan),
				}, 400)
			})
		}
	})

	t.Run("batch migrations are rejected on MySQL", func(t *testing.T) {
		var batch struct {
			Migration migration.Migration `json:"migration"`
		}
		decode(t, f.editor.do(t, "POST", "/databases/"+databaseID+"/migrations/definitions", map[string]any{
			"name": "mysql batch", "sql_up": "UPDATE `" + table + "` SET note = 'b' WHERE id BETWEEN $1 AND $2",
			"sql_down": "UPDATE `" + table + "` SET note = 'a' WHERE id BETWEEN $1 AND $2",
			"batch":    map[string]any{"table": table, "key_column": "id", "size": 10},
		}, 201), &batch)
		if _, err := f.migrations.PrepareAction(context.Background(), databaseID, batch.Migration.ID, migration.Apply); !errors.Is(err, migration.ErrInvalidMigration) {
			t.Fatalf("batch plan on MySQL returned %v", err)
		}
	})

	t.Run("Milestone 1 history is imported once", func(t *testing.T) {
		if _, err := f.seedLegacyMigration(context.Background(), databaseID, f.actorID, "legacy mysql migration", "SELECT 1"); err != nil {
			t.Fatal(err)
		}
		first, second := history(t), history(t)
		legacy := 0
		for _, execution := range second.Executions {
			if execution.LegacyVersion != nil {
				legacy++
			}
		}
		if legacy != 1 || len(first.Executions) != len(second.Executions) {
			t.Fatalf("legacy import = %d entries, history sizes %d and %d", legacy, len(first.Executions), len(second.Executions))
		}
	})

	t.Run("the target holds the lock while a migration runs", func(t *testing.T) {
		conn, err := target.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var acquired sql.NullInt64
		if err := conn.QueryRowContext(context.Background(), `SELECT GET_LOCK(`+migration.MySQLMigrationLockName+`, 0)`).Scan(&acquired); err != nil || acquired.Int64 != 1 {
			t.Fatalf("lock = %v, %v", acquired, err)
		}
		blocked := f.createMigrationOn(t, databaseID, "waits for lock", "UPDATE `"+table+"` SET note = 'locked'", "UPDATE `"+table+"` SET note = 'unlocked'")
		request := f.queueActionOn(t, databaseID, blocked, migration.Apply)
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		if _, err := executor.Run(ctx, request, nil); err == nil {
			t.Fatal("migration ran while another session held the target lock")
		}
		if _, err := conn.ExecContext(context.Background(), `SELECT RELEASE_LOCK(`+migration.MySQLMigrationLockName+`)`); err != nil {
			t.Fatal(err)
		}
		if done := f.processJob(t, executor, request); done.Status != job.Succeeded {
			t.Fatalf("job after lock release = %+v", done)
		}
	})
}
