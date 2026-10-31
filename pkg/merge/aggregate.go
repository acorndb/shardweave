package merge

import (
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"

	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/result"
)

// Aggregate merges one aggregate row per shard into the final row.
// It supports COUNT/SUM/MIN/MAX/AVG without GROUP BY. DISTINCT COUNT/SUM/AVG
// read raw argument columns and deduplicate them across shards; AVG without
// DISTINCT needs a SUM/COUNT rewrite plus hidden columns.
func Aggregate(streams []result.Stream, fields []result.Field, specs []plan.AggregateSpec) (result.Stream, error) {
	if len(specs) == 0 {
		return nil, errs.Unsupported("multi-shard aggregate requires at least one aggregate")
	}
	if len(fields) < len(specs) {
		return nil, errs.Unsupported("multi-shard aggregate result has %d columns, want %d", len(fields), len(specs))
	}
	kinds := make([]valueKind, len(specs))
	distincts := make([]*distinctAccumulator, len(specs))
	for i, spec := range specs {
		if spec.Output < 0 || spec.Output >= len(specs) {
			return nil, errs.Unsupported("multi-shard aggregate output %d is out of range", spec.Output)
		}
		if spec.Column < 0 || spec.Column >= len(fields) {
			return nil, errs.Unsupported("multi-shard aggregate column %d is out of range", spec.Column)
		}
		if spec.Distinct {
			d, err := newDistinctAccumulator(spec, fields[spec.Column])
			if err != nil {
				return nil, err
			}
			distincts[i] = d
			continue
		}
		switch spec.Func {
		case plan.AggCount:
		case plan.AggSum:
			kind, ok := sumKind(fields[spec.Column])
			if !ok {
				return nil, errs.Unsupported("multi-shard SUM on column %q type 0x%02x is not supported",
					fields[spec.Column].Name, fields[spec.Column].Type)
			}
			kinds[i] = kind
		case plan.AggMin, plan.AggMax:
			kind, err := comparatorFor(fields[spec.Column])
			if err != nil {
				return nil, err
			}
			kinds[i] = kind
		case plan.AggAvg:
			if spec.CountColumn < 0 || spec.CountColumn >= len(fields) {
				return nil, errs.Unsupported("multi-shard AVG count column %d is out of range", spec.CountColumn)
			}
			kind, ok := sumKind(fields[spec.Column])
			if !ok {
				return nil, errs.Unsupported("multi-shard AVG on column %q type 0x%02x is not supported",
					fields[spec.Column].Name, fields[spec.Column].Type)
			}
			kinds[i] = kind
		default:
			return nil, errs.Unsupported("multi-shard aggregate function %d is not supported", spec.Func)
		}
	}

	kept := make([]result.Stream, 0, len(streams))
	for _, s := range streams {
		if s != nil {
			kept = append(kept, s)
		}
	}
	return &aggregate{
		streams:   kept,
		fields:    fields[:len(specs)],
		specs:     specs,
		kinds:     kinds,
		distincts: distincts,
	}, nil
}

func sumKind(f result.Field) (valueKind, bool) {
	switch f.Type {
	case result.TypeTiny, result.TypeShort, result.TypeLong, result.TypeLongLong, result.TypeInt24, result.TypeYear, result.TypeNewDecimal:
		return kindDecimal, true
	case result.TypeFloat, result.TypeDouble:
		return kindFloat, true
	default:
		return 0, false
	}
}

type aggregate struct {
	streams   []result.Stream
	fields    []result.Field
	specs     []plan.AggregateSpec
	kinds     []valueKind
	distincts []*distinctAccumulator

	row    result.Row
	done   bool
	read   bool
	closed bool
}

func (a *aggregate) Fields() []result.Field { return a.fields }

func (a *aggregate) Next() (result.Row, error) {
	if !a.read {
		a.read = true
		row, err := a.fold()
		if err != nil {
			return nil, err
		}
		a.row = row
	}
	if a.done {
		return nil, io.EOF
	}
	a.done = true
	return a.row, nil
}

func (a *aggregate) fold() (result.Row, error) {
	counts := make([]uint64, len(a.specs))
	avgCounts := make([]uint64, len(a.specs))
	decs := make([]*decimalSum, len(a.specs))
	floats := make([]*floatSum, len(a.specs))
	bests := make([]*bestValue, len(a.specs))

	for _, stream := range a.streams {
		for {
			row, err := stream.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			for i, spec := range a.specs {
				if spec.Column >= len(row) {
					return nil, fmt.Errorf("merge: aggregate column %d out of row width %d", spec.Column, len(row))
				}
				v := row[spec.Column]
				if spec.Distinct {
					if err := a.distincts[i].add(v); err != nil {
						return nil, err
					}
					continue
				}
				switch spec.Func {
				case plan.AggCount:
					if v == nil {
						continue
					}
					n, err := parseCount(v)
					if err != nil {
						return nil, err
					}
					counts[i] += n
				case plan.AggSum:
					if v == nil {
						continue
					}
					if a.kinds[i] == kindFloat {
						if floats[i] == nil {
							floats[i] = &floatSum{}
						}
						if err := floats[i].add(v); err != nil {
							return nil, err
						}
					} else {
						if decs[i] == nil {
							decs[i] = &decimalSum{}
						}
						if err := decs[i].add(v); err != nil {
							return nil, err
						}
					}
				case plan.AggMin, plan.AggMax:
					if v == nil {
						continue
					}
					if bests[i] == nil {
						bests[i] = &bestValue{}
					}
					if err := bests[i].add(v, a.kinds[i], spec.Func == plan.AggMax); err != nil {
						return nil, err
					}
				case plan.AggAvg:
					if v != nil {
						if a.kinds[i] == kindFloat {
							if floats[i] == nil {
								floats[i] = &floatSum{}
							}
							if err := floats[i].add(v); err != nil {
								return nil, err
							}
						} else {
							if decs[i] == nil {
								decs[i] = &decimalSum{}
							}
							if err := decs[i].add(v); err != nil {
								return nil, err
							}
						}
					}
					if spec.CountColumn >= len(row) {
						return nil, fmt.Errorf("merge: AVG count column %d out of row width %d", spec.CountColumn, len(row))
					}
					if cv := row[spec.CountColumn]; cv != nil {
						n, err := parseCount(cv)
						if err != nil {
							return nil, err
						}
						avgCounts[i] += n
					}
				}
			}
		}
	}

	out := make(result.Row, len(a.specs))
	for i, spec := range a.specs {
		if spec.Distinct {
			v, err := a.distincts[i].value(a.fields[spec.Output])
			if err != nil {
				return nil, err
			}
			out[spec.Output] = v
			continue
		}
		switch spec.Func {
		case plan.AggCount:
			out[spec.Output] = []byte(strconv.FormatUint(counts[i], 10))
		case plan.AggSum:
			if a.kinds[i] == kindFloat {
				if floats[i] != nil {
					out[spec.Output] = []byte(strconv.FormatFloat(floats[i].sum, 'g', -1, 64))
				}
			} else if decs[i] != nil {
				out[spec.Output] = []byte(decs[i].rat.FloatString(int(a.fields[spec.Output].Decimals)))
			}
		case plan.AggMin, plan.AggMax:
			if bests[i] != nil {
				out[spec.Output] = bests[i].raw
			}
		case plan.AggAvg:
			if avgCounts[i] == 0 {
				continue
			}
			if a.kinds[i] == kindFloat {
				if floats[i] != nil {
					out[spec.Output] = []byte(strconv.FormatFloat(
						floats[i].sum/float64(avgCounts[i]), 'g', -1, 64))
				}
			} else if decs[i] != nil {
				avg := new(big.Rat).Quo(decs[i].rat,
					new(big.Rat).SetInt(new(big.Int).SetUint64(avgCounts[i])))
				out[spec.Output] = []byte(avg.FloatString(int(a.fields[spec.Output].Decimals)))
			}
		}
	}
	return out, nil
}

func parseCount(v result.Value) (uint64, error) {
	b, ok := v.([]byte)
	if !ok {
		return 0, fmt.Errorf("merge: count value type %T", v)
	}
	n, err := strconv.ParseUint(string(b), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("merge: invalid count %q", b)
	}
	return n, nil
}

func (a *aggregate) Close() error {
	if a.closed {
		return nil
	}
	a.closed = true
	var first error
	for _, s := range a.streams {
		if err := s.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

type decimalSum struct {
	rat *big.Rat
}

func (d *decimalSum) add(v result.Value) error {
	b, ok := v.([]byte)
	if !ok {
		return fmt.Errorf("merge: decimal value type %T", v)
	}
	r, ok := new(big.Rat).SetString(string(b))
	if !ok {
		return fmt.Errorf("merge: invalid decimal %q", b)
	}
	if d.rat == nil {
		d.rat = r
	} else {
		d.rat.Add(d.rat, r)
	}
	return nil
}

type floatSum struct {
	sum float64
}

func (f *floatSum) add(v result.Value) error {
	b, ok := v.([]byte)
	if !ok {
		return fmt.Errorf("merge: float value type %T", v)
	}
	n, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		return fmt.Errorf("merge: invalid float %q", b)
	}
	f.sum += n
	return nil
}

type bestValue struct {
	raw []byte
	val orderValue
}

func (b *bestValue) add(v result.Value, kind valueKind, max bool) error {
	parsed, err := parseOrderValue(v, kind)
	if err != nil {
		return err
	}
	if b.raw == nil {
		b.raw = append([]byte(nil), v.([]byte)...)
		b.val = parsed
		return nil
	}
	cmp := compareOrderValues(parsed, b.val, kind)
	if (max && cmp > 0) || (!max && cmp < 0) {
		b.raw = append([]byte(nil), v.([]byte)...)
		b.val = parsed
	}
	return nil
}
