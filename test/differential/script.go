package differential

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Step 是会话脚本中的一步,Exec、Query、Prepared 三选一。
type Step struct {
	Name     string
	Exec     string
	Query    string
	Prepared *PreparedStep
}

// PreparedStep 对应一次 prepare 与若干次执行,覆盖二进制协议路径
// (COM_STMT_PREPARE / COM_STMT_EXECUTE)。同一步内的 runs 不混用 Query 与 Exec。
type PreparedStep struct {
	Query string
	Runs  []PreparedRun
}

// PreparedRun 是一次参数化执行;Query 为 true 时走 Query,否则走 Exec。
type PreparedRun struct {
	Args  []any
	Query bool
}

// StepResult 记录一步的完整观测,供两侧对比。
type StepResult struct {
	Err        *SQLError    // 语句级错误;nil 表示成功
	Affected   int64        // Exec 步骤
	InsertID   int64        // Exec 步骤
	ResultSets []ResultSet  // Query 步骤与 prepared 的 query runs
	Execs      []ExecResult // prepared 的 exec runs
	Warnings   []WarningRow // 步骤后 SHOW WARNINGS 的观测
}

// ResultSet 是一个结果集的归一化快照。
type ResultSet struct {
	Columns []string
	Rows    [][]string
}

// ExecResult 是 prepared exec run 的观测。
type ExecResult struct {
	Err      *SQLError
	Affected int64
	InsertID int64
}

// WarningRow 对应 SHOW WARNINGS 的一行。
type WarningRow struct {
	Level   string
	Code    string
	Message string
}

// SQLError 是 MySQL 错误的归一化表示;Number 为 0 表示非 MySQL 协议错误(驱动层错误)。
type SQLError struct {
	Number  uint16
	Message string
}

// RunScript 在 dsn 上顺序执行脚本并记录观测。整个脚本固定在一条连接上执行,
// 以保留会话语义(用户变量、last_insert_id、auto_increment 计数)。
func RunScript(ctx context.Context, dsn string, steps []Step) ([]StepResult, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if err := conn.PingContext(ctx); err != nil {
		return nil, err
	}

	results := make([]StepResult, 0, len(steps))
	for _, s := range steps {
		res := runStep(ctx, conn, s)
		res.Warnings = showWarnings(ctx, conn)
		results = append(results, res)
	}
	return results, nil
}

func runStep(ctx context.Context, conn *sql.Conn, s Step) StepResult {
	var res StepResult
	switch {
	case s.Exec != "":
		r, err := conn.ExecContext(ctx, s.Exec)
		if err != nil {
			res.Err = toSQLError(err)
			return res
		}
		res.Affected, _ = r.RowsAffected()
		res.InsertID, _ = r.LastInsertId()
	case s.Query != "":
		rows, err := conn.QueryContext(ctx, s.Query)
		if err != nil {
			res.Err = toSQLError(err)
			return res
		}
		defer rows.Close()
		for {
			rs, err := readResultSet(rows)
			if err != nil {
				if res.Err == nil {
					res.Err = toSQLError(err)
				}
				return res
			}
			res.ResultSets = append(res.ResultSets, rs)
			if !rows.NextResultSet() {
				return res
			}
		}
	case s.Prepared != nil:
		stmt, err := conn.PrepareContext(ctx, s.Prepared.Query)
		if err != nil {
			res.Err = toSQLError(err)
			return res
		}
		defer stmt.Close()
		for _, run := range s.Prepared.Runs {
			if run.Query {
				rows, err := stmt.QueryContext(ctx, run.Args...)
				if err != nil {
					if res.Err == nil {
						res.Err = toSQLError(err)
					}
					res.ResultSets = append(res.ResultSets, ResultSet{})
					continue
				}
				rs, err := readResultSet(rows)
				rows.Close()
				if err != nil && res.Err == nil {
					res.Err = toSQLError(err)
				}
				res.ResultSets = append(res.ResultSets, rs)
			} else {
				r, err := stmt.ExecContext(ctx, run.Args...)
				if err != nil {
					res.Execs = append(res.Execs, ExecResult{Err: toSQLError(err)})
					continue
				}
				affected, _ := r.RowsAffected()
				insertID, _ := r.LastInsertId()
				res.Execs = append(res.Execs, ExecResult{Affected: affected, InsertID: insertID})
			}
		}
	}
	return res
}

func readResultSet(rows *sql.Rows) (ResultSet, error) {
	cols, err := rows.Columns()
	if err != nil {
		return ResultSet{}, err
	}
	rs := ResultSet{Columns: cols}
	raw := make([]any, len(cols))
	scan := make([]any, len(cols))
	for i := range raw {
		scan[i] = &raw[i]
	}
	for rows.Next() {
		if err := rows.Scan(scan...); err != nil {
			return rs, err
		}
		row := make([]string, len(raw))
		for i, v := range raw {
			row[i] = normalize(v)
		}
		rs.Rows = append(rs.Rows, row)
	}
	return rs, rows.Err()
}

func showWarnings(ctx context.Context, conn *sql.Conn) []WarningRow {
	rows, err := conn.QueryContext(ctx, "SHOW WARNINGS")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []WarningRow
	for rows.Next() {
		var level, code, message string
		if err := rows.Scan(&level, &code, &message); err != nil {
			return out
		}
		out = append(out, WarningRow{Level: level, Code: code, Message: message})
	}
	return out
}

// normalize 把驱动扫描出的任意值归一化为确定性的字符串表示。
func normalize(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(x)
	case string:
		return x
	case time.Time:
		return x.Format(time.RFC3339Nano)
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case int64:
		return strconv.FormatInt(x, 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	default:
		return fmt.Sprintf("%v", x)
	}
}

func toSQLError(err error) *SQLError {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return &SQLError{Number: me.Number, Message: me.Message}
	}
	return &SQLError{Message: err.Error()}
}
