package provider

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/varroaci/varroa-jenkins/internal/agent"
)

// Factory builds the adapter for one endpoint.
type Factory func(e Endpoint) (Provider, error)

// Entry is one configured provider with its adapter.
type Entry struct {
	Endpoint Endpoint
	Provider Provider
}

// Registry holds the configured providers in config order. It reads a key
// file on every LoadKey call, so a rotated key takes effect without a restart.
type Registry struct {
	cfg     Config
	entries []Entry
	byName  map[string]int
	keyDir  string
}

// NewRegistry builds one adapter for each endpoint with the factory of its
// kind. It fails when a kind has no factory.
func NewRegistry(c Config, factories map[agent.ProviderKind]Factory, keyDir string) (*Registry, error) {
	r := &Registry{cfg: c, byName: map[string]int{}, keyDir: keyDir}
	for _, e := range c.Providers {
		f, ok := factories[e.Kind]
		if !ok || f == nil {
			return nil, fmt.Errorf("provider %s: no factory for kind %q", e.Name, e.Kind)
		}
		p, err := f(e)
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", e.Name, err)
		}
		r.byName[e.Name] = len(r.entries)
		r.entries = append(r.entries, Entry{Endpoint: e, Provider: p})
	}
	return r, nil
}

// Config returns the decoded configuration the registry was built from.
func (r *Registry) Config() Config { return r.cfg }

// Get returns the entry with the name.
func (r *Registry) Get(name string) (Entry, bool) {
	i, ok := r.byName[name]
	if !ok {
		return Entry{}, false
	}
	return r.entries[i], true
}

// Default returns the entry marked default.
func (r *Registry) Default() (Entry, bool) {
	for _, e := range r.entries {
		if e.Endpoint.Default {
			return e, true
		}
	}
	return Entry{}, false
}

// List returns every entry in config order.
func (r *Registry) List() []Entry {
	out := make([]Entry, len(r.entries))
	copy(out, r.entries)
	return out
}

// HasModel reports whether the provider exists and lists the model.
func (r *Registry) HasModel(provider, model string) bool {
	e, ok := r.Get(provider)
	if !ok {
		return false
	}
	for _, m := range e.Endpoint.Models {
		if m == model {
			return true
		}
	}
	return false
}

// Available reports whether the provider exists and can be called. A provider
// with a secret needs a non-empty key file.
func (r *Registry) Available(name string) bool {
	e, ok := r.Get(name)
	if !ok {
		return false
	}
	if !e.Endpoint.HasSecret {
		return true
	}
	k, err := r.LoadKey(name)
	return err == nil && !k.Empty()
}

// LoadKey reads the key file of the provider on every call. A provider with no
// secret gives an empty Key and no error.
func (r *Registry) LoadKey(name string) (Key, error) {
	e, ok := r.Get(name)
	if !ok {
		return Key{}, fmt.Errorf("%w: unknown provider %q", ErrKeyUnavailable, name)
	}
	if !e.Endpoint.HasSecret {
		return Key{}, nil
	}
	raw, err := os.ReadFile(filepath.Join(r.keyDir, name, "apiKey"))
	if err != nil {
		return Key{}, fmt.Errorf("%w: read key for %s: %v", ErrKeyUnavailable, name, err) //nolint:errorlint // the sentinel is the caller-visible error
	}
	return NewKey(bytes.TrimSpace(raw)), nil
}
