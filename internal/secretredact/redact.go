// Package secretredact replaces secret values in JCasC and items YAML with a
// fixed marker, so a read surface can show configuration without a
// credential value.
package secretredact

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Marker replaces each secret scalar value.
const Marker = "<redacted>"

// UnparseableMarker replaces the whole content when it does not parse as
// YAML. A redactor that cannot read the content must never return it.
const UnparseableMarker = "<redacted: unparseable content>"

// sensitiveKeyParts are the lower-case key fragments that mark a secret
// value. A key that ends in "id" names a reference, not a value.
var sensitiveKeyParts = []string{"password", "secret", "passphrase", "privatekey", "token"}

// wholeReference matches a value that is one complete ${...} reference.
var wholeReference = regexp.MustCompile(`^\$\{([^{}]+)\}$`)

// plainReferenceName matches a reference to a named secret source entry,
// with no prefix and no inline default value.
var plainReferenceName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// pointerPrefixes are the JCasC lookups that name an outside location. The
// other JCasC lookups (base64, decodeBase64, json, trim) carry their data
// inline, so a value that uses them is redacted.
var pointerPrefixes = []string{"file:", "readfile:", "filebase64:", "readfilebase64:", "sysprop:"}

// credentialStoreKeys open a credential store. Inside a store, every scalar
// is redacted except the values under storeVisibleKeys, because a
// credential type can carry its secret under any key name, also one that
// ends in "id", such as the AppRole secretId.
var credentialStoreKeys = map[string]bool{"domaincredentials": true, "foldercredentials": true}

// storeVisibleKeys hold the non-secret fields of a credential or a domain.
var storeVisibleKeys = map[string]bool{
	"id": true, "scope": true, "description": true, "username": true,
	"name": true, "includes": true, "excludes": true, "schemes": true,
}

// JCasC redacts content that Jenkins reads as configuration-as-code. A value
// that is one whole ${...} reference to a named secret source entry, a file,
// or a system property stays visible, because the text holds no secret.
func JCasC(content string) string {
	return redact(content, true)
}

// Items redacts an items manifest. The mite renders items into config.xml
// without JCasC, so a ${...} value in items is literal text and is redacted
// like every other value.
func Items(content string) string {
	return redact(content, false)
}

// IsSensitiveKey reports whether a mapping key names a secret value.
func IsSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	if strings.HasSuffix(lower, "id") {
		return false
	}
	for _, part := range sensitiveKeyParts {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return false
}

func redact(content string, keepReferences bool) string {
	if strings.TrimSpace(content) == "" {
		return content
	}
	// Every document is encoded again, also one with no secret. The two
	// sides of a diff then come from the same encoder, and a style
	// difference does not show as a change.
	dec := yaml.NewDecoder(strings.NewReader(content))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return UnparseableMarker
		}
		w := walker{keepReferences: keepReferences, seen: map[aliasVisit]bool{}}
		w.redactNode(&doc, mode{})
		docs = append(docs, &doc)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, doc := range docs {
		if err := enc.Encode(doc); err != nil {
			return UnparseableMarker
		}
	}
	if err := enc.Close(); err != nil {
		return UnparseableMarker
	}
	return buf.String()
}

// mode is the redaction state of one node. sensitive is true when a scalar
// at this position is a secret. Outside a credential store, the value of a
// sensitive key is sensitive at every depth below it. inStore is true inside
// a credential store, where every scalar not under a visible key is a secret.
type mode struct {
	sensitive bool
	inStore   bool
}

// aliasVisit is one alias target walked in one mode.
type aliasVisit struct {
	node *yaml.Node
	mode mode
}

// walker carries the state of one document walk. seen stops a walk through
// an alias target that it already walked in the same mode, so a recursive
// anchor cannot loop.
type walker struct {
	keepReferences bool
	seen           map[aliasVisit]bool
}

// redactNode walks n and reports whether it changed a node. It removes
// every comment too, because a comment can hold a secret that is not in
// use, such as a value that is commented out.
func (w *walker) redactNode(n *yaml.Node, m mode) bool {
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	switch n.Kind {
	case yaml.DocumentNode:
		changed := false
		for _, c := range n.Content {
			if w.redactNode(c, mode{inStore: m.inStore}) {
				changed = true
			}
		}
		return changed
	case yaml.MappingNode:
		changed := false
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			stripComments(key)
			lower := strings.ToLower(key.Value)
			child := mode{inStore: m.inStore || credentialStoreKeys[lower]}
			if m.inStore {
				child.sensitive = !storeVisibleKeys[lower]
			} else {
				child.sensitive = m.sensitive || IsSensitiveKey(key.Value)
			}
			if w.redactNode(val, child) {
				changed = true
			}
		}
		return changed
	case yaml.SequenceNode:
		changed := false
		for _, c := range n.Content {
			if w.redactNode(c, m) {
				changed = true
			}
		}
		return changed
	case yaml.AliasNode:
		// The alias target can sit outside the store or the sensitive
		// key. It is walked again in the mode of the alias position.
		visit := aliasVisit{node: n.Alias, mode: m}
		if n.Alias == nil || w.seen[visit] {
			return false
		}
		w.seen[visit] = true
		return w.redactNode(n.Alias, m)
	case yaml.ScalarNode:
		if m.sensitive {
			return redactScalar(n, w.keepReferences)
		}
	}
	return false
}

func redactScalar(n *yaml.Node, keepReferences bool) bool {
	if n.Value == "" || n.Tag == "!!null" || n.Value == Marker {
		return false
	}
	if keepReferences && isPointerReference(n.Value) {
		return false
	}
	n.Value = Marker
	n.Tag = "!!str"
	n.Style = 0
	return true
}

// isPointerReference reports whether v is one whole ${...} reference that
// names where a secret lives and carries no secret data itself.
func isPointerReference(v string) bool {
	m := wholeReference.FindStringSubmatch(v)
	if m == nil {
		return false
	}
	name := m[1]
	if plainReferenceName.MatchString(name) {
		return true
	}
	lower := strings.ToLower(name)
	for _, p := range pointerPrefixes {
		if strings.HasPrefix(lower, p) && len(lower) > len(p) {
			return true
		}
	}
	return false
}

// stripComments removes every comment in the subtree of n. A complex key is
// not walked for secrets, so its comments are removed here.
func stripComments(n *yaml.Node) {
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	for _, c := range n.Content {
		stripComments(c)
	}
}
