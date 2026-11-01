// Package corpus loads SQL differential cases from YAML and runs them against
// a directly connected MySQL and the proxy. Cases own their tables; the fixed
// Docker fixture supplies a broad default schema (docker/init/schema.sql).
package corpus

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/acorndb/shardweave/test/differential"
)

// Case is one YAML corpus entry.
type Case struct {
	ID       string              `yaml:"id"`
	Setup    []string            `yaml:"setup"`
	Steps    []differential.Step `yaml:"steps"`
	Teardown []string            `yaml:"teardown"`
}

// Load reads every *.yaml under dir, sorted by path.
func Load(dir string) ([]Case, error) {
	var out []Case
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var cases []Case
		if err := yaml.Unmarshal(raw, &cases); err != nil {
			return fmt.Errorf("corpus: %s: %w", path, err)
		}
		for i := range cases {
			if cases[i].ID == "" {
				return fmt.Errorf("corpus: %s: case %d has no id", path, i)
			}
			if len(cases[i].Steps) == 0 {
				return fmt.Errorf("corpus: %s: case %q has no steps", path, cases[i].ID)
			}
		}
		out = append(out, cases...)
		return nil
	})
	return out, err
}

// Run executes one case. Both sides share the same MySQL in single mode, so
// mutating cases must reset (teardown + setup) before each side; otherwise the
// direct run's UPDATE/INSERT would be visible to the proxy run.
func Run(ctx context.Context, directDSN, proxyDSN string, c Case) ([]string, error) {
	runSide := func(dsn, label string) ([]differential.StepResult, error) {
		_ = execAll(ctx, directDSN, c.Teardown) // best-effort cleanup of a previous run
		if err := execAll(ctx, directDSN, c.Setup); err != nil {
			return nil, fmt.Errorf("%s setup: %w", label, err)
		}
		results, err := differential.RunScript(ctx, dsn, c.Steps)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}
		return results, nil
	}
	defer func() { _ = execAll(ctx, directDSN, c.Teardown) }()

	direct, err := runSide(directDSN, "direct")
	if err != nil {
		return nil, err
	}
	proxied, err := runSide(proxyDSN, "proxy")
	if err != nil {
		return nil, err
	}
	return differential.Compare(c.Steps, direct, proxied), nil
}

func execAll(ctx context.Context, dsn string, stmts []string) error {
	if len(stmts) == 0 {
		return nil
	}
	steps := make([]differential.Step, 0, len(stmts))
	for i, s := range stmts {
		steps = append(steps, differential.Step{Name: fmt.Sprintf("stmt-%d", i+1), Exec: s})
	}
	results, err := differential.RunScript(ctx, dsn, steps)
	if err != nil {
		return err
	}
	for i := range results {
		if results[i].Err != nil {
			return fmt.Errorf("stmt %d: %d %s", i+1, results[i].Err.Number, results[i].Err.Message)
		}
	}
	return nil
}
