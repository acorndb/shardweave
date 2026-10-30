package differential

// Scenario is a named, self-contained query script for differential comparison.
type Scenario struct {
	Name  string
	Steps []Step
}

// QueryScenarios assembles multi-step query scenes used by passthrough
// differential tests. Each scenario owns its tables and cleans up so scenes
// can run sequentially on the same schema pair.
func QueryScenarios() []Scenario {
	return []Scenario{
		{
			Name: "select_basic",
			Steps: []Step{
				{Name: "create", Exec: `CREATE TABLE s_basic (
					id INT PRIMARY KEY,
					name VARCHAR(64) NOT NULL,
					n INT
				)`},
				{Name: "insert", Exec: `INSERT INTO s_basic (id, name, n) VALUES
					(1, 'alpha', 10),
					(2, 'beta', 20),
					(3, 'gamma', NULL)`},
				{Name: "select-all", Query: `SELECT id, name, n FROM s_basic ORDER BY id`},
				{Name: "select-where", Query: `SELECT id, name FROM s_basic WHERE n >= 20 ORDER BY id`},
				{Name: "select-empty", Query: `SELECT id FROM s_basic WHERE id = 999`},
				{Name: "drop", Exec: `DROP TABLE s_basic`},
			},
		},
		{
			Name: "aggregate_group",
			Steps: []Step{
				{Name: "create", Exec: `CREATE TABLE s_agg (
					id INT PRIMARY KEY,
					g VARCHAR(8) NOT NULL,
					v INT NOT NULL
				)`},
				{Name: "insert", Exec: `INSERT INTO s_agg (id, g, v) VALUES
					(1, 'a', 1), (2, 'a', 3), (3, 'b', 5), (4, 'b', 7), (5, 'c', 9)`},
				{Name: "group-by", Query: `SELECT g, COUNT(*) AS cnt, SUM(v) AS s, AVG(v) AS avg_v
					FROM s_agg GROUP BY g ORDER BY g`},
				{Name: "having", Query: `SELECT g, SUM(v) AS s FROM s_agg GROUP BY g HAVING SUM(v) > 4 ORDER BY g`},
				{Name: "drop", Exec: `DROP TABLE s_agg`},
			},
		},
		{
			Name: "join_union",
			Steps: []Step{
				{Name: "create-a", Exec: `CREATE TABLE s_ja (id INT PRIMARY KEY, label VARCHAR(16))`},
				{Name: "create-b", Exec: `CREATE TABLE s_jb (id INT PRIMARY KEY, a_id INT, note VARCHAR(16))`},
				{Name: "insert-a", Exec: `INSERT INTO s_ja VALUES (1, 'one'), (2, 'two'), (3, 'three')`},
				{Name: "insert-b", Exec: `INSERT INTO s_jb VALUES (10, 1, 'x'), (11, 2, 'y'), (12, 9, 'z')`},
				{Name: "inner-join", Query: `SELECT a.id, a.label, b.note FROM s_ja a
					INNER JOIN s_jb b ON b.a_id = a.id ORDER BY a.id`},
				{Name: "left-join", Query: `SELECT a.id, a.label, b.note FROM s_ja a
					LEFT JOIN s_jb b ON b.a_id = a.id ORDER BY a.id, b.note`},
				{Name: "union-all", Query: `SELECT id AS k, label AS v FROM s_ja
					UNION ALL
					SELECT id, note FROM s_jb
					ORDER BY k, v`},
				{Name: "drop-b", Exec: `DROP TABLE s_jb`},
				{Name: "drop-a", Exec: `DROP TABLE s_ja`},
			},
		},
		{
			Name: "dml_update_delete",
			Steps: []Step{
				{Name: "create", Exec: `CREATE TABLE s_dml (id INT PRIMARY KEY, v INT NOT NULL)`},
				{Name: "insert", Exec: `INSERT INTO s_dml VALUES (1, 1), (2, 2), (3, 3), (4, 4)`},
				{Name: "update", Exec: `UPDATE s_dml SET v = v * 10 WHERE id IN (2, 4)`},
				{Name: "select-after-update", Query: `SELECT id, v FROM s_dml ORDER BY id`},
				{Name: "delete", Exec: `DELETE FROM s_dml WHERE v < 10`},
				{Name: "select-after-delete", Query: `SELECT id, v FROM s_dml ORDER BY id`},
				{Name: "drop", Exec: `DROP TABLE s_dml`},
			},
		},
		{
			Name: "transaction",
			Steps: []Step{
				{Name: "create", Exec: `CREATE TABLE s_tx (id INT PRIMARY KEY, v VARCHAR(8))`},
				{Name: "seed", Exec: `INSERT INTO s_tx VALUES (1, 'keep')`},
				{Name: "begin-rollback", Exec: `BEGIN`},
				{Name: "insert-temp", Exec: `INSERT INTO s_tx VALUES (2, 'tmp')`},
				{Name: "rollback", Exec: `ROLLBACK`},
				{Name: "after-rollback", Query: `SELECT id, v FROM s_tx ORDER BY id`},
				{Name: "begin-commit", Exec: `BEGIN`},
				{Name: "insert-ok", Exec: `INSERT INTO s_tx VALUES (3, 'ok')`},
				{Name: "commit", Exec: `COMMIT`},
				{Name: "after-commit", Query: `SELECT id, v FROM s_tx ORDER BY id`},
				{Name: "drop", Exec: `DROP TABLE s_tx`},
			},
		},
		{
			Name: "null_string_expr",
			Steps: []Step{
				{Name: "create", Exec: `CREATE TABLE s_null (
					id INT PRIMARY KEY,
					s VARCHAR(32),
					n INT
				)`},
				{Name: "insert", Exec: `INSERT INTO s_null VALUES
					(1, 'hello', 1),
					(2, NULL, NULL),
					(3, '世界', 3),
					(4, 'a''b', 4)`},
				{Name: "coalesce", Query: `SELECT id, COALESCE(s, '<nil>'), COALESCE(n, -1) FROM s_null ORDER BY id`},
				{Name: "is-null", Query: `SELECT id FROM s_null WHERE s IS NULL ORDER BY id`},
				{Name: "like", Query: `SELECT id, s FROM s_null WHERE s LIKE '%l%' ORDER BY id`},
				{Name: "drop", Exec: `DROP TABLE s_null`},
			},
		},
		{
			Name: "order_limit_distinct",
			Steps: []Step{
				{Name: "create", Exec: `CREATE TABLE s_ord (id INT PRIMARY KEY, g VARCHAR(8), v INT)`},
				{Name: "insert", Exec: `INSERT INTO s_ord VALUES
					(1, 'x', 5), (2, 'y', 1), (3, 'x', 5), (4, 'z', 9), (5, 'y', 1)`},
				{Name: "order-multi", Query: `SELECT id, g, v FROM s_ord ORDER BY v DESC, id ASC`},
				{Name: "limit", Query: `SELECT id FROM s_ord ORDER BY id LIMIT 2 OFFSET 2`},
				{Name: "distinct", Query: `SELECT DISTINCT g, v FROM s_ord ORDER BY g, v`},
				{Name: "drop", Exec: `DROP TABLE s_ord`},
			},
		},
		{
			Name: "prepared_params",
			Steps: []Step{
				{Name: "create", Exec: `CREATE TABLE s_prep (id INT PRIMARY KEY, name VARCHAR(32), n INT)`},
				{Name: "insert", Exec: `INSERT INTO s_prep VALUES (1, 'a', 10), (2, 'b', 20), (3, 'c', 30)`},
				{Name: "prepared-select", Prepared: &PreparedStep{
					Query: `SELECT id, name FROM s_prep WHERE n >= ? ORDER BY id`,
					Runs: []PreparedRun{
						{Args: []any{20}, Query: true},
						{Args: []any{100}, Query: true},
					},
				}},
				{Name: "prepared-update", Prepared: &PreparedStep{
					Query: `UPDATE s_prep SET n = n + ? WHERE id = ?`,
					Runs:  []PreparedRun{{Args: []any{5, 2}}},
				}},
				{Name: "after-update", Query: `SELECT id, n FROM s_prep ORDER BY id`},
				{Name: "drop", Exec: `DROP TABLE s_prep`},
			},
		},
		{
			Name: "errors",
			Steps: []Step{
				{Name: "create", Exec: `CREATE TABLE s_err (id INT PRIMARY KEY, name VARCHAR(8) NOT NULL)`},
				{Name: "insert-ok", Exec: `INSERT INTO s_err VALUES (1, 'ok')`},
				{Name: "dup-key", Exec: `INSERT INTO s_err VALUES (1, 'dup')`},
				// Successful step clears session warning state so the next error
				// is comparable (same pattern as CalibrationScript).
				{Name: "clear-after-dup", Query: `SELECT id FROM s_err ORDER BY id`},
				{Name: "missing-table", Query: `SELECT * FROM s_err_missing`},
				{Name: "clear-after-missing", Query: `SELECT 1`},
				{Name: "syntax", Query: `SELEKT 1`},
				{Name: "drop", Exec: `DROP TABLE s_err`},
			},
		},
		{
			Name: "session_vars",
			Steps: []Step{
				{Name: "set-names", Exec: `SET NAMES utf8mb4`},
				{Name: "set-user-vars", Exec: `SET @a := 1, @b := 'x'`},
				{Name: "read-vars", Query: `SELECT @a, @b, @missing`},
				{Name: "use-in-expr", Query: `SELECT @a + 2, CONCAT(@b, 'y')`},
			},
		},
	}
}
