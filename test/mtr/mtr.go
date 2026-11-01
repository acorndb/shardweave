// Package mtr implements a minimal mysql-test-runner-style golden test format:
// cases live in t/*.sql (single) and t_sharding/*.sql (4-shard merge), expected
// output in r/*.result and r_sharding/*.result. It supports the subset of
// mysqltest directives used by this repository:
//
//	--echo <text>
//	--error <code>[,<code>...]
//	--sorted_result
//	--replace_column <n> <value>
//	--disable_query_log / --enable_query_log
//	--source <path>
//
// Rows are rendered tab-separated (NULL as NULL); Exec statements render
// affected_rows. Golden files are recorded with `go test -update` against
// DIRECT_DSN, then compared against PROXY_DSN.
package mtr

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"
)

// Script is one parsed .sql case.
type Script struct {
	Steps []Step
}

// Step is either an echoed line or one statement with its directives.
type Step struct {
	Echo *string
	Stmt *Statement
}

// Statement is one executable SQL statement plus per-statement directives.
type Statement struct {
	SQL       string
	ExpectErr []uint16
	Sorted    bool
	Replace   map[int]string
	Log       bool
}

type pending struct {
	expectErr []uint16
	sorted    bool
	replace   map[int]string
	log       bool
}

// ParseFile parses path, expanding --source includes relative to its directory.
func ParseFile(path string) (Script, error) {
	return parseFile(path, map[string]bool{})
}

func parseFile(path string, seen map[string]bool) (Script, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Script{}, err
	}
	if seen[abs] {
		return Script{}, fmt.Errorf("mtr: source cycle at %s", path)
	}
	seen[abs] = true
	defer delete(seen, abs)

	raw, err := os.ReadFile(path)
	if err != nil {
		return Script{}, err
	}

	var (
		script Script
		state  = pending{log: true}
		buf    strings.Builder
	)
	emit := func(sqlText string) {
		stmt := &Statement{
			SQL:       normalizeSQL(sqlText),
			ExpectErr: state.expectErr,
			Sorted:    state.sorted,
			Replace:   state.replace,
			Log:       state.log,
		}
		script.Steps = append(script.Steps, Step{Stmt: stmt})
		state.expectErr = nil
		state.sorted = false
		state.replace = nil
	}

	lines := strings.Split(string(raw), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.TrimSpace(buf.String()) == "" && strings.HasPrefix(trimmed, "--") {
			if handled, err := applyDirective(trimmed, filepath.Dir(path), &script, &state, seen); handled {
				if err != nil {
					return Script{}, err
				}
				continue
			}
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
		complete, rest := splitSQL(buf.String())
		for _, s := range complete {
			emit(s)
		}
		buf.Reset()
		buf.WriteString(rest)
	}
	if strings.TrimSpace(buf.String()) != "" {
		emit(buf.String())
	}
	return script, nil
}

func applyDirective(line, dir string, script *Script, state *pending, seen map[string]bool) (bool, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false, nil
	}
	switch fields[0] {
	case "--echo":
		text := strings.TrimSpace(strings.TrimPrefix(line, "--echo"))
		echo := text
		script.Steps = append(script.Steps, Step{Echo: &echo})
		return true, nil
	case "--error":
		if len(fields) < 2 {
			return true, fmt.Errorf("mtr: --error needs a code list")
		}
		for _, part := range strings.Split(fields[1], ",") {
			n, err := strconv.ParseUint(part, 10, 16)
			if err != nil {
				return true, fmt.Errorf("mtr: bad --error code %q", part)
			}
			state.expectErr = append(state.expectErr, uint16(n))
		}
		return true, nil
	case "--sorted_result":
		state.sorted = true
		return true, nil
	case "--replace_column":
		if len(fields) < 3 {
			return true, fmt.Errorf("mtr: --replace_column needs <n> <value>")
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil || n < 1 {
			return true, fmt.Errorf("mtr: bad --replace_column index %q", fields[1])
		}
		if state.replace == nil {
			state.replace = map[int]string{}
		}
		state.replace[n] = strings.Join(fields[2:], " ")
		return true, nil
	case "--disable_query_log":
		state.log = false
		return true, nil
	case "--enable_query_log":
		state.log = true
		return true, nil
	case "--source":
		if len(fields) < 2 {
			return true, fmt.Errorf("mtr: --source needs a path")
		}
		included, err := parseFile(filepath.Join(dir, fields[1]), seen)
		if err != nil {
			return true, err
		}
		script.Steps = append(script.Steps, included.Steps...)
		return true, nil
	default:
		// Plain SQL comment, not a directive.
		return false, nil
	}
}

// splitSQL returns complete statements and the trailing unterminated text.
func splitSQL(s string) ([]string, string) {
	var out []string
	start, i := 0, 0
	for i < len(s) {
		switch c := s[i]; c {
		case '\'', '"', '`':
			quote := c
			i++
			for i < len(s) {
				if s[i] == '\\' && quote != '`' && i+1 < len(s) {
					i += 2
					continue
				}
				if s[i] == quote {
					if i+1 < len(s) && s[i+1] == quote {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		case '-':
			if i+1 < len(s) && s[i+1] == '-' {
				for i < len(s) && s[i] != '\n' {
					i++
				}
			} else {
				i++
			}
		case '#':
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case '/':
			if i+1 < len(s) && s[i+1] == '*' {
				i += 2
				for i+1 < len(s) && !(s[i] == '*' && s[i+1] == '/') {
					i++
				}
				i += 2
			} else {
				i++
			}
		case ';':
			if stmt := strings.TrimSpace(s[start:i]); stmt != "" {
				out = append(out, stmt)
			}
			i++
			start = i
		default:
			i++
		}
	}
	return out, s[start:]
}

func normalizeSQL(s string) string {
	var b strings.Builder
	quote := byte(0)
	space := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			b.WriteByte(c)
			if c == '\\' && quote != '`' && i+1 < len(s) {
				i++
				b.WriteByte(s[i])
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
			b.WriteByte(c)
			space = false
		case ' ', '\t', '\n', '\r':
			if !space && b.Len() > 0 {
				b.WriteByte(' ')
				space = true
			}
		default:
			b.WriteByte(c)
			space = false
		}
	}
	return strings.TrimSpace(b.String())
}

// Run executes script on dsn and returns the golden text.
func Run(ctx context.Context, dsn string, script Script) (string, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return "", err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	conn, err := db.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	var out strings.Builder
	for _, step := range script.Steps {
		if step.Echo != nil {
			out.WriteString(*step.Echo)
			out.WriteByte('\n')
			continue
		}
		stmt := step.Stmt
		if stmt == nil {
			continue
		}
		if stmt.Log {
			out.WriteString(stmt.SQL)
			out.WriteString(";\n")
		}
		if err := runStatement(ctx, conn, stmt, &out); err != nil {
			return "", err
		}
	}
	return out.String(), nil
}

func runStatement(ctx context.Context, conn *sql.Conn, stmt *Statement, out *strings.Builder) error {
	if isQuery(stmt.SQL) {
		rows, err := conn.QueryContext(ctx, stmt.SQL)
		if err != nil {
			return writeError(stmt, err, out)
		}
		defer rows.Close()

		first := true
		for {
			columns, err := rows.Columns()
			if err != nil {
				return err
			}
			types, err := rows.ColumnTypes()
			if err != nil {
				return err
			}
			var data [][]string
			for rows.Next() {
				values := make([]any, len(columns))
				ptrs := make([]any, len(columns))
				for i := range values {
					ptrs[i] = &values[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					return err
				}
				row := make([]string, len(columns))
				for i := range values {
					dbType := ""
					if i < len(types) {
						dbType = types[i].DatabaseTypeName()
					}
					row[i] = formatValue(values[i], dbType)
				}
				applyReplace(row, stmt.Replace)
				data = append(data, row)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			if stmt.Sorted {
				sort.SliceStable(data, func(i, j int) bool {
					return strings.Join(data[i], "\t") < strings.Join(data[j], "\t")
				})
			}
			if !first {
				out.WriteByte('\n')
			}
			first = false
			out.WriteString(strings.Join(columns, "\t"))
			out.WriteByte('\n')
			for _, row := range data {
				out.WriteString(strings.Join(row, "\t"))
				out.WriteByte('\n')
			}
			if !rows.NextResultSet() {
				break
			}
		}
		if len(stmt.ExpectErr) > 0 {
			return fmt.Errorf("mtr: %s: expected error %v but statement succeeded", stmt.SQL, stmt.ExpectErr)
		}
		return nil
	}

	res, err := conn.ExecContext(ctx, stmt.SQL)
	if err != nil {
		return writeError(stmt, err, out)
	}
	if len(stmt.ExpectErr) > 0 {
		return fmt.Errorf("mtr: %s: expected error %v but statement succeeded", stmt.SQL, stmt.ExpectErr)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "affected_rows: %d\n", affected)
	return nil
}

func writeError(stmt *Statement, err error, out *strings.Builder) error {
	var me *drivermysql.MySQLError
	if !errors.As(err, &me) {
		return err
	}
	for _, code := range stmt.ExpectErr {
		if code == me.Number {
			fmt.Fprintf(out, "ERROR %d (%s): %s\n", me.Number, string(me.SQLState[:]), me.Message)
			return nil
		}
	}
	return fmt.Errorf("mtr: %s: unexpected error %d: %s", stmt.SQL, me.Number, me.Message)
}

func isQuery(sqlText string) bool {
	fields := strings.Fields(sqlText)
	if len(fields) == 0 {
		return false
	}
	switch strings.ToUpper(fields[0]) {
	case "SELECT", "SHOW", "DESC", "DESCRIBE", "EXPLAIN", "WITH", "TABLE", "VALUES", "CALL":
		return true
	default:
		return false
	}
}

func applyReplace(row []string, replace map[int]string) {
	for idx, value := range replace {
		if idx >= 1 && idx <= len(row) {
			row[idx-1] = value
		}
	}
}

func formatValue(v any, dbType string) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return escapeCell(string(x))
	case string:
		return escapeCell(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case time.Time:
		if dbType == "DATE" {
			return x.Format("2006-01-02")
		}
		return x.Format("2006-01-02 15:04:05.000000")
	default:
		return fmt.Sprint(x)
	}
}

func escapeCell(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\t", "\\t")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return strings.ReplaceAll(s, "\r", "\\r")
}
