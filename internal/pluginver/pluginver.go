// Package pluginver orders Jenkins *plugin* versions.
//
// It is a structure-for-structure port of hudson.util.VersionNumber
// (jenkinsci/lib-version-number), which is itself Maven's ComparableVersion with
// a Jenkins-specific qualifier alias. That ordering is what makes a
// Plugin-Dependencies entry a *minimum* rather than a pin:
// workflow-api:1384.vdc05a_48f535f is satisfied by 1413.v2ff1a_5e720fa_.
//
// # This is NOT internal/jenkinsver
//
// internal/jenkinsver parses Jenkins *core image tags* ("2.479.3-jdk17"): it
// splits on the first '-', discards the remainder, and compares []int. Applying
// it to a plugin version would truncate "4.5.14-269.vfa_2321039a_83" to
// "4.5.14" — erasing exactly the incrementals suffix that distinguishes plugin
// releases — and its []int model cannot express the nested item tree that makes
// "1-1" sort below "1.1". The two packages are deliberately separate and must
// not be merged.
//
// # Totality
//
// Every string parses. There is no error return and no ok bool: a segment that
// is not an integer becomes a string item and is ordered by the qualifier
// table, with unrecognised qualifiers ranking above every table entry and tying
// among themselves lexicographically. A caller can therefore always order two
// versions, which is what the closure planner needs — an unorderable version
// would have no defined behaviour anywhere in the tree.
package pluginver

import (
	"math/big"
	"strconv"
	"strings"
)

// Compare returns -1, 0, or +1 as a sorts before, equal to, or after b,
// following hudson.util.VersionNumber ordering.
func Compare(a, b string) int {
	return sign(parse(a).compare(parse(b)))
}

// AtLeast reports whether have satisfies the minimum want, i.e. whether
// Compare(have, want) >= 0. This is the Plugin-Dependencies predicate.
func AtLeast(have, want string) bool {
	return Compare(have, want) >= 0
}

// ---------------------------------------------------------------------------
// Item model
// ---------------------------------------------------------------------------

// itemKind is the cross-kind ordering key: string < list < integer. A trailing
// hyphen group therefore sorts below a plain numeric continuation ("1-1" <
// "1.1") and a qualifier sorts below a release ("1.0-beta" < "1.0").
type itemKind int

const (
	kindString itemKind = iota
	kindList
	kindInt
)

// item is one node of the parsed version tree. compare accepts a nil other,
// which stands for "the other side ran out of items" and is what gives
// "1.0" == "1" and "1.0-beta" < "1".
type item interface {
	kind() itemKind
	isNull() bool
	compare(other item) int
}

// --- integer item ---

type intItem struct{ v *big.Int }

func newIntItem(s string) *intItem {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		// Unreachable: the parser only routes all-digit runs here. Kept because
		// the package contract is total — there is no error path to take.
		n = big.NewInt(0)
	}
	return &intItem{v: n}
}

func intZero() *intItem { return &intItem{v: big.NewInt(0)} }

func (i *intItem) kind() itemKind { return kindInt }
func (i *intItem) isNull() bool   { return i.v.Sign() == 0 }

func (i *intItem) compare(other item) int {
	if other == nil {
		// "1.0" == "1", but "1.1" > "1".
		if i.isNull() {
			return 0
		}
		return 1
	}
	if o, ok := other.(*intItem); ok {
		return i.v.Cmp(o.v)
	}
	// An integer outranks both a qualifier ("1.1" > "1-sp") and a sub-list
	// ("1.1" > "1-1").
	return 1
}

// --- string (qualifier) item ---

type stringItem struct{ v string }

// qualifiers is the ordering table, lowest first. Note "snapshot" sits below
// "alpha", not near "rc"; "" is the release qualifier.
var qualifiers = []string{"snapshot", "alpha", "beta", "milestone", "rc", "", "sp"}

// releaseIndex is comparableQualifier("") — the rank a bare release sorts at.
var releaseIndex = strconv.Itoa(indexOfQualifier(""))

// aliases are applied when the item is constructed, before ranking.
var aliases = map[string]string{
	"ga":    "",
	"final": "",
	"cr":    "rc",
	"ea":    "rc",
}

func indexOfQualifier(q string) int {
	for i, s := range qualifiers {
		if s == q {
			return i
		}
	}
	return -1
}

// newStringItem builds a qualifier item. followedByDigit expands the
// single-letter forms ("1.0a1" means alpha 1); the alias table is applied
// afterwards, so "ga"/"final" collapse onto the release qualifier.
func newStringItem(v string, followedByDigit bool) *stringItem {
	if followedByDigit && len(v) == 1 {
		switch v[0] {
		case 'a':
			v = "alpha"
		case 'b':
			v = "beta"
		case 'm':
			v = "milestone"
		}
	}
	if alias, ok := aliases[v]; ok {
		v = alias
	}
	return &stringItem{v: v}
}

// comparableQualifier maps a qualifier onto a lexicographically comparable
// rank. Unknown qualifiers become len(qualifiers)+"-"+qualifier, which sorts
// above every table entry and ties among themselves lexicographically — that is
// what makes incrementals suffixes such as "v2ff1a_5e720fa_" comparable.
func comparableQualifier(q string) string {
	if alias, ok := aliases[q]; ok {
		q = alias
	}
	if i := indexOfQualifier(q); i >= 0 {
		return strconv.Itoa(i)
	}
	return strconv.Itoa(len(qualifiers)) + "-" + q
}

func (s *stringItem) kind() itemKind { return kindString }
func (s *stringItem) isNull() bool   { return comparableQualifier(s.v) == releaseIndex }

func (s *stringItem) compare(other item) int {
	if other == nil {
		// "1-rc" < "1", "1-sp" > "1", "1-ga" == "1".
		return strings.Compare(comparableQualifier(s.v), releaseIndex)
	}
	if o, ok := other.(*stringItem); ok {
		return strings.Compare(comparableQualifier(s.v), comparableQualifier(o.v))
	}
	// A qualifier sorts below both an integer and a sub-list.
	return -1
}

// --- list item ---

type listItem struct{ items []item }

func (l *listItem) kind() itemKind { return kindList }
func (l *listItem) isNull() bool   { return len(l.items) == 0 }

func (l *listItem) add(i item) { l.items = append(l.items, i) }

// normalize drops trailing null items so "1.0" equals "1" and "1.0.0".
func (l *listItem) normalize() {
	for len(l.items) > 0 && l.items[len(l.items)-1].isNull() {
		l.items = l.items[:len(l.items)-1]
	}
}

func (l *listItem) compare(other item) int {
	if other == nil {
		if len(l.items) == 0 {
			return 0 // "1-0" normalizes to "1-" which equals "1"
		}
		return l.items[0].compare(nil)
	}
	switch o := other.(type) {
	case *listItem:
		n := len(l.items)
		if len(o.items) > n {
			n = len(o.items)
		}
		for i := 0; i < n; i++ {
			var left, right item
			if i < len(l.items) {
				left = l.items[i]
			}
			if i < len(o.items) {
				right = o.items[i]
			}
			var result int
			if left == nil {
				// This side is shorter: invert the comparison.
				result = -right.compare(nil)
			} else {
				result = left.compare(right)
			}
			if result != 0 {
				return result
			}
		}
		return 0
	case *intItem:
		return -1 // "1-1" < "1.0.x"
	default:
		return 1 // "1-1" > "1-sp"
	}
}

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

// parse builds the nested item tree.
//
//   - '.' separates items within the current list;
//   - '-' also separates, and additionally opens a nested sub-list when it sits
//     between two digits (this is what distinguishes "1-1" from "1.1");
//   - a digit↔letter transition splits without consuming a separator, which is
//     what separates a numeric prefix from an incrementals suffix;
//   - '_' is not a separator — Jenkins' incrementals escape unsafe characters
//     with it, so it is an ordinary qualifier character.
func parse(version string) *listItem {
	version = strings.ToLower(version)

	root := &listItem{}
	list := root
	stack := []*listItem{root}

	isDigit := false
	startIndex := 0

	for i := 0; i < len(version); i++ {
		c := version[i]
		switch {
		case c == '.':
			if i == startIndex {
				list.add(intZero())
			} else {
				list.add(parseItem(isDigit, version[startIndex:i]))
			}
			startIndex = i + 1

		case c == '-':
			if i == startIndex {
				list.add(intZero())
			} else {
				list.add(parseItem(isDigit, version[startIndex:i]))
			}
			startIndex = i + 1
			if isDigit {
				list.normalize() // "1.0-*" == "1-*"
				if i+1 < len(version) && isASCIIDigit(version[i+1]) {
					// Only a digit-'-'-digit boundary opens a sub-list; that is
					// the sole difference between "1.1" and "1-1".
					nested := &listItem{}
					list.add(nested)
					list = nested
					stack = append(stack, nested)
				}
			}

		case isASCIIDigit(c):
			if !isDigit && i > startIndex {
				list.add(newStringItem(version[startIndex:i], true))
				startIndex = i
			}
			isDigit = true

		default:
			if isDigit && i > startIndex {
				list.add(parseItem(true, version[startIndex:i]))
				startIndex = i
			}
			isDigit = false
		}
	}

	if len(version) > startIndex {
		list.add(parseItem(isDigit, version[startIndex:]))
	}

	for len(stack) > 0 {
		l := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		l.normalize()
	}
	return root
}

func parseItem(isDigit bool, buf string) item {
	if isDigit {
		return newIntItem(buf)
	}
	return newStringItem(buf, false)
}

func isASCIIDigit(c byte) bool { return c >= '0' && c <= '9' }

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}
