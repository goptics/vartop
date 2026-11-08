package discovery

import "sort"

// static is an immutable Source backed by a fixed endpoint list.
type static struct {
	endpoints []string
}

// NewStatic returns a Source with the given endpoints, sorted.
func NewStatic(endpoints []string) Source {
	sorted := make([]string, len(endpoints))
	copy(sorted, endpoints)
	sort.Strings(sorted)
	return &static{sorted}
}

func (s *static) Endpoints() []string {
	out := make([]string, len(s.endpoints))
	copy(out, s.endpoints)
	return out
}

func (s *static) Close() error { return nil }
