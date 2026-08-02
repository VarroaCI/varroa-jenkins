package main

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"text/tabwriter"

	"sigs.k8s.io/yaml"
)

// ---------------------------------------------------------------------------
// Output primitives
// ---------------------------------------------------------------------------

// printTable writes a table via text/tabwriter. headers and rows must have the
// same length. If noHeaders is true, the header row is omitted.
func printTable(w io.Writer, headers []string, rows [][]string, noHeaders bool) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	if !noHeaders && len(headers) > 0 {
		for i, h := range headers {
			if i > 0 {
				_, _ = fmt.Fprint(tw, "\t")
			}
			_, _ = fmt.Fprint(tw, h)
		}
		_, _ = fmt.Fprintln(tw)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i > 0 {
				_, _ = fmt.Fprint(tw, "\t")
			}
			_, _ = fmt.Fprint(tw, cell)
		}
		_, _ = fmt.Fprintln(tw)
	}
	_ = tw.Flush()
}

// printJSON marshals v as JSON with indentation and writes to w.
func printJSON(w io.Writer, v interface{}) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// printYAML marshals v as YAML and writes to w.
func printYAML(w io.Writer, v interface{}) error {
	data, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// printName writes "<ns>/<name>" lines for each item that has Namespace and
// Name fields accessible via a getter function.
func printName(w io.Writer, items interface {
	GetNamespace() string
	GetName() string
}) {
	_, _ = fmt.Fprintf(w, "%s/%s\n", items.GetNamespace(), items.GetName())
}

// printNameMulti writes "<ns>/<name>" lines for multiple items.
// items accepts a slice of any type with GetNamespace() and GetName() methods.
func printNameMulti(w io.Writer, items interface{}) error {
	v := reflect.ValueOf(items)
	if v.Kind() != reflect.Slice {
		return fmt.Errorf("printNameMulti: expected slice, got %T", items)
	}
	for i := 0; i < v.Len(); i++ {
		item := v.Index(i)
		ns := item.MethodByName("GetNamespace").Call(nil)[0].String()
		name := item.MethodByName("GetName").Call(nil)[0].String()
		_, _ = fmt.Fprintf(w, "%s/%s\n", ns, name)
	}
	return nil
}
