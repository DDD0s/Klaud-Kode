package backend

import (
	"fmt"
	"sort"
	"strings"
)

// Registry is the set of enabled CLI backends and the default route.
type Registry struct {
	List    []Backend
	Default Backend
	byName  map[string]Backend
}

// NewRegistry validates the enabled set and advertised model ids.
func NewRegistry(list []Backend, defaultName string) (*Registry, error) {
	if len(list) == 0 {
		return nil, fmt.Errorf("at least one backend must be enabled")
	}
	byName := map[string]Backend{}
	ids := map[string]bool{}
	for _, b := range list {
		byName[b.Name()] = b
		for _, id := range b.IDs() {
			if ids[id] {
				return nil, fmt.Errorf("model id %s is advertised by two backends", id)
			}
			ids[id] = true
		}
	}
	def := byName[defaultName]
	if def == nil {
		return nil, fmt.Errorf("KOMPUTER_DEFAULT_BACKEND must be one of KOMPUTER_BACKENDS")
	}
	return &Registry{List: list, Default: def, byName: byName}, nil
}

// Get returns a backend by name, defaulting empty names to Claude for
// records written before backends existed.
func (r *Registry) Get(name string) Backend {
	if name == "" {
		name = "claude"
	}
	return r.byName[name]
}

// IDs returns every advertised model id in backend order.
func (r *Registry) IDs() []string {
	var out []string
	for _, b := range r.List {
		out = append(out, b.IDs()...)
	}
	return out
}

// Names returns enabled backend names in configuration order.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.List))
	for _, b := range r.List {
		out = append(out, b.Name())
	}
	return out
}

// Route picks the backend for a requested model id: exact advertised ids
// first, then backend name patterns with Claude's broader aliases last.
func (r *Registry) Route(model string) Backend {
	id := strings.TrimSpace(model)
	if id == "" {
		return r.Default
	}
	for _, b := range r.List {
		for _, advertised := range b.IDs() {
			if advertised == id {
				return b
			}
		}
	}
	ordered := append([]Backend{}, r.List...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Name() != "claude" && ordered[j].Name() == "claude"
	})
	for _, b := range ordered {
		if b.Owns(id) {
			return b
		}
	}
	return r.Default
}
