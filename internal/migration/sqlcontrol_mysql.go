package migration

import (
	"strings"

	"github.com/wafhr/IF3110_K01-13/internal/database"
)

// transactionControlFor memilih aturan parser sesuai DBMS target. Runner
// mengelola transaksi sendiri, sehingga SQL migration tidak boleh membuka,
// menutup, atau mengubah mode transaksi.
func transactionControlFor(dbms database.DBMS, statement string) bool {
	if dbms == database.MySQL {
		return mysqlTransactionControl(statement)
	}
	return transactionControl(statement)
}

// mysqlTransactionControl memakai aturan komentar MySQL: '#' dan "-- " adalah
// komentar baris, komentar blok tidak bersarang, dan komentar "/*!" dijalankan
// sebagai SQL. Kedua mode backslash diperiksa seperti pada PostgreSQL.
func mysqlTransactionControl(statement string) bool {
	for _, backslashEscapes := range []bool{false, true} {
		if mysqlExecutableCommentControl(statement, backslashEscapes) ||
			mysqlStatementsContainControl(statement, backslashEscapes) {
			return true
		}
	}
	return false
}

func mysqlStatementsContainControl(statement string, backslashEscapes bool) bool {
	for position := 0; position < len(statement); {
		position = mysqlSkipTrivia(statement, position)
		if position >= len(statement) {
			return false
		}
		first, end := sqlWord(statement, position)
		second, secondEnd := sqlWord(statement, mysqlSkipTrivia(statement, end))
		if mysqlForbiddenStatement(statement, first, second, end, secondEnd, backslashEscapes) {
			return true
		}
		if end == position {
			end++
		}
		position = mysqlNextStatement(statement, end, backslashEscapes)
	}
	return false
}

func mysqlForbiddenStatement(statement, first, second string, firstEnd, secondEnd int, backslashEscapes bool) bool {
	switch first {
	case "BEGIN", "START", "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE", "XA":
		return true
	case "LOCK", "UNLOCK":
		return second == "TABLES" || second == "TABLE" || second == "INSTANCE"
	case "SET":
		if second == "TRANSACTION" || mysqlStatementHasWord(statement, firstEnd, backslashEscapes, "AUTOCOMMIT") {
			return true
		}
		if second == "LOCAL" || second == "SESSION" || second == "GLOBAL" || second == "PERSIST" {
			third, _ := sqlWord(statement, mysqlSkipTrivia(statement, secondEnd))
			return third == "TRANSACTION"
		}
	}
	return false
}

// mysqlStatementHasWord mencari kata dalam statement saat ini di luar literal,
// misalnya "SET @x = 1, autocommit = 0".
func mysqlStatementHasWord(statement string, position int, backslashEscapes bool, target string) bool {
	for position < len(statement) {
		position = mysqlSkipTrivia(statement, position)
		if position >= len(statement) || statement[position] == ';' {
			return false
		}
		if quote := statement[position]; quote == '\'' || quote == '"' || quote == '`' {
			position = skipSQLQuoted(statement, position, quote, backslashEscapes && quote != '`')
			continue
		}
		word, end := sqlWord(statement, position)
		if strings.Contains(word, target) {
			return true
		}
		if end == position {
			end++
		}
		position = end
	}
	return false
}

// mysqlExecutableCommentControl menolak komentar "/*! ... */" yang berisi
// kendali transaksi karena MySQL menjalankan isi komentar tersebut.
func mysqlExecutableCommentControl(statement string, backslashEscapes bool) bool {
	for position := 0; position < len(statement); {
		start := strings.Index(statement[position:], "/*!")
		if start < 0 {
			return false
		}
		start += position + 3
		end := strings.Index(statement[start:], "*/")
		if end < 0 {
			end = len(statement) - start
		}
		end += start
		body := strings.TrimLeft(statement[start:end], "0123456789 \t\r\n")
		if strings.Contains(strings.ToUpper(body), "AUTOCOMMIT") || mysqlStatementsContainControl(body, backslashEscapes) {
			return true
		}
		position = end + 2
	}
	return false
}

func mysqlSkipTrivia(statement string, position int) int {
	for position < len(statement) {
		switch {
		case strings.ContainsRune(" \t\r\n\f\v", rune(statement[position])):
			position++
		case statement[position] == '#':
			position = skipSQLLine(statement, position+1)
		case mysqlLineComment(statement, position):
			position = skipSQLLine(statement, position+2)
		case strings.HasPrefix(statement[position:], "/*") && !strings.HasPrefix(statement[position:], "/*!"):
			position = mysqlSkipBlockComment(statement, position)
		default:
			return position
		}
	}
	return position
}

// MySQL hanya menganggap "--" sebagai komentar jika diikuti spasi atau kontrol.
func mysqlLineComment(statement string, position int) bool {
	return strings.HasPrefix(statement[position:], "--") &&
		(position+2 == len(statement) || statement[position+2] <= ' ')
}

func mysqlSkipBlockComment(statement string, position int) int {
	end := strings.Index(statement[position+2:], "*/")
	if end < 0 {
		return len(statement)
	}
	return position + 2 + end + 2
}

func mysqlNextStatement(statement string, position int, backslashEscapes bool) int {
	for position < len(statement) {
		switch quote := statement[position]; {
		case quote == ';':
			return position + 1
		case quote == '\'' || quote == '"' || quote == '`':
			position = skipSQLQuoted(statement, position, quote, backslashEscapes && quote != '`')
		case quote == '#' || mysqlLineComment(statement, position):
			position = mysqlSkipTrivia(statement, position)
		case strings.HasPrefix(statement[position:], "/*!"):
			position += 3
		case strings.HasPrefix(statement[position:], "/*"):
			position = mysqlSkipBlockComment(statement, position)
		default:
			position++
		}
	}
	return len(statement)
}
