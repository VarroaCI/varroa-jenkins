package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// nodePath is a dotted key-path into a YAML document (e.g. "jenkins.systemMessage").
type nodePath []string

func (p nodePath) String() string {
	return strings.Join(p, ".")
}

// setPath declares a managed path whose list value should be treated as a set
// (order-insensitive) in the fingerprint. innerSetKeys names keys within each
// list element whose values are themselves sets.
type setPath struct {
	match        []string
	innerSetKeys []string
}

var declaredSetPaths = []setPath{
	{match: []string{"jenkins", "authorizationStrategy", "roleBased", "roles", "*"},
		innerSetKeys: []string{"permissions", "entries"}},
}

func matchSetPath(p nodePath) (setPath, bool) {
	for _, sp := range declaredSetPaths {
		if len(p) != len(sp.match) {
			continue
		}
		match := true
		for i, seg := range sp.match {
			if seg != "*" && p[i] != seg {
				match = false
				break
			}
		}
		if match {
			return sp, true
		}
	}
	return setPath{}, false
}

// managedPaths parses the applied bundle YAML and returns the sorted set of key-paths
// it declares, each terminating at a scalar leaf or a list node.
func managedPaths(appliedJCasC string) ([]nodePath, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(appliedJCasC), &doc); err != nil {
		return nil, fmt.Errorf("parse applied JCasC: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, nil
	}
	root := doc.Content[0]
	var paths []nodePath
	walkForPaths(root, nil, &paths)
	sort.Slice(paths, func(i, j int) bool {
		return paths[i].String() < paths[j].String()
	})
	return paths, nil
}

func walkForPaths(n *yaml.Node, prefix nodePath, paths *[]nodePath) {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			walkForPaths(c, prefix, paths)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			val := n.Content[i+1]
			sub := append(prefix, key)
			walkForPaths(val, sub, paths)
		}
	case yaml.SequenceNode:
		// List nodes are captured whole, not per-index.
		*paths = append(*paths, prefix)
	case yaml.ScalarNode:
		*paths = append(*paths, prefix)
		// Aliases not traversed; the resolved value is a scalar.
	}
}

// projectAndHash extracts the managed paths from the full exportYAML, assembles
// a canonical subset, and returns its sha256 hex. An absent managed path is
// encoded as an explicit absent-sentinel so removal out-of-band changes the hash.
func projectAndHash(exportYAML, appliedJCasC string) (string, error) {
	paths, err := managedPaths(appliedJCasC)
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "", nil
	}

	var exportRoot yaml.Node
	if err := yaml.Unmarshal([]byte(exportYAML), &exportRoot); err != nil {
		return "", fmt.Errorf("parse export YAML: %w", err)
	}
	if exportRoot.Kind != yaml.DocumentNode || len(exportRoot.Content) == 0 {
		return "", fmt.Errorf("empty export document")
	}
	exportDoc := exportRoot.Content[0]

	// Build a canonical projection: a sorted-map tree containing only managed values.
	proj := buildProjection(exportDoc, paths)
	if len(proj) == 0 {
		return "", nil
	}

	canonical, err := canonicalJSON(proj)
	if err != nil {
		return "", fmt.Errorf("canonicalize projection: %w", err)
	}

	h := sha256.Sum256(canonical)
	return hex.EncodeToString(h[:]), nil
}

// canonicalProjectionValue is either a flat value or a nested map (sorted).
type canonicalProjectionValue map[string]interface{}

// buildProjection extracts managed-path values from exportDoc into a
// sorted-map tree. Absent paths encode "::absent::" as a sentinel.
// Set-canonicalization is applied for paths matching declaredSetPaths.
func buildProjection(exportDoc *yaml.Node, paths []nodePath) canonicalProjectionValue {
	root := canonicalProjectionValue{}
	for _, p := range paths {
		v := getAtPath(exportDoc, p)
		if v == nil {
			setAtPath(root, p, "::absent::")
			continue
		}
		val := nodeToInterface(v)
		if sp, ok := matchSetPath(p); ok {
			if list, isList := val.([]interface{}); isList {
				val = canonicalizeSet(list, sp.innerSetKeys)
			}
		}
		setAtPath(root, p, val)
	}
	return root
}

// canonicalizeSet sorts a list treated as a set: inner set-key slices are
// sorted first, then the outer list elements are sorted. Uses canonicalJSON
// bytes as the stable sort key.
func canonicalizeSet(list []interface{}, innerSetKeys []string) []interface{} {
	for i, elem := range list {
		m, ok := elem.(canonicalProjectionValue)
		if !ok {
			continue
		}
		for _, key := range innerSetKeys {
			inner, ok := m[key]
			if !ok {
				continue
			}
			innerList, ok := inner.([]interface{})
			if !ok {
				continue
			}
			sort.SliceStable(innerList, func(a, b int) bool {
				ja, _ := canonicalJSON(innerList[a])
				jb, _ := canonicalJSON(innerList[b])
				return bytesCompare(ja, jb) < 0
			})
			list[i] = m
		}
	}
	sort.SliceStable(list, func(a, b int) bool {
		ja, _ := canonicalJSON(list[a])
		jb, _ := canonicalJSON(list[b])
		return bytesCompare(ja, jb) < 0
	})
	return list
}

func bytesCompare(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return 0
}

// getAtPath walks a YAML node tree following key names in the path.
// Returns nil if any segment is missing.
func getAtPath(n *yaml.Node, path nodePath) *yaml.Node {
	if len(path) == 0 {
		return n
	}
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		return getAtPath(n.Content[0], path)
	}
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == path[0] {
			return getAtPath(n.Content[i+1], path[1:])
		}
	}
	return nil
}

// setAtPath writes val at path within the canonical tree, creating intermediate
// maps as needed.
func setAtPath(root canonicalProjectionValue, path nodePath, val interface{}) {
	if len(path) == 0 {
		return
	}
	if len(path) == 1 {
		root[path[0]] = val
		return
	}
	existing, ok := root[path[0]]
	if !ok {
		sub := canonicalProjectionValue{}
		root[path[0]] = sub
		setAtPath(sub, path[1:], val)
		return
	}
	sub, ok := existing.(canonicalProjectionValue)
	if !ok {
		sub = canonicalProjectionValue{}
		root[path[0]] = sub
	}
	setAtPath(sub, path[1:], val)
}

// nodeToInterface converts a yaml.Node to a Go value.
func nodeToInterface(n *yaml.Node) interface{} {
	switch n.Kind {
	case yaml.ScalarNode:
		// Try numeric, fall back to string.
		if n.Tag == "!!int" {
			var v int64
			if err := n.Decode(&v); err == nil {
				return v
			}
		}
		if n.Tag == "!!float" {
			var v float64
			if err := n.Decode(&v); err == nil {
				return v
			}
		}
		if n.Tag == "!!bool" {
			var v bool
			if err := n.Decode(&v); err == nil {
				return v
			}
		}
		return n.Value
	case yaml.SequenceNode:
		var list []interface{}
		for _, c := range n.Content {
			list = append(list, nodeToInterface(c))
		}
		return list
	case yaml.MappingNode:
		m := canonicalProjectionValue{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			m[key] = nodeToInterface(n.Content[i+1])
		}
		return m
	default:
		return n.Value
	}
}

// canonicalJSON marshals a Go value to JSON with sorted-map keys, avoiding
// map[interface{}]which fails json.Marshal. The canonicalProjectionValue
// type uses string keys which marshal deterministically.
func canonicalJSON(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}
