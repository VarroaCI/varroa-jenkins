package main

import (
	"bytes"
	_ "embed"
	"strings"
	"text/template"
)

//go:embed doc.md.tmpl
var docTemplate string

// pageSection is one table of the generated page.
type pageSection struct {
	Title string
	Rows  []Connection
}

// renderDoc builds the page from the inventory. The output depends only on the
// inventory and the template, so two runs give the same bytes.
func renderDoc(inv *Inventory) ([]byte, error) {
	var sections []pageSection
	for _, c := range componentOrder {
		var rows []Connection
		for _, row := range inv.Connections {
			if row.Component == c.Key {
				rows = append(rows, row)
			}
		}
		if len(rows) == 0 {
			continue
		}
		sections = append(sections, pageSection{Title: c.Title, Rows: rows})
	}

	tmpl, err := template.New("doc").Funcs(template.FuncMap{"cell": tableCell}).Parse(docTemplate)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct{ Sections []pageSection }{sections}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// tableCell makes a value safe inside a Markdown table cell.
func tableCell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(s, "|", `\|`)
}
