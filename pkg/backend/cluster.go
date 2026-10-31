package backend

import (
	"fmt"
	"sort"
)

// Cluster is a named set of slices. It is the runtime topology after config
// validation; single/rwsplit currently build exactly one slice, sharding will
// build N.
type Cluster struct {
	slices map[string]*Slice
}

// NewCluster validates and builds a Cluster.
func NewCluster(slices ...*Slice) (*Cluster, error) {
	if len(slices) == 0 {
		return nil, fmt.Errorf("backend: cluster requires at least 1 slice")
	}
	m := make(map[string]*Slice, len(slices))
	for i, s := range slices {
		if s == nil {
			return nil, fmt.Errorf("backend: nil slice at index %d", i)
		}
		if s.Name == "" {
			return nil, fmt.Errorf("backend: slice[%d] name is required", i)
		}
		if _, ok := m[s.Name]; ok {
			return nil, fmt.Errorf("backend: duplicate slice %q", s.Name)
		}
		m[s.Name] = s
	}
	return &Cluster{slices: m}, nil
}

// Get returns the slice with name.
func (c *Cluster) Get(name string) (*Slice, error) {
	if c == nil {
		return nil, fmt.Errorf("backend: nil cluster")
	}
	s, ok := c.slices[name]
	if !ok {
		return nil, fmt.Errorf("backend: slice %q not found", name)
	}
	return s, nil
}

// Names returns slice names in deterministic order.
func (c *Cluster) Names() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.slices))
	for name := range c.slices {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Close closes all slices.
func (c *Cluster) Close() error {
	if c == nil {
		return nil
	}
	var first error
	for _, s := range c.slices {
		if err := s.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
