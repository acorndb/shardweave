package merge

import (
	"fmt"
	"strconv"

	"github.com/acorndb/shardweave/pkg/errs"
	"github.com/acorndb/shardweave/pkg/plan"
	"github.com/acorndb/shardweave/pkg/result"
)

// distinctAccumulator deduplicates raw aggregate arguments across shards and
// computes COUNT/SUM/AVG over the distinct set. Collated strings deduplicate by
// weight string (D3).
type distinctAccumulator struct {
	fn      plan.AggregateFunc
	keyKind valueKind
	keyColl stringCollator
	sumKind valueKind
	seen    map[string]struct{}
	count   uint64
	dec     *decimalSum
	flt     *floatSum
}

func newDistinctAccumulator(spec plan.AggregateSpec, field result.Field) (*distinctAccumulator, error) {
	switch spec.Func {
	case plan.AggCount, plan.AggSum, plan.AggAvg:
	default:
		return nil, errs.Unsupported("multi-shard aggregate DISTINCT function %d is not supported", spec.Func)
	}
	keyKind, keyColl, err := orderComparatorFor(field)
	if err != nil {
		return nil, err
	}
	d := &distinctAccumulator{
		fn:      spec.Func,
		keyKind: keyKind,
		keyColl: keyColl,
		seen:    make(map[string]struct{}),
	}
	if spec.Func == plan.AggSum || spec.Func == plan.AggAvg {
		kind, ok := sumKind(field)
		if !ok {
			return nil, errs.Unsupported("multi-shard DISTINCT aggregate on column %q type 0x%02x is not supported",
				field.Name, field.Type)
		}
		d.sumKind = kind
	}
	return d, nil
}

func (d *distinctAccumulator) add(v result.Value) error {
	if v == nil {
		return nil
	}
	key, err := encodeValueKey(v, d.keyKind, d.keyColl)
	if err != nil {
		return err
	}
	if _, ok := d.seen[key]; ok {
		return nil
	}
	d.seen[key] = struct{}{}
	d.count++
	if d.fn == plan.AggCount {
		return nil
	}
	if d.sumKind == kindFloat {
		if d.flt == nil {
			d.flt = &floatSum{}
		}
		return d.flt.add(v)
	}
	if d.dec == nil {
		d.dec = &decimalSum{}
	}
	return d.dec.add(v)
}

func (d *distinctAccumulator) value(outField result.Field) (result.Value, error) {
	switch d.fn {
	case plan.AggCount:
		return []byte(strconv.FormatUint(d.count, 10)), nil
	case plan.AggSum:
		if d.count == 0 {
			return nil, nil
		}
		if d.sumKind == kindFloat {
			return []byte(strconv.FormatFloat(d.flt.sum, 'g', -1, 64)), nil
		}
		return []byte(d.dec.rat.FloatString(int(outField.Decimals))), nil
	case plan.AggAvg:
		if d.count == 0 {
			return nil, nil
		}
		if d.sumKind == kindFloat {
			return []byte(strconv.FormatFloat(d.flt.sum/float64(d.count), 'g', -1, 64)), nil
		}
		avg := newRatQuo(d.dec.rat, d.count)
		return []byte(avg.FloatString(int(outField.Decimals))), nil
	default:
		return nil, fmt.Errorf("merge: unsupported DISTINCT aggregate %d", d.fn)
	}
}
