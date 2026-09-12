package items

import "fmt"

// ReRoot returns a manifest whose only top-level item is a folder named
// tenant, carrying every item of in as its children. A non-empty in.Root is
// rejected: a tenant manifest may not choose its own path, because the cell
// assigns the path when it places the folder under the merged manifest.
func ReRoot(in *Manifest, tenant string) (*Manifest, error) {
	if in == nil {
		in = &Manifest{}
	}
	if in.Root != "" {
		return nil, fmt.Errorf("tenant %q: manifest declares root %q, which a tenant manifest must not set", tenant, in.Root)
	}
	return &Manifest{
		Items: []Item{
			{
				Kind:  "folder",
				Name:  tenant,
				Items: in.Items,
			},
		},
	}, nil
}

// Merge returns one manifest holding every input's top-level items. It
// rejects a duplicate top-level name, which would otherwise let one tenant
// overwrite another.
//
// The result's remove strategy is fixed to RemoveSync. The default
// RemoveNone would leave a removed tenant's folder on the cell forever, and
// no other source supplies the value: a materialized tenant items.yaml
// never carries a removeStrategy (composeItems reads only the items key),
// and the reconciliation policy carries none either.
func Merge(in []*Manifest) (*Manifest, error) {
	var out []Item
	seen := make(map[string]bool, len(in))
	for _, m := range in {
		if m == nil {
			continue
		}
		for _, it := range m.Items {
			if seen[it.Name] {
				return nil, fmt.Errorf("duplicate top-level item %q", it.Name)
			}
			seen[it.Name] = true
			out = append(out, it)
		}
	}
	return &Manifest{
		RemoveStrategy: &RemoveStrategy{Items: RemoveSync},
		Items:          out,
	}, nil
}
