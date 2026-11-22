package migration

import (
	"strings"
)

// Kedua mode quoting diperiksa agar pengaturan backslash target tidak dapat
// menyembunyikan kendali transaksi dari runner PostgreSQL.
func transactionControl(statement string) bool {
	return sqlStatementsContainTransactionControl(statement, false) || sqlStatementsContainTransactionControl(statement, true)
}

func sqlStatementsContainTransactionControl(statement string, backslashEscapes bool) bool {
	for position := 0; position < len(statement); {
		position = skipSQLTrivia(statement, position)
		if position >= len(statement) {
			return false
		}
		first, end := sqlWord(statement, position)
		next := skipSQLTrivia(statement, end)
		second, secondEnd := sqlWord(statement, next)
		switch first {
		case "BEGIN", "START", "COMMIT", "END", "ROLLBACK", "ABORT", "SAVEPOINT", "RELEASE", "PREPARE":
			return true
		case "SET":
			if second == "TRANSACTION" {
				return true
			}
			if second == "LOCAL" || second == "SESSION" {
				third, _ := sqlWord(statement, skipSQLTrivia(statement, secondEnd))
				if third == "TRANSACTION" {
					return true
				}
			}
		}
		position = nextSQLStatement(statement, end, backslashEscapes)
	}
	return false
}

func sqlWord(statement string, position int) (string, int) {
	start := position
	for position < len(statement) {
		b := statement[position]
		if (b < 'A' || b > 'Z') && (b < 'a' || b > 'z') && (b < '0' || b > '9') &&
			b != '_' && b != '$' && b != '@' && b != '.' {
			break
		}
		position++
	}
	return strings.ToUpper(statement[start:position]), position
}

func skipSQLTrivia(statement string, position int) int {
	for position < len(statement) {
		if strings.ContainsRune(" \t\r\n\f\v", rune(statement[position])) {
			position++
			continue
		}
		if position+1 < len(statement) && statement[position:position+2] == "--" {
			position = skipSQLLine(statement, position+2)
			continue
		}
		if position+1 < len(statement) && statement[position:position+2] == "/*" {
			position = skipSQLBlockComment(statement, position)
			continue
		}
		break
	}
	return position
}

func skipSQLLine(statement string, position int) int {
	for position < len(statement) && statement[position] != '\n' && statement[position] != '\r' {
		position++
	}
	return position
}

func skipSQLBlockComment(statement string, position int) int {
	depth := 1
	position += 2
	for position < len(statement) && depth > 0 {
		if position+1 < len(statement) && statement[position:position+2] == "/*" {
			depth++
			position += 2
		} else if position+1 < len(statement) && statement[position:position+2] == "*/" {
			depth--
			position += 2
		} else {
			position++
		}
	}
	return position
}

func nextSQLStatement(statement string, position int, backslashEscapes bool) int {
	for position < len(statement) {
		if position+1 < len(statement) && statement[position:position+2] == "--" {
			position = skipSQLLine(statement, position+2)
			continue
		}
		if position+1 < len(statement) && statement[position:position+2] == "/*" {
			position = skipSQLBlockComment(statement, position)
			continue
		}
		if statement[position] == ';' {
			return position + 1
		}
		quote := statement[position]
		if quote == '\'' || quote == '"' {
			position = skipSQLQuoted(statement, position, quote, backslashEscapes)
			continue
		}
		if statement[position] == '$' &&
			(position == 0 || !isSQLWordByte(statement[position-1])) {
			if end := dollarQuoteEnd(statement, position); end > position {
				position = end
				continue
			}
		}
		position++
	}
	return len(statement)
}

func skipSQLQuoted(statement string, position int, quote byte, backslashEscapes bool) int {
	position++
	for position < len(statement) {
		if statement[position] == quote {
			if position+1 < len(statement) && statement[position+1] == quote {
				position += 2
				continue
			}
			return position + 1
		}
		if statement[position] == '\\' && backslashEscapes && position+1 < len(statement) {
			position += 2
			continue
		}
		position++
	}
	return position
}

func isSQLWordByte(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_'
}

func dollarQuoteEnd(statement string, position int) int {
	end := position + 1
	for end < len(statement) && isSQLWordByte(statement[end]) {
		end++
	}
	if end >= len(statement) || statement[end] != '$' || (end > position+1 && statement[position+1] >= '0' && statement[position+1] <= '9') {
		return position
	}
	delimiter := statement[position : end+1]
	closing := strings.Index(statement[end+1:], delimiter)
	if closing < 0 {
		return len(statement)
	}
	return end + 1 + closing + len(delimiter)
}
