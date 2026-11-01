// Package sqlmatrix loads the SQL syntax support matrix used by the plan-level
// gate test. The matrix enumerates what single/rwsplit forwarding and sharding
// planning do for each syntax family; see docs/architecture/sql-support-matrix.md.
package sqlmatrix

import (
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"
)

// Expect is one expected plan outcome.
type Expect struct {
	Kind   string `yaml:"kind"`   // passthrough | single_shard | multi_shard | reject
	Reason string `yaml:"reason"` // substring of the rejection message
}

// Case is one SQL syntax probe.
type Case struct {
	Category string `yaml:"category"`
	SQL      string `yaml:"sql"`
	Single   Expect `yaml:"single"`
	Sharding Expect `yaml:"sharding"`
}

// Load reads and validates a matrix YAML file.
func Load(path string) ([]Case, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cases []Case
	if err := yaml.Unmarshal(raw, &cases); err != nil {
		return nil, fmt.Errorf("sqlmatrix: %w", err)
	}
	for i := range cases {
		if cases[i].Category == "" || cases[i].SQL == "" {
			return nil, fmt.Errorf("sqlmatrix: case %d needs category and sql", i)
		}
		for _, e := range []Expect{cases[i].Single, cases[i].Sharding} {
			switch e.Kind {
			case "passthrough", "single_shard", "multi_shard", "reject":
			default:
				return nil, fmt.Errorf("sqlmatrix: case %q has unknown kind %q", cases[i].SQL, e.Kind)
			}
		}
	}
	return cases, nil
}
