package migration

import (
	"testing"

	"github.com/wafhr/IF3110_K01-13/internal/database"
)

func TestMySQLTransactionControl(t *testing.T) {
	tests := []struct {
		name      string
		statement string
		want      bool
	}{
		{name: "plain DDL", statement: "CREATE TABLE orders (id INT PRIMARY KEY)", want: false},
		{name: "multiple data statements", statement: "INSERT INTO orders VALUES (1); UPDATE orders SET id = 2", want: false},
		{name: "keyword inside string", statement: "INSERT INTO notes VALUES ('COMMIT; SET autocommit=0')", want: false},
		{name: "keyword inside backticks", statement: "CREATE TABLE `commit` (id INT)", want: false},
		{name: "keyword in comment", statement: "-- COMMIT\nUPDATE orders SET id = 1 /* ROLLBACK */", want: false},
		{name: "user variable", statement: "SET @next = 1; UPDATE orders SET id = @next", want: false},
		{name: "commit", statement: "UPDATE orders SET id = 1; COMMIT", want: true},
		{name: "start transaction", statement: "START TRANSACTION", want: true},
		{name: "xa", statement: "XA START 'x'", want: true},
		{name: "savepoint", statement: "SAVEPOINT before_update", want: true},
		{name: "autocommit", statement: "SET autocommit = 0", want: true},
		{name: "autocommit after variable", statement: "SET @x = 1, autocommit = 0", want: true},
		{name: "session autocommit", statement: "SET SESSION autocommit = 0", want: true},
		{name: "set transaction", statement: "SET SESSION TRANSACTION ISOLATION LEVEL SERIALIZABLE", want: true},
		{name: "lock tables", statement: "LOCK TABLES orders WRITE", want: true},
		{name: "unlock tables", statement: "UNLOCK TABLES", want: true},
		{name: "executable comment", statement: "/*!40101 COMMIT */", want: true},
		{name: "executable autocommit", statement: "/*!50000 SET autocommit=0 */", want: true},
		{name: "hash comment hides nothing", statement: "UPDATE orders SET id = 1 # note\n; COMMIT", want: true},
		{name: "dash without space is not a comment", statement: "UPDATE orders SET id = 1--1\n; COMMIT", want: true},
		{name: "block comments do not nest", statement: "/* /* */ COMMIT; */", want: true},
		{name: "backslash escaped quote", statement: `INSERT INTO notes VALUES ('it\'s'); COMMIT`, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := transactionControlFor(database.MySQL, test.statement); got != test.want {
				t.Fatalf("transactionControlFor(MySQL, %q) = %t, want %t", test.statement, got, test.want)
			}
		})
	}
}

func TestTransactionControlForUsesDialect(t *testing.T) {
	if !transactionControlFor(database.PostgreSQL, "SELECT 1 # 2; COMMIT") {
		t.Fatal("PostgreSQL parser hid COMMIT after the # operator")
	}
	if !transactionControlFor(database.PostgreSQL, "BEGIN") || transactionControlFor(database.PostgreSQL, "CREATE TABLE t (id INT)") {
		t.Fatal("PostgreSQL rules changed")
	}
}
