package merge

import (
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/result"
)

// Group merges per-shard GROUP BY rows. Each group key is accumulated in a map
// and emitted once; aggregate values reuse the same folding rules as Aggregate.
// Group keys must be numeric/decimal/date-text columns (string collation is
// rejected, as with ORDER BY).
func Group(
	streams []result.Stream,
	fields []result.Field,
	groupBy []int,
	specs []plan.AggregateSpec,
	output []int,
) (result.Stream, error) {
	if len(groupBy) == 0 {
		return nil, errs.Unsupported("multi-shard GROUP BY requires at least one column")
	}
	if len(output) == 0 {
		return nil, errs.Unsupported("multi-shard GROUP BY requires output columns")
	}
	outFields := make([]result.Field, len(output))
	for i, col := range output {
		if col < 0 || col >= len(fields) {
			return nil, errs.Unsupported("multi-shard output column %d is out of range", col)
		}
		outFields[i] = fields[col]
	}

	kinds := make([]valueKind, len(groupBy))
	outPos := make([]int, len(groupBy))
	covered := make([]bool, len(output))
	for i, col := range groupBy {
		if col < 0 || col >= len(fields) {
			return nil, errs.Unsupported("multi-shard GROUP BY column %d is out of range", col)
		}
		kind, err := comparatorFor(fields[col])
		if err != nil {
			return nil, err
		}
		kinds[i] = kind
		pos := outputIndex(output, col)
		if pos < 0 {
			return nil, errs.Unsupported("multi-shard GROUP BY column %d is not in the output", col)
		}
		outPos[i] = pos
		covered[pos] = true
	}

	for _, spec := range specs {
		if spec.Output < 0 || spec.Output >= len(output) {
			return nil, errs.Unsupported("multi-shard aggregate output %d is out of range", spec.Output)
		}
		if spec.Column < 0 || spec.Column >= len(fields) {
			return nil, errs.Unsupported("multi-shard aggregate column %d is out of range", spec.Column)
		}
		if spec.Distinct {
			if _, err := newDistinctAccumulator(spec, fields[spec.Column]); err != nil {
				return nil, err
			}
			covered[spec.Output] = true
			continue
		}
		switch spec.Func {
		case plan.AggCount:
		case plan.AggSum:
			if _, ok := sumKind(fields[spec.Column]); !ok {
				return nil, errs.Unsupported("multi-shard SUM on column %q type 0x%02x is not supported",
					fields[spec.Column].Name, fields[spec.Column].Type)
			}
		case plan.AggMin, plan.AggMax:
			if _, err := comparatorFor(fields[spec.Column]); err != nil {
				return nil, err
			}
		case plan.AggAvg:
			if spec.CountColumn < 0 || spec.CountColumn >= len(fields) {
				return nil, errs.Unsupported("multi-shard AVG count column %d is out of range", spec.CountColumn)
			}
			if _, ok := sumKind(fields[spec.Column]); !ok {
				return nil, errs.Unsupported("multi-shard AVG on column %q type 0x%02x is not supported",
					fields[spec.Column].Name, fields[spec.Column].Type)
			}
		default:
			return nil, errs.Unsupported("multi-shard aggregate function %d is not supported", spec.Func)
		}
		covered[spec.Output] = true
	}
	for i, ok := range covered {
		if !ok {
			return nil, errs.Unsupported("multi-shard output column %d has no group key or aggregate", output[i])
		}
	}

	kept := make([]result.Stream, 0, len(streams))
	for _, s := range streams {
		if s != nil {
			kept = append(kept, s)
		}
	}
	return &groupStream{
		streams:   kept,
		fields:    fields,
		groupBy:   groupBy,
		outPos:    outPos,
		kinds:     kinds,
		specs:     specs,
		outFields: outFields,
		output:    output,
		groups:    make(map[string]*groupState),
	}, nil
}

func outputIndex(output []int, col int) int {
	for i, c := range output {
		if c == col {
			return i
		}
	}
	return -1
}

type groupStream struct {
	streams []result.Stream
	fields  []result.Field
	groupBy []int
	outPos  []int
	kinds   []valueKind
	specs   []plan.AggregateSpec

	outFields []result.Field
	output    []int

	order  []string
	groups map[string]*groupState
	rows   []result.Row
	idx    int
	read   bool
	closed bool
}

func (g *groupStream) Fields() []result.Field { return g.outFields }

func (g *groupStream) Next() (result.Row, error) {
	if !g.read {
		g.read = true
		if err := g.fold(); err != nil {
			return nil, err
		}
	}
	if g.idx >= len(g.rows) {
		return nil, io.EOF
	}
	row := g.rows[g.idx]
	g.idx++
	return row, nil
}

func (g *groupStream) fold() error {
	for _, stream := range g.streams {
		for {
			row, err := stream.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			key, err := g.key(row)
			if err != nil {
				return err
			}
			state := g.groups[key]
			if state == nil {
				state = newGroupState(len(g.output), len(g.specs))
				for i, col := range g.groupBy {
					if col >= len(row) {
						return fmt.Errorf("merge: group column %d out of row width %d", col, len(row))
					}
					state.values[g.outPos[i]] = copyValue(row[col])
				}
				g.groups[key] = state
				g.order = append(g.order, key)
			}
			if err := state.add(row, g.specs, g.fields); err != nil {
				return err
			}
		}
	}

	g.rows = make([]result.Row, 0, len(g.order))
	for _, key := range g.order {
		state := g.groups[key]
		row := state.values
		for i, spec := range g.specs {
			v, err := state.value(i, spec, g.outFields[spec.Output])
			if err != nil {
				return err
			}
			row[spec.Output] = v
		}
		g.rows = append(g.rows, row)
	}
	return nil
}

func (g *groupStream) key(row result.Row) (string, error) {
	return encodeRowKey(row, g.groupBy, g.kinds)
}

// encodeRowKey builds a canonical key for numeric/date group/distinct values.
// String collations are rejected before this function is reached.
func encodeRowKey(row result.Row, cols []int, kinds []valueKind) (string, error) {
	var b strings.Builder
	for i, col := range cols {
		if col >= len(row) {
			return "", fmt.Errorf("merge: key column %d out of row width %d", col, len(row))
		}
		part, err := encodeValueKey(row[col], kinds[i])
		if err != nil {
			return "", fmt.Errorf("merge: key column %d: %w", col, err)
		}
		b.WriteString(strconv.Itoa(len(part)))
		b.WriteByte(':')
		b.WriteString(part)
		b.WriteByte('|')
	}
	return b.String(), nil
}

// encodeValueKey builds the canonical key for one numeric/date value. String
// collations are rejected before this function is reached.
func encodeValueKey(value result.Value, kind valueKind) (string, error) {
	v, err := parseOrderValue(value, kind)
	if err != nil {
		return "", err
	}
	switch {
	case v.null:
		return "n", nil
	case kind == kindInt:
		return "i:" + strconv.FormatInt(v.i, 10), nil
	case kind == kindUint:
		return "u:" + strconv.FormatUint(v.u, 10), nil
	case kind == kindFloat:
		f := v.f
		if f == 0 {
			f = 0 // normalize -0
		}
		return "f:" + strconv.FormatUint(math.Float64bits(f), 16), nil
	case kind == kindDecimal:
		return "d:" + v.r.RatString(), nil
	default:
		return "t:" + string(v.b), nil
	}
}

func (g *groupStream) Close() error {
	if g.closed {
		return nil
	}
	g.closed = true
	var first error
	for _, s := range g.streams {
		if err := s.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

type groupState struct {
	values    result.Row
	counts    []uint64
	avgCounts []uint64
	decs      []*decimalSum
	floats    []*floatSum
	bests     []*bestValue
	kinds     []valueKind
	distincts []*distinctAccumulator
}

func newGroupState(outputLen, specLen int) *groupState {
	return &groupState{
		values:    make(result.Row, outputLen),
		counts:    make([]uint64, specLen),
		avgCounts: make([]uint64, specLen),
		decs:      make([]*decimalSum, specLen),
		floats:    make([]*floatSum, specLen),
		bests:     make([]*bestValue, specLen),
		kinds:     make([]valueKind, specLen),
		distincts: make([]*distinctAccumulator, specLen),
	}
}

func (s *groupState) add(row result.Row, specs []plan.AggregateSpec, fields []result.Field) error {
	for i, spec := range specs {
		if spec.Column >= len(row) {
			return fmt.Errorf("merge: aggregate column %d out of row width %d", spec.Column, len(row))
		}
		v := row[spec.Column]
		if spec.Distinct {
			if s.distincts[i] == nil {
				d, err := newDistinctAccumulator(spec, fields[spec.Column])
				if err != nil {
					return err
				}
				s.distincts[i] = d
			}
			if err := s.distincts[i].add(v); err != nil {
				return err
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
				return err
			}
			s.counts[i] += n
		case plan.AggSum:
			if v == nil {
				continue
			}
			if s.kinds[i] == 0 && s.decs[i] == nil && s.floats[i] == nil {
				if kind, ok := sumKind(fields[spec.Column]); ok {
					s.kinds[i] = kind
				}
			}
			if s.kinds[i] == kindFloat {
				if s.floats[i] == nil {
					s.floats[i] = &floatSum{}
				}
				if err := s.floats[i].add(v); err != nil {
					return err
				}
			} else {
				if s.decs[i] == nil {
					s.decs[i] = &decimalSum{}
				}
				if err := s.decs[i].add(v); err != nil {
					return err
				}
			}
		case plan.AggMin, plan.AggMax:
			if v == nil {
				continue
			}
			if s.bests[i] == nil {
				s.bests[i] = &bestValue{}
			}
			kind, err := comparatorFor(fields[spec.Column])
			if err != nil {
				return err
			}
			if err := s.bests[i].add(v, kind, spec.Func == plan.AggMax); err != nil {
				return err
			}
		case plan.AggAvg:
			if v != nil {
				if s.kinds[i] == 0 && s.decs[i] == nil && s.floats[i] == nil {
					if kind, ok := sumKind(fields[spec.Column]); ok {
						s.kinds[i] = kind
					}
				}
				if s.kinds[i] == kindFloat {
					if s.floats[i] == nil {
						s.floats[i] = &floatSum{}
					}
					if err := s.floats[i].add(v); err != nil {
						return err
					}
				} else {
					if s.decs[i] == nil {
						s.decs[i] = &decimalSum{}
					}
					if err := s.decs[i].add(v); err != nil {
						return err
					}
				}
			}
			if spec.CountColumn >= len(row) {
				return fmt.Errorf("merge: AVG count column %d out of row width %d", spec.CountColumn, len(row))
			}
			if cv := row[spec.CountColumn]; cv != nil {
				n, err := parseCount(cv)
				if err != nil {
					return err
				}
				s.avgCounts[i] += n
			}
		}
	}
	return nil
}

func (s *groupState) value(i int, spec plan.AggregateSpec, outField result.Field) (result.Value, error) {
	if spec.Distinct {
		return s.distincts[i].value(outField)
	}
	switch spec.Func {
	case plan.AggCount:
		return []byte(strconv.FormatUint(s.counts[i], 10)), nil
	case plan.AggSum:
		if s.kinds[i] == kindFloat {
			if s.floats[i] == nil {
				return nil, nil
			}
			return []byte(strconv.FormatFloat(s.floats[i].sum, 'g', -1, 64)), nil
		}
		if s.decs[i] == nil {
			return nil, nil
		}
		return []byte(s.decs[i].rat.FloatString(int(outField.Decimals))), nil
	case plan.AggMin, plan.AggMax:
		if s.bests[i] == nil {
			return nil, nil
		}
		return s.bests[i].raw, nil
	case plan.AggAvg:
		if s.avgCounts[i] == 0 {
			return nil, nil
		}
		if s.kinds[i] == kindFloat {
			if s.floats[i] == nil {
				return nil, nil
			}
			return []byte(strconv.FormatFloat(s.floats[i].sum/float64(s.avgCounts[i]), 'g', -1, 64)), nil
		}
		if s.decs[i] == nil {
			return nil, nil
		}
		avg := newRatQuo(s.decs[i].rat, s.avgCounts[i])
		return []byte(avg.FloatString(int(outField.Decimals))), nil
	default:
		return nil, fmt.Errorf("merge: unsupported group aggregate %d", spec.Func)
	}
}

func newRatQuo(r *big.Rat, n uint64) *big.Rat {
	return new(big.Rat).Quo(r, new(big.Rat).SetInt(new(big.Int).SetUint64(n)))
}

func copyValue(v result.Value) result.Value {
	if v == nil {
		return nil
	}
	if b, ok := v.([]byte); ok {
		return append([]byte(nil), b...)
	}
	return v
}
