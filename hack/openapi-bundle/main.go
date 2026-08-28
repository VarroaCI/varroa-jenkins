package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"

	"github.com/getkin/kin-openapi/openapi3"
	"sigs.k8s.io/yaml"
)

func main() {
	ctx := context.Background()
	loader := &openapi3.Loader{IsExternalRefsAllowed: true}
	doc, err := loader.LoadFromFile("api/openapi/varroa.root.yaml")
	if err != nil {
		log.Fatalf("load: %v", err)
	}
	doc.InternalizeRefs(ctx, nil)
	if err := doc.Validate(ctx); err != nil {
		log.Fatalf("invalid spec: %v", err)
	}
	js, err := doc.MarshalJSON() // canonical, sorted
	if err != nil {
		log.Fatalf("marshal: %v", err)
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, js, "", "  "); err != nil {
		log.Fatalf("indent: %v", err)
	}
	if err := os.WriteFile("api/openapi/varroa.json", buf.Bytes(), 0644); err != nil {
		log.Fatal(err)
	}
	y, err := yaml.JSONToYAML(buf.Bytes()) // sigs.k8s.io/yaml — NEVER a Go-map YAML marshal
	if err != nil {
		log.Fatalf("yaml: %v", err)
	}
	if err := os.WriteFile("api/openapi/varroa.yaml", y, 0644); err != nil {
		log.Fatal(err)
	}
}
