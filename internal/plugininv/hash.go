package plugininv

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"regexp"
	"sort"
)

// hashRecognisedRE matches the recognised hash format: "v1:" followed by
// exactly 64 lowercase hexadecimal characters.
var hashRecognisedRE = regexp.MustCompile(`^v1:[0-9a-f]{64}$`)

// Hash computes the canonical installed_plugins_hash for the inventory.
// Returns the "v1:"-prefixed lowercase hex SHA-256 string.
//
// Hash normalizes a COPY rather than the receiver. The mite publishes an
// inventory through an atomic.Pointer and hashes it from the heartbeat
// goroutine while the collector may be hashing or serializing the same
// pointee: the atomic makes the pointer load safe, not the pointee. A
// mutating Normalize here sorts Deps in place on the shared backing array and
// appends to Warnings, which is a data race, and can also make the heartbeat
// advertise a hash that does not describe the records actually pushed.
func (inv *Inventory) Hash() string {
	normalized := inv.normalizedCopy()
	return normalized.hashNormalized()
}

// normalizedCopy returns a deep-enough copy of inv with Normalize applied.
// Records and each record's Deps are cloned because Normalize sorts both in
// place; the copy's Warnings start empty so duplicate-drop warnings raised
// during hashing are not appended to the shared inventory.
func (inv *Inventory) normalizedCopy() *Inventory {
	cp := &Inventory{
		Records:          make([]Record, len(inv.Records)),
		CollectedAt:      inv.CollectedAt,
		Source:           inv.Source,
		CollectionFailed: inv.CollectionFailed,
		CollectionError:  inv.CollectionError,
	}
	copy(cp.Records, inv.Records)
	for i := range cp.Records {
		if cp.Records[i].Deps != nil {
			deps := make([]Dep, len(cp.Records[i].Deps))
			copy(deps, cp.Records[i].Deps)
			cp.Records[i].Deps = deps
		}
	}
	cp.Normalize()
	return cp
}

// hashNormalized renders the canonical form of an already-normalized
// inventory and returns its digest.
func (inv *Inventory) hashNormalized() string {
	var buf bytes.Buffer
	// Fixed key order, no whitespace.
	buf.WriteString(`{"v":1,"plugins":[`)
	for i, r := range inv.Records {
		if i > 0 {
			buf.WriteByte(',')
		}
		writeRecord(&buf, r)
	}
	buf.WriteString(`]}`)

	sum := sha256.Sum256(buf.Bytes())
	return fmt.Sprintf("v1:%x", sum)
}

// writeRecord writes a single plugin record in canonical form.
func writeRecord(buf *bytes.Buffer, r Record) {
	buf.WriteString(`{"name":"`)
	escapeString(buf, r.Name)
	buf.WriteString(`","version":"`)
	escapeString(buf, r.Version)
	buf.WriteString(`","enabled":`)
	writeTri(buf, r.Enabled)
	buf.WriteString(`,"detached":`)
	writeTri(buf, r.Detached)
	buf.WriteString(`,"bundled":`)
	writeTri(buf, r.Bundled)
	buf.WriteString(`,"deps":[`)
	writeDeps(buf, r.Deps)
	buf.WriteString(`]}`)
}

// writeTri writes a Tri value as null, false, or true.
func writeTri(buf *bytes.Buffer, t Tri) {
	switch t {
	case TriTrue:
		buf.WriteString("true")
	case TriFalse:
		buf.WriteString("false")
	default:
		buf.WriteString("null")
	}
}

// writeDeps writes the sorted, deduplicated dependency list.
func writeDeps(buf *bytes.Buffer, deps []Dep) {
	// Sort deps by (name, min, optional) with false before true.
	sorted := make([]Dep, len(deps))
	copy(sorted, deps)
	sort.Slice(sorted, func(i, j int) bool {
		return depLess(sorted[i], sorted[j])
	})

	for i, d := range sorted {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(`{"name":"`)
		escapeString(buf, d.Name)
		buf.WriteString(`","min":"`)
		escapeString(buf, d.Min)
		buf.WriteString(`","optional":`)
		if d.Optional {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		buf.WriteByte('}')
	}
}

// escapeString writes s to buf with the canonical escaping rules:
//
//	"   → \"
//	\   → \\
//	\x08 → \b
//	\x09 → \t
//	\x0a → \n
//	\x0c → \f
//	\x0d → \r
//	other codepoints below U+0020 → \u00xx with lowercase hex
//	every other codepoint → raw UTF-8
//
// <, >, &, / and non-ASCII characters are NOT escaped.
func escapeString(buf *bytes.Buffer, s string) {
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch b {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\t':
			buf.WriteString(`\t`)
		case '\n':
			buf.WriteString(`\n`)
		case '\f':
			buf.WriteString(`\f`)
		case '\r':
			buf.WriteString(`\r`)
		default:
			if b < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, b)
			} else {
				buf.WriteByte(b)
			}
		}
	}
}

// hashRecognised reports whether s matches the recognised format: "v1:"
// followed by exactly 64 lowercase hexadecimal characters.
func hashRecognised(s string) bool {
	return hashRecognisedRE.MatchString(s)
}

// HashRecognised is the exported form. It reports whether s matches the
// recognised format: "v1:" plus exactly 64 lowercase hex characters.
func HashRecognised(s string) bool {
	return hashRecognised(s)
}
