package openapi

import (
	"maps"
	"slices"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// TestSchemaConditionsHaveTypeAndStatus walks every schema of the bundled
// document. The items of each array property named conditions must declare a
// type and a status property. A metadata schema in that place fails the test.
func TestSchemaConditionsHaveTypeAndStatus(t *testing.T) {
	doc, err := (&openapi3.Loader{}).LoadFromData(SpecJSON)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	found := 0
	seen := map[*openapi3.Schema]bool{}
	var walk func(path string, ref *openapi3.SchemaRef)
	walk = func(path string, ref *openapi3.SchemaRef) {
		if ref == nil || ref.Value == nil || seen[ref.Value] {
			return
		}
		s := ref.Value
		seen[s] = true
		for _, name := range slices.Sorted(maps.Keys(s.Properties)) {
			prop := s.Properties[name]
			if name == "conditions" && prop != nil && prop.Value != nil && prop.Value.Type.Is("array") {
				found++
				items := prop.Value.Items
				if items == nil || items.Value == nil {
					t.Errorf("%s.conditions: the array has no items schema", path)
				} else {
					for _, field := range []string{"type", "status"} {
						if !declaresProperty(items.Value, field, map[*openapi3.Schema]bool{}) {
							t.Errorf("%s.conditions: items %s have no %s property", path, items.Ref, field)
						}
					}
				}
			}
			walk(path+"."+name, prop)
		}
		walk(path+"[]", s.Items)
		for _, sub := range slices.Concat(s.AllOf, s.OneOf, s.AnyOf) {
			walk(path, sub)
		}
		if s.AdditionalProperties.Schema != nil {
			walk(path+"{}", s.AdditionalProperties.Schema)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(doc.Components.Schemas)) {
		walk(name, doc.Components.Schemas[name])
	}
	paths := doc.Paths.Map()
	for _, p := range slices.Sorted(maps.Keys(paths)) {
		ops := paths[p].Operations()
		for _, method := range slices.Sorted(maps.Keys(ops)) {
			op := ops[method]
			if op.RequestBody != nil && op.RequestBody.Value != nil {
				for ct, mt := range op.RequestBody.Value.Content {
					walk(method+" "+p+" request "+ct, mt.Schema)
				}
			}
			if op.Responses == nil {
				continue
			}
			for code, resp := range op.Responses.Map() {
				if resp == nil || resp.Value == nil {
					continue
				}
				for ct, mt := range resp.Value.Content {
					walk(method+" "+p+" "+code+" "+ct, mt.Schema)
				}
			}
		}
	}
	if found == 0 {
		t.Fatal("found no conditions property: the walk does not reach the schemas")
	}
}

// declaresProperty reports whether the schema declares the property directly
// or through its composition. An allOf member that declares it is enough. A
// oneOf or anyOf schema declares it only when every alternative does, because
// another alternative could omit it. The seen set stops a cycle.
func declaresProperty(s *openapi3.Schema, name string, seen map[*openapi3.Schema]bool) bool {
	if s == nil || seen[s] {
		return false
	}
	seen[s] = true
	if s.Properties[name] != nil {
		return true
	}
	for _, sub := range s.AllOf {
		if sub != nil && declaresProperty(sub.Value, name, seen) {
			return true
		}
	}
	for _, alts := range [][]*openapi3.SchemaRef{s.OneOf, s.AnyOf} {
		if len(alts) == 0 {
			continue
		}
		every := true
		for _, sub := range alts {
			if sub == nil || !declaresProperty(sub.Value, name, maps.Clone(seen)) {
				every = false
				break
			}
		}
		if every {
			return true
		}
	}
	return false
}
