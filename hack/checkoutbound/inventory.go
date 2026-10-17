package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Inventory is the source of truth for every outbound destination.
type Inventory struct {
	Connections  []Connection  `yaml:"connections"`
	Identifiers  []Identifier  `yaml:"identifiers"`
	ExcludePaths []ExcludePath `yaml:"excludePaths"`
}

// Connection is one row of the generated page.
type Connection struct {
	Component   string   `yaml:"component"`
	Destination string   `yaml:"destination"`
	Hosts       []string `yaml:"hosts"`
	Purpose     string   `yaml:"purpose"`
	Protocol    string   `yaml:"protocol"`
	Control     string   `yaml:"control"`
	Disable     string   `yaml:"disable"`
}

// Identifier is a literal that a source holds but never dials.
type Identifier struct {
	Host   string `yaml:"host"`
	Reason string `yaml:"reason"`
}

// ExcludePath removes a source path prefix from the scan.
type ExcludePath struct {
	Path   string `yaml:"path"`
	Reason string `yaml:"reason"`
}

// componentSection names one table of the generated page, in page order.
type componentSection struct {
	Key   string
	Title string
}

var componentOrder = []componentSection{
	{"operator", "Operator"},
	{"gateway", "Gateway"},
	{"bff", "BFF"},
	{"updatecenter", "Update center"},
	{"recorder", "Recorder"},
	{"mite", "Mite"},
	{"jenkins", "Jenkins controller"},
	{"agents", "Jenkins agents"},
	{"dex", "Dex"},
	{"nats", "NATS"},
	{"varroactl", "varroactl"},
	{"cluster", "Cluster-wide"},
}

// loadInventory reads and validates the inventory file. Unknown keys fail,
// so a misspelled field cannot silently drop a row.
func loadInventory(path string) (*Inventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var inv Inventory
	if err := dec.Decode(&inv); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := inv.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &inv, nil
}

func (inv *Inventory) validate() error {
	valid := map[string]bool{}
	for _, c := range componentOrder {
		valid[c.Key] = true
	}
	for i, c := range inv.Connections {
		where := fmt.Sprintf("connections[%d] (%q)", i, c.Destination)
		if !valid[c.Component] {
			return fmt.Errorf("%s: unknown component %q", where, c.Component)
		}
		for name, value := range map[string]string{
			"destination": c.Destination, "purpose": c.Purpose,
			"protocol": c.Protocol, "control": c.Control, "disable": c.Disable,
		} {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("%s: field %s is empty", where, name)
			}
		}
		for _, h := range c.Hosts {
			ipv6 := strings.Contains(h, ":") && net.ParseIP(h) != nil
			if h != strings.ToLower(h) || strings.ContainsAny(h, "/@ ") || (strings.Contains(h, ":") && !ipv6) {
				return fmt.Errorf("%s: host %q is not a bare lowercase host name", where, h)
			}
		}
	}
	for i, id := range inv.Identifiers {
		if id.Host == "" || strings.TrimSpace(id.Reason) == "" {
			return fmt.Errorf("identifiers[%d]: host and reason are required", i)
		}
	}
	for i, ex := range inv.ExcludePaths {
		if ex.Path == "" || strings.TrimSpace(ex.Reason) == "" {
			return fmt.Errorf("excludePaths[%d]: path and reason are required", i)
		}
	}
	return nil
}

// isExcludedHost reports whether a host literal is not an external destination.
// The checker leaves such literals out of the scan. Each rule names one class:
// placeholders, names that never resolve on the internet, and loopback.
func isExcludedHost(host string) bool {
	h := strings.ToLower(host)
	switch {
	case h == "":
		return true
	case strings.ContainsAny(h, "%{}$< "):
		return true // a template or format verb, not a name
	case strings.HasSuffix(h, "."):
		return true // a trailing dot names no host in an allowlist
	case h == "localhost" || h == "0.0.0.0" || h == "::1":
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	if !strings.Contains(h, ".") {
		return true // a bare name is a cluster-local service or a loopback alias
	}
	for _, suffix := range []string{".svc", ".cluster.local"} {
		if strings.HasSuffix(h, suffix) {
			return true // in-cluster service names
		}
	}
	// RFC 2606 reserved names and RFC 6761 special-use names.
	for _, ref := range []string{"example.com", "example.net", "example.org"} {
		if h == ref || strings.HasSuffix(h, "."+ref) {
			return true
		}
	}
	for _, tld := range []string{".example", ".test", ".invalid", ".localhost", ".local"} {
		if strings.HasSuffix(h, tld) {
			return true
		}
	}
	return false
}
