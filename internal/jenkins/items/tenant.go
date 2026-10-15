package items

import (
	"fmt"
	"strings"
)

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
	if err := checkGenericTokens(in.Items, tenant); err != nil {
		return nil, err
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

// checkGenericTokens rejects a genericTrigger token that does not start
// with "<tenant>:". The tenant door fences a call by that prefix, so a
// token outside the prefix would let another tenant's door fire the job.
// It walks every nested item and every pipelineTriggers entry.
// The error never names the token, because the token is the job's webhook secret.
func checkGenericTokens(items []Item, tenant string) error {
	for _, it := range items {
		for _, tr := range collectTriggers(it) {
			if tr.GenericTrigger == nil {
				continue
			}
			tok, prefix := tr.GenericTrigger.Token, tenant+":"
			if !strings.HasPrefix(tok, prefix) || len(tok) == len(prefix) {
				return fmt.Errorf("tenant %q: item %q genericTrigger token must start with %q", tenant, it.Name, prefix)
			}
		}
		if err := checkGenericTokens(it.Items, tenant); err != nil {
			return err
		}
	}
	return nil
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
