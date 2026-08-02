package openapi

import "embed"

// SpecJSON is the bundled OpenAPI document served at /api/v1/openapi.json.
//
//go:embed varroa.json
var SpecJSON []byte

// DocsFS holds the self-contained interactive docs page served at /api/v1/docs.
//
//go:embed docs/index.html docs/rapidoc-min.js
var DocsFS embed.FS
