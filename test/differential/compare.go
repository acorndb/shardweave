package differential

import "fmt"

// Compare 逐字段对比两侧观测,返回差异描述列表;完全一致返回 nil。
func Compare(steps []Step, direct, proxied []StepResult) []string {
	if len(direct) != len(proxied) {
		return []string{fmt.Sprintf("结果数量不一致: 直连=%d 经代理=%d", len(direct), len(proxied))}
	}
	var diffs []string
	for i := range steps {
		diffs = append(diffs, compareStep(i, steps[i].Name, direct[i], proxied[i])...)
	}
	return diffs
}

func compareStep(idx int, name string, a, b StepResult) []string {
	var diffs []string
	add := func(field string, av, bv any) {
		if av != bv {
			diffs = append(diffs, fmt.Sprintf("步骤 %d(%s) %s: 直连=%v 经代理=%v", idx+1, name, field, av, bv))
		}
	}

	add("错误码", errNumber(a.Err), errNumber(b.Err))
	add("错误信息", errMessage(a.Err), errMessage(b.Err))
	add("affected_rows", a.Affected, b.Affected)
	add("last_insert_id", a.InsertID, b.InsertID)

	add("exec结果数", len(a.Execs), len(b.Execs))
	for j := range a.Execs {
		if j >= len(b.Execs) {
			break
		}
		add(fmt.Sprintf("exec[%d]错误码", j+1), errNumber(a.Execs[j].Err), errNumber(b.Execs[j].Err))
		add(fmt.Sprintf("exec[%d]affected", j+1), a.Execs[j].Affected, b.Execs[j].Affected)
		add(fmt.Sprintf("exec[%d]last_insert_id", j+1), a.Execs[j].InsertID, b.Execs[j].InsertID)
	}

	add("结果集数量", len(a.ResultSets), len(b.ResultSets))
	for j := range a.ResultSets {
		if j >= len(b.ResultSets) {
			break
		}
		ra, rb := a.ResultSets[j], b.ResultSets[j]
		add(fmt.Sprintf("结果集[%d]列数", j+1), len(ra.Columns), len(rb.Columns))
		for k := range ra.Columns {
			if k < len(rb.Columns) {
				add(fmt.Sprintf("结果集[%d]列名[%d]", j+1, k), ra.Columns[k], rb.Columns[k])
			}
		}
		add(fmt.Sprintf("结果集[%d]行数", j+1), len(ra.Rows), len(rb.Rows))
		for k := range ra.Rows {
			if k >= len(rb.Rows) {
				break
			}
			for c := range ra.Rows[k] {
				if c < len(rb.Rows[k]) {
					add(fmt.Sprintf("结果集[%d]行[%d]列[%d]", j+1, k+1, c), ra.Rows[k][c], rb.Rows[k][c])
				}
			}
		}
	}

	add("warnings数量", len(a.Warnings), len(b.Warnings))
	for j := range a.Warnings {
		if j < len(b.Warnings) {
			add(fmt.Sprintf("warning[%d]", j+1), a.Warnings[j], b.Warnings[j])
		}
	}
	return diffs
}

func errNumber(e *SQLError) any {
	if e == nil {
		return "OK"
	}
	return e.Number
}

func errMessage(e *SQLError) any {
	if e == nil {
		return ""
	}
	return e.Message
}
