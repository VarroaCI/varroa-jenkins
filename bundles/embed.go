// Package bundles carries the JCasC content Varroa ships inside its own binary.
//
// The starter bundle is the configuration a Controller gets when it names no
// ComposedBundle. It is embedded rather than fetched so that a first run needs
// no git remote, no OCI registry, and no network at all — an air-gapped install
// provisions the same Jenkins as a connected one.
package bundles

import (
	_ "embed"
)

//go:embed starter/jenkins.yaml
var starterJCasC string

//go:embed starter/items.yaml
var starterItems string

// StarterJCasC returns the starter bundle's JCasC content, verbatim.
func StarterJCasC() string { return starterJCasC }

// StarterItems returns the starter bundle's items content, verbatim.
func StarterItems() string { return starterItems }
