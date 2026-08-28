package main

import (
	"bytes"
	"embed"
	"flag"
	"fmt"
	"go/format"
	"os"
	"sort"
	"strings"
	"text/template"

	"github.com/emicklei/proto"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// ---- IR Types ----

type IR struct {
	Package   string
	Messages  []MessageIR
	Enums     []EnumIR
	Service   *ServiceIR
	enumNames map[string]bool
}

type FieldIR struct {
	GoName     string
	JSONName   string
	GoType     string
	IsRequired bool
}

type OneofFieldInfo struct {
	GoName   string
	JSONName string
	TypeName string
	Number   int
}

type MessageIR struct {
	Name        string
	Comment     string
	Fields      []FieldIR
	OneofName   string
	OneofFields []OneofFieldInfo
}

type EnumValueIR struct {
	GoName string
	Value  int
}

type EnumIR struct {
	Name   string
	Values []EnumValueIR
}

type RPCIR struct {
	GoName        string
	RequestType   string
	ResponseType  string
	ClientStreams bool
	ServerStreams bool
}

type ServiceIR struct {
	Name string
	RPCs []RPCIR
}

// ---- Proto Parsing ----

func buildIR(def *proto.Proto) *IR {
	ir := &IR{Package: "mitev1", enumNames: make(map[string]bool)}

	var protoMessages []*proto.Message
	var protoEnums []*proto.Enum
	var protoService *proto.Service

	for _, el := range def.Elements {
		switch v := el.(type) {
		case *proto.Message:
			protoMessages = append(protoMessages, v)
		case *proto.Enum:
			protoEnums = append(protoEnums, v)
			ir.enumNames[v.Name] = true
		case *proto.Service:
			protoService = v
		}
	}

	sort.Slice(protoMessages, func(i, j int) bool {
		return protoMessages[i].Name < protoMessages[j].Name
	})
	for _, pm := range protoMessages {
		ir.Messages = append(ir.Messages, buildMessageIR(pm, ir.enumNames))
	}

	sort.Slice(protoEnums, func(i, j int) bool {
		return protoEnums[i].Name < protoEnums[j].Name
	})
	for _, pe := range protoEnums {
		ir.Enums = append(ir.Enums, buildEnumIR(pe))
	}

	if protoService != nil {
		ir.Service = buildServiceIR(protoService)
	}

	return ir
}

func buildMessageIR(pm *proto.Message, enumNames map[string]bool) MessageIR {
	m := MessageIR{
		Name:    pm.Name,
		Comment: commentText(pm.Comment),
	}

	var oneof *proto.Oneof

	for _, el := range pm.Elements {
		switch v := el.(type) {
		case *proto.NormalField:
			fi := buildFieldIR(v.Field, v.Repeated, enumNames)
			m.Fields = append(m.Fields, fi)
		case *proto.MapField:
			fi := buildMapFieldIR(v)
			m.Fields = append(m.Fields, fi)
		case *proto.Oneof:
			oneof = v
		}
	}

	if oneof != nil {
		m.OneofName = oneof.Name
		for _, of := range oneof.Elements {
			ofield, ok := of.(*proto.OneOfField)
			if !ok {
				continue
			}
			m.OneofFields = append(m.OneofFields, OneofFieldInfo{
				GoName:   camelCase(ofield.Name),
				JSONName: ofield.Name,
				TypeName: ofield.Type,
				Number:   ofield.Sequence,
			})
		}
		sort.Slice(m.OneofFields, func(i, j int) bool {
			return m.OneofFields[i].Number < m.OneofFields[j].Number
		})
	}

	return m
}

func buildFieldIR(f *proto.Field, repeated bool, enumNames map[string]bool) FieldIR {
	isRequired := false
	if f.Comment != nil {
		msg := f.Comment.Message()
		isRequired = strings.Contains(msg, "@required")
	}

	goType := protoToGo(f.Type)
	if repeated && isMessageType(f.Type, enumNames) {
		goType = "[]*" + f.Type
	} else if repeated {
		goType = "[]" + protoToGo(f.Type)
	} else if isMessageType(f.Type, enumNames) {
		goType = "*" + f.Type
	}

	return FieldIR{
		GoName:     goName(f.Name),
		JSONName:   f.Name,
		GoType:     goType,
		IsRequired: isRequired,
	}
}

func buildMapFieldIR(mf *proto.MapField) FieldIR {
	isRequired := false
	if mf.Comment != nil {
		msg := mf.Comment.Message()
		isRequired = strings.Contains(msg, "@required")
	}
	return FieldIR{
		GoName:     camelCase(mf.Name),
		JSONName:   mf.Name,
		GoType:     "map[" + protoToGo(mf.KeyType) + "]" + protoToGo(mf.Type),
		IsRequired: isRequired,
	}
}

func buildEnumIR(pe *proto.Enum) EnumIR {
	e := EnumIR{Name: pe.Name}
	prefix := camelToUpperSnake(pe.Name) + "_"

	for _, el := range pe.Elements {
		ef, ok := el.(*proto.EnumField)
		if !ok {
			continue
		}
		name := strings.TrimPrefix(ef.Name, prefix)
		e.Values = append(e.Values, EnumValueIR{
			GoName: pe.Name + camelCaseUnderscore(name),
			Value:  ef.Integer,
		})
	}
	return e
}

// camelToUpperSnake converts CamelCase to UPPER_SNAKE_CASE.
func camelToUpperSnake(s string) string {
	var result strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			result.WriteByte('_')
		}
		result.WriteRune(r)
	}
	return strings.ToUpper(result.String())
}

func buildServiceIR(ps *proto.Service) *ServiceIR {
	s := &ServiceIR{Name: ps.Name}
	for _, el := range ps.Elements {
		rpc, ok := el.(*proto.RPC)
		if !ok {
			continue
		}
		s.RPCs = append(s.RPCs, RPCIR{
			GoName:        camelCase(rpc.Name),
			RequestType:   rpc.RequestType,
			ResponseType:  rpc.ReturnsType,
			ClientStreams: rpc.StreamsRequest,
			ServerStreams: rpc.StreamsReturns,
		})
	}
	return s
}

// ---- Type Mapping ----

func protoToGo(protoType string) string {
	switch protoType {
	case "string":
		return "string"
	case "int32":
		return "int"
	case "int64":
		return "int64"
	case "bool":
		return "bool"
	case "bytes":
		return "[]byte"
	default:
		// Message type — already a Go type name.
		return protoType
	}
}

func isMessageType(t string, enumNames map[string]bool) bool {
	if enumNames[t] {
		return false
	}
	switch t {
	case "string", "int32", "int64", "bool", "bytes":
		return false
	default:
		return true
	}
}

// ---- String Helpers ----

func camelCase(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

// goName computes the Go struct field name from a proto field name.
// Handles the specific acronym exceptions present in the existing API.
func goName(protoName string) string {
	n := camelCase(protoName)
	switch n {
	case "BannerUrl":
		return "BannerURL"
	case "TtlSeconds":
		return "TTLSeconds"
	case "Url":
		return "URL"
	default:
		return n
	}
}

func camelCaseUnderscore(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + strings.ToLower(p[1:])
		}
	}
	return strings.Join(parts, "")
}

func commentText(c *proto.Comment) string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.Message())
}

// ---- Template Functions ----

var funcMap = template.FuncMap{
	"lowerFirst": func(s string) string {
		if s == "" {
			return s
		}
		return strings.ToLower(s[:1]) + s[1:]
	},
	"camelCase": camelCase,
}

// ---- Main ----

func main() {
	outputFile := flag.String("o", "", "output file (default: stdout)")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "usage: protogen [-o output.go] <proto-file>\n")
		os.Exit(1)
	}
	protoFile := flag.Arg(0)

	reader, err := os.Open(protoFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error opening %s: %v\n", protoFile, err)
		os.Exit(1)
	}
	defer func() { _ = reader.Close() }()

	parser := proto.NewParser(reader)
	def, err := parser.Parse()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error parsing %s: %v\n", protoFile, err)
		os.Exit(1)
	}

	ir := buildIR(def)

	tmpl := template.New("master.tmpl").Funcs(funcMap)
	if _, err := tmpl.ParseFS(templateFS, "templates/*.tmpl"); err != nil {
		fmt.Fprintf(os.Stderr, "error parsing templates: %v\n", err)
		os.Exit(1)
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "master.tmpl", ir); err != nil {
		fmt.Fprintf(os.Stderr, "error rendering template: %v\n", err)
		os.Exit(1)
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		fmt.Fprintf(os.Stderr, "error formatting output: %v\n", err)
		os.Stdout.Write(buf.Bytes())
		os.Exit(1)
	}

	w := os.Stdout
	if *outputFile != "" {
		f, err := os.Create(*outputFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error creating %s: %v\n", *outputFile, err)
			os.Exit(1)
		}
		defer func() { _ = f.Close() }()
		w = f
	}
	if _, err := w.Write(formatted); err != nil {
		fmt.Fprintf(os.Stderr, "error writing output: %v\n", err)
		os.Exit(1)
	}
}
