package merge

import (
	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/result"
)

// Filter applies HAVING predicates to merged group rows. Predicates are
// AND-only and compare one output column with a numeric literal; NULL never
// satisfies a comparison (MySQL HAVING semantics).
func Filter(upstream result.Stream, fields []result.Field, preds []plan.HavingPredicate) (result.Stream, error) {
	if len(preds) == 0 {
		return upstream, nil
	}
	resolved := make([]filterPredicate, len(preds))
	for i, pred := range preds {
		if pred.Column < 0 || pred.Column >= len(fields) {
			return nil, errs.Unsupported("HAVING column %d is out of range", pred.Column)
		}
		kind, err := comparatorFor(fields[pred.Column])
		if err != nil {
			return nil, err
		}
		value, err := parseOrderValue([]byte(pred.Value), kind)
		if err != nil {
			return nil, errs.Unsupported("HAVING literal %q is not valid for column %q", pred.Value, fields[pred.Column].Name)
		}
		resolved[i] = filterPredicate{col: pred.Column, op: pred.Op, value: value, kind: kind}
	}
	return &filterStream{upstream: upstream, fields: fields, preds: resolved}, nil
}

type filterPredicate struct {
	col   int
	op    plan.HavingOp
	value orderValue
	kind  valueKind
}

type filterStream struct {
	upstream result.Stream
	fields   []result.Field
	preds    []filterPredicate
	closed   bool
}

func (f *filterStream) Fields() []result.Field { return f.fields }

func (f *filterStream) Next() (result.Row, error) {
	for {
		row, err := f.upstream.Next()
		if err != nil {
			return nil, err
		}
		ok, err := f.match(row)
		if err != nil {
			return nil, err
		}
		if ok {
			return row, nil
		}
	}
}

func (f *filterStream) match(row result.Row) (bool, error) {
	for _, pred := range f.preds {
		if pred.col >= len(row) {
			return false, errs.Unsupported("HAVING column %d exceeds row width %d", pred.col, len(row))
		}
		v, err := parseOrderValue(row[pred.col], pred.kind)
		if err != nil {
			return false, err
		}
		if v.null {
			return false, nil
		}
		c := compareOrderValues(v, pred.value, pred.kind, nil)
		switch pred.op {
		case plan.HavingEQ:
			if c != 0 {
				return false, nil
			}
		case plan.HavingNE:
			if c == 0 {
				return false, nil
			}
		case plan.HavingLT:
			if c >= 0 {
				return false, nil
			}
		case plan.HavingLE:
			if c > 0 {
				return false, nil
			}
		case plan.HavingGT:
			if c <= 0 {
				return false, nil
			}
		case plan.HavingGE:
			if c < 0 {
				return false, nil
			}
		default:
			return false, errs.Unsupported("HAVING operator %d is not supported", pred.op)
		}
	}
	return true, nil
}

func (f *filterStream) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	return f.upstream.Close()
}
