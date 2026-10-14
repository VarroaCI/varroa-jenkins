package items

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// UnknownKey is one mapping key that no field of the manifest types declares.
type UnknownKey struct {
	ItemPath string // slash-joined item names from the manifest root; "" outside any item
	Field    string // key path from the manifest root, e.g. items[0].builders[0].jobDsl
	Key      string
}

// UnknownKeysError lists every unknown key of a manifest, in document order.
type UnknownKeysError struct{ Keys []UnknownKey }

func (e *UnknownKeysError) Error() string {
	parts := make([]string, 0, len(e.Keys))
	for _, k := range e.Keys {
		if k.ItemPath != "" {
			parts = append(parts, fmt.Sprintf("unknown key %q at item %q (%s)", k.Key, k.ItemPath, k.Field))
		} else {
			parts = append(parts, fmt.Sprintf("unknown key %q at the manifest top level (%s)", k.Key, k.Field))
		}
	}
	return strings.Join(parts, "; ")
}

// ParseStrict parses like Parse and also refuses a key that no manifest
// field declares. Only a cell tenant manifest uses it: a non-cell controller
// and the mite keep Parse, which ignores an unknown key.
func ParseStrict(yamlContent string) (*Manifest, error) {
	if strings.TrimSpace(yamlContent) == "" {
		return &Manifest{}, nil
	}

	dec := yaml.NewDecoder(strings.NewReader(yamlContent))
	dec.KnownFields(true)
	var m Manifest
	err := dec.Decode(&m)
	if errors.Is(err, io.EOF) {
		// A comment-only document holds no node to decode.
		return &Manifest{}, nil
	}
	if err != nil {
		var te *yaml.TypeError
		if errors.As(err, &te) {
			var root yaml.Node
			if yaml.Unmarshal([]byte(yamlContent), &root) == nil {
				if keys := findUnknownKeys(&root); len(keys) > 0 {
					return nil, &UnknownKeysError{Keys: keys}
				}
			}
		}
		return nil, fmt.Errorf("parse items.yaml: %w", err)
	}

	if err := validateItems(m.Items, ""); err != nil {
		return nil, err
	}
	return &m, nil
}

// findUnknownKeys walks a parsed document against the manifest types and
// returns every mapping key that no field declares.
func findUnknownKeys(root *yaml.Node) []UnknownKey {
	var out []UnknownKey
	walkUnknown(root, reflect.TypeOf(Manifest{}), "", "", map[*yaml.Node]bool{}, &out)
	return out
}

// walkUnknown appends each unknown key under n. t is the Go type that n
// decodes into, field is the key path to n, and itemPath is the slash-joined
// name of the enclosing item.
func walkUnknown(n *yaml.Node, t reflect.Type, field, itemPath string, seen map[*yaml.Node]bool, out *[]UnknownKey) {
	// seen guards the current path only, so an alias used by two items
	// reports its unknown keys under each item.
	if n == nil || seen[n] {
		return
	}
	seen[n] = true
	defer delete(seen, n)

	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) > 0 {
			walkUnknown(n.Content[0], t, field, itemPath, seen, out)
		}
		return
	case yaml.AliasNode:
		walkUnknown(n.Alias, t, field, itemPath, seen, out)
		return
	}

	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	switch t.Kind() {
	case reflect.Struct:
		if n.Kind != yaml.MappingNode {
			return
		}
		fields := yamlFields(t)
		pairs := mappingPairs(n, 0)
		if t == reflect.TypeOf(Item{}) {
			itemPath = extendItemPath(itemPath, pairs)
		}
		for _, kv := range pairs {
			k, v := kv[0], kv[1]
			p := k.Value
			if field != "" {
				p = field + "." + k.Value
			}
			ft, ok := fields[k.Value]
			if !ok {
				*out = append(*out, UnknownKey{ItemPath: itemPath, Field: p, Key: k.Value})
				continue
			}
			walkUnknown(v, ft, p, itemPath, seen, out)
		}
	case reflect.Slice, reflect.Array:
		if n.Kind != yaml.SequenceNode {
			return
		}
		for i, c := range n.Content {
			walkUnknown(c, t.Elem(), field+"["+strconv.Itoa(i)+"]", itemPath, seen, out)
		}
	case reflect.Map:
		et := t.Elem()
		for et.Kind() == reflect.Pointer {
			et = et.Elem()
		}
		if n.Kind != yaml.MappingNode {
			return
		}
		// A free-form map such as map[string]any stops here.
		if et.Kind() != reflect.Struct && et.Kind() != reflect.Slice && et.Kind() != reflect.Map {
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			walkUnknown(v, et, field+"."+k.Value, itemPath, seen, out)
		}
	}
}

// mappingPairs returns the key/value pairs of a mapping node in document
// order, with each YAML merge key (<<) expanded into the pairs of the
// mappings it names. An explicit key overrides a merged one, as YAML
// specifies. depth bounds the expansion.
func mappingPairs(n *yaml.Node, depth int) [][2]*yaml.Node {
	if n.Kind != yaml.MappingNode || depth > 32 {
		return nil
	}
	var explicit, merged [][2]*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if !isMergeKey(k) {
			explicit = append(explicit, [2]*yaml.Node{k, v})
			continue
		}
		for _, src := range mergeSources(v) {
			merged = append(merged, mappingPairs(src, depth+1)...)
		}
	}
	taken := make(map[string]bool, len(explicit))
	for _, kv := range explicit {
		taken[kv[0].Value] = true
	}
	out := explicit
	for _, kv := range merged {
		if !taken[kv[0].Value] {
			taken[kv[0].Value] = true
			out = append(out, kv)
		}
	}
	return out
}

// isMergeKey reports whether k is a YAML merge key. It applies the same test
// as yaml.v3, so a quoted "<<" is an ordinary key.
func isMergeKey(k *yaml.Node) bool {
	return k.Kind == yaml.ScalarNode &&
		k.Value == "<<" &&
		(k.Tag == "" || k.Tag == "!" || k.ShortTag() == "!!merge")
}

// mergeSources resolves the value of a merge key to the mappings it names:
// one alias or mapping, or a sequence of them.
func mergeSources(v *yaml.Node) []*yaml.Node {
	resolve := func(n *yaml.Node) *yaml.Node {
		if n.Kind == yaml.AliasNode {
			return n.Alias
		}
		return n
	}
	v = resolve(v)
	if v.Kind == yaml.SequenceNode {
		out := make([]*yaml.Node, 0, len(v.Content))
		for _, c := range v.Content {
			out = append(out, resolve(c))
		}
		return out
	}
	return []*yaml.Node{v}
}

// extendItemPath returns the item path for an Item mapping. It appends the
// item's name to the enclosing path, or uses the name alone at the top level.
func extendItemPath(parent string, pairs [][2]*yaml.Node) string {
	for _, kv := range pairs {
		if kv[0].Value == "name" && kv[1].Kind == yaml.ScalarNode {
			if parent == "" {
				return kv[1].Value
			}
			return parent + "/" + kv[1].Value
		}
	}
	return parent
}

// yamlFields maps each yaml key of a struct type to its field type. An inline
// field contributes its own keys.
func yamlFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("yaml")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}
		if strings.Contains(opts, "inline") {
			it := f.Type
			for it.Kind() == reflect.Pointer {
				it = it.Elem()
			}
			if it.Kind() == reflect.Struct {
				for k, v := range yamlFields(it) {
					out[k] = v
				}
			}
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		out[name] = f.Type
	}
	return out
}

// IsManifestKey reports whether Manifest declares key at its top level.
func IsManifestKey(key string) bool {
	_, ok := yamlFields(reflect.TypeOf(Manifest{}))[key]
	return ok
}
