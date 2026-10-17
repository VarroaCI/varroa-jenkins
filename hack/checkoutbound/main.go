// Command checkoutbound keeps the outbound-connection inventory complete.
//
// The inventory file names every destination that a Varroa component dials.
// The command extracts host literals from the sources, fails on any host the
// inventory does not name, fails on an inventory row that no source uses, and
// checks that the committed docs/security/outbound-connections.md matches the
// inventory. The page is generated, so the inventory is the only place to edit.
//
// Usage:
//
//	go run ./hack/checkoutbound [-list] [-write] [dir]
//
// -list prints every extracted host with its classification. -write
// regenerates the page; it refuses to write while a check fails.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	inventoryRel = "hack/checkoutbound/inventory.yaml"
	pageRel      = "docs/security/outbound-connections.md"
)

func main() {
	list := flag.Bool("list", false, "print every extracted host with its classification")
	write := flag.Bool("write", false, "regenerate "+pageRel+" from the inventory")
	flag.Parse()

	root := "."
	if flag.NArg() > 0 {
		root = flag.Arg(0)
	}
	os.Exit(run(root, *list, *write, os.Stdout, os.Stderr))
}

// run executes one mode and returns the process exit code.
func run(root string, list, write bool, stdout, stderr *os.File) int {
	res, err := scan(root)
	if err != nil {
		fmt.Fprintf(stderr, "checkoutbound: %v\n", err)
		return 2
	}

	if list {
		printList(res, stdout)
		return 0
	}

	problems := res.problems()
	if len(problems) > 0 {
		fmt.Fprintf(stderr, "checkoutbound: %d problem(s):\n\n", len(problems))
		for _, p := range problems {
			fmt.Fprintf(stderr, "  %s\n", p)
		}
		_, _ = fmt.Fprint(stderr, "\nFix the inventory in "+inventoryRel+", then run make outbound-doc.\n")
		return 1
	}

	doc, err := renderDoc(res.inv)
	if err != nil {
		fmt.Fprintf(stderr, "checkoutbound: render: %v\n", err)
		return 2
	}
	pagePath := filepath.Join(root, pageRel)

	if write {
		if err := os.WriteFile(pagePath, doc, 0o644); err != nil {
			fmt.Fprintf(stderr, "checkoutbound: %v\n", err)
			return 2
		}
		fmt.Fprintf(stdout, "checkoutbound: wrote %s\n", pageRel)
		return 0
	}

	committed, err := os.ReadFile(pagePath)
	if err != nil {
		fmt.Fprintf(stderr, "checkoutbound: read %s: %v\n", pageRel, err)
		return 2
	}
	if !bytes.Equal(committed, doc) {
		fmt.Fprintf(stderr, "checkoutbound: %s differs from the inventory. Run make outbound-doc and commit the result.\n", pageRel)
		return 1
	}
	fmt.Fprintf(stdout, "checkoutbound: %d host(s) inventoried, page is current\n", len(res.inv.hostSet()))
	return 0
}

// printList prints each distinct host with its file and line, sorted by host.
func printList(res *scanResult, w *os.File) {
	hits := append([]hit(nil), res.hits...)
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Host != hits[j].Host {
			return hits[i].Host < hits[j].Host
		}
		if hits[i].File != hits[j].File {
			return hits[i].File < hits[j].File
		}
		return hits[i].Line < hits[j].Line
	})
	for _, h := range hits {
		status := "UNKNOWN"
		switch {
		case res.inv.hostSet()[h.Host]:
			status = "inventory"
		case res.inv.identifierSet()[h.Host]:
			status = "identifier"
		}
		fmt.Fprintf(w, "%-40s %-10s %s:%d\n", h.Host, status, h.File, h.Line)
	}
}

// scanResult holds the inventory and every host literal the scan found.
type scanResult struct {
	inv  *Inventory
	hits []hit
}

// scan loads the inventory under root and extracts every host literal.
func scan(root string) (*scanResult, error) {
	inv, err := loadInventory(filepath.Join(root, inventoryRel))
	if err != nil {
		return nil, err
	}
	hits, err := collectHits(root, inv)
	if err != nil {
		return nil, err
	}
	return &scanResult{inv: inv, hits: hits}, nil
}

// problems lists every unknown host and every stale inventory entry.
func (r *scanResult) problems() []string {
	var out []string
	known := map[string]bool{}
	for h := range r.inv.hostSet() {
		known[h] = true
	}
	for h := range r.inv.identifierSet() {
		known[h] = true
	}

	seen := map[string]bool{}
	for _, h := range r.hits {
		seen[h.Host] = true
		if !known[h.Host] {
			out = append(out, fmt.Sprintf("%s:%d: host %q is not in %s; add a connections row or an identifiers entry",
				h.File, h.Line, h.Host, inventoryRel))
		}
	}
	for _, c := range r.inv.Connections {
		for _, host := range c.Hosts {
			if !seen[host] {
				out = append(out, fmt.Sprintf("%s: connections row %q lists host %q, which no source uses; remove the host or the row",
					inventoryRel, c.Destination, host))
			}
		}
	}
	for _, id := range r.inv.Identifiers {
		if !seen[id.Host] {
			out = append(out, fmt.Sprintf("%s: identifiers entry %q is not used by any source; remove it",
				inventoryRel, id.Host))
		}
	}
	sort.Strings(out)
	return out
}

// hostSet returns every host that a connections row covers.
func (inv *Inventory) hostSet() map[string]bool {
	out := map[string]bool{}
	for _, c := range inv.Connections {
		for _, h := range c.Hosts {
			out[h] = true
		}
	}
	return out
}

// identifierSet returns every literal that the inventory lists as an identifier.
func (inv *Inventory) identifierSet() map[string]bool {
	out := map[string]bool{}
	for _, id := range inv.Identifiers {
		out[id.Host] = true
	}
	return out
}

// normalizeSlash returns a relative path that uses forward slashes.
func normalizeSlash(p string) string {
	return strings.ReplaceAll(filepath.ToSlash(p), "//", "/")
}
