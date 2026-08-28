// Package hpi parses the META-INF/MANIFEST.MF of a Jenkins plugin archive
// (.hpi/.jpi) and extracts the plugin identity, its minimum core version, and
// its declared dependencies.
//
// This package performs NO version comparison. Declared dependency minimums are
// recorded verbatim as opaque strings; comparing a resolved version against a
// declared minimum belongs to the shared plugin-version package owned elsewhere.
// A second comparator in this tree is exactly the duplication that ownership
// assignment exists to prevent.
package hpi

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Dependency is a single entry of a plugin's Plugin-Dependencies declaration.
type Dependency struct {
	Name     string // plugin short name
	Min      string // minimum version, verbatim from the manifest
	Optional bool   // ;resolution:=optional
}

// PluginManifest is the subset of META-INF/MANIFEST.MF that Varroa consumes.
type PluginManifest struct {
	ShortName    string       // Short-Name
	Version      string       // Plugin-Version
	LongName     string       // Long-Name       (may be empty)
	RequiredCore string       // Jenkins-Version (may be empty)
	Dependencies []Dependency // Plugin-Dependencies, in declared order
}

// manifestPath is the archive entry holding the JAR manifest. It is matched
// case-insensitively because the zip entry casing is not guaranteed.
const manifestPath = "meta-inf/manifest.mf"

// Manifest attribute names, lower-cased: manifest keys are matched
// case-insensitively.
const (
	keyShortName    = "short-name"
	keyExtName      = "extension-name"
	keyPluginVer    = "plugin-version"
	keyLongName     = "long-name"
	keyJenkinsVer   = "jenkins-version"
	keyPluginDeps   = "plugin-dependencies"
	attrResolution  = "resolution"
	attrValOptional = "optional"
)

// ErrManifestNotFound is returned when an archive holds no META-INF/MANIFEST.MF.
var ErrManifestNotFound = errors.New("hpi: META-INF/MANIFEST.MF not found in archive")

// ParseHPI reads the manifest out of a Jenkins plugin archive. The zip format
// needs random access, hence io.ReaderAt plus an explicit size.
func ParseHPI(r io.ReaderAt, size int64) (PluginManifest, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return PluginManifest{}, fmt.Errorf("hpi: open archive: %w", err)
	}
	for _, f := range zr.File {
		if !strings.EqualFold(strings.TrimPrefix(f.Name, "/"), manifestPath) {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return PluginManifest{}, fmt.Errorf("hpi: open manifest entry: %w", err)
		}
		data, readErr := io.ReadAll(rc)
		_ = rc.Close()
		if readErr != nil {
			return PluginManifest{}, fmt.Errorf("hpi: read manifest entry: %w", readErr)
		}
		return ParseManifest(data)
	}
	return PluginManifest{}, ErrManifestNotFound
}

// ParseHPIBytes parses an in-memory plugin archive.
func ParseHPIBytes(b []byte) (PluginManifest, error) {
	return ParseHPI(bytes.NewReader(b), int64(len(b)))
}

// ParseHPIFile parses a plugin archive from disk.
func ParseHPIFile(path string) (PluginManifest, error) {
	f, err := os.Open(path) // #nosec G304 -- operator-supplied path, this is a CLI tool
	if err != nil {
		return PluginManifest{}, fmt.Errorf("hpi: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return PluginManifest{}, fmt.Errorf("hpi: stat %s: %w", path, err)
	}
	mf, err := ParseHPI(f, st.Size())
	if err != nil {
		return PluginManifest{}, fmt.Errorf("%s: %w", path, err)
	}
	return mf, nil
}

// ParseManifest parses an already-extracted META-INF/MANIFEST.MF.
//
// It implements the java.util.jar.Manifest line format, which is not plain
// "key: value":
//
//   - lines are separated by \r\n, \n, or \r;
//   - a line beginning with a single space (0x20) continues the previous line;
//     the leading space is dropped and the remainder appended with no
//     separator. Unfolding happens BEFORE any ':' split — writers wrap at 72
//     bytes, so Plugin-Dependencies on a real plugin is always folded and a
//     parser that splits first silently truncates the dependency list;
//   - the first empty line ends the main section; only the main section is read;
//   - within an unfolded line the key is everything before the first ':',
//     followed by at most one consumed space, with the value running to end of
//     line;
//   - keys are matched case-insensitively, values kept verbatim;
//   - where a key repeats, the first occurrence wins.
func ParseManifest(b []byte) (PluginManifest, error) {
	attrs := unfoldMainSection(b)

	var mf PluginManifest
	// Short-Name falls back to Extension-Name: older and some non-Maven-built
	// plugins carry only the latter, and the upload contract admits both.
	mf.ShortName = attrs[keyShortName]
	if mf.ShortName == "" {
		mf.ShortName = attrs[keyExtName]
	}
	mf.Version = attrs[keyPluginVer]
	mf.LongName = attrs[keyLongName]
	mf.RequiredCore = attrs[keyJenkinsVer]

	if mf.ShortName == "" {
		return PluginManifest{}, errors.New("hpi: manifest has no Short-Name or Extension-Name")
	}
	if mf.Version == "" {
		return PluginManifest{}, errors.New("hpi: manifest has no Plugin-Version")
	}

	deps, err := ParseDependencies(attrs[keyPluginDeps])
	if err != nil {
		return PluginManifest{}, fmt.Errorf("hpi: plugin %q: %w", mf.ShortName, err)
	}
	mf.Dependencies = deps
	return mf, nil
}

// unfoldMainSection splits the manifest into lines, unfolds continuations, and
// returns the main section's attributes keyed by lower-cased name. The first
// occurrence of a key wins.
func unfoldMainSection(b []byte) map[string]string {
	attrs := make(map[string]string)

	var cur strings.Builder
	started := false
	ended := false

	commit := func() {
		if !started {
			return
		}
		line := cur.String()
		cur.Reset()
		started = false

		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			return
		}
		key := strings.ToLower(line[:idx])
		val := line[idx+1:]
		// At most one space is consumed after the colon; the rest is verbatim.
		val = strings.TrimPrefix(val, " ")
		if _, dup := attrs[key]; !dup {
			attrs[key] = val
		}
	}

	for _, line := range splitManifestLines(b) {
		if ended {
			break
		}
		if line == "" {
			// Section separator: commit whatever is buffered, then stop —
			// only the main section is read.
			commit()
			ended = true
			continue
		}
		if line[0] == ' ' {
			// Continuation: drop exactly one leading space, append verbatim.
			// A continuation with no preceding line is malformed; treat the
			// remainder as the start of a line rather than panicking.
			cur.WriteString(line[1:])
			started = true
			continue
		}
		commit()
		cur.WriteString(line)
		started = true
	}
	commit()

	return attrs
}

// splitManifestLines splits on \r\n, \n, or \r. A trailing terminator does not
// produce a spurious final empty line, but interior empty lines are preserved
// because they are section separators.
func splitManifestLines(b []byte) []string {
	s := string(b)
	out := make([]string, 0, 32)
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\n':
			out = append(out, s[start:i])
			start = i + 1
		case '\r':
			out = append(out, s[start:i])
			if i+1 < len(s) && s[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// ParseDependencies parses a Plugin-Dependencies value.
//
// The value is a comma-separated list. Each entry splits on ';': the head is
// "name:min" (split on the FIRST ':'), and each remaining segment is an
// attribute. "resolution:=optional" marks the entry optional; the attribute
// name is compared case-insensitively after trimming surrounding whitespace.
// Any other attribute is ignored rather than rejected, so a future Jenkins
// attribute cannot break a lock refresh.
//
// An empty value yields an empty list. An entry whose name or minimum is empty
// is an error. Declared order is preserved.
//
// Minimums are recorded VERBATIM: they are never normalized, truncated, or
// decomposed.
func ParseDependencies(value string) ([]Dependency, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return nil, nil
	}

	parts := strings.Split(v, ",")
	deps := make([]Dependency, 0, len(parts))
	for _, raw := range parts {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			return nil, fmt.Errorf("empty dependency entry in %q", value)
		}
		segs := strings.Split(entry, ";")
		head := strings.TrimSpace(segs[0])
		idx := strings.IndexByte(head, ':')
		if idx < 0 {
			return nil, fmt.Errorf("dependency entry %q is not name:version", entry)
		}
		d := Dependency{
			Name: strings.TrimSpace(head[:idx]),
			Min:  strings.TrimSpace(head[idx+1:]),
		}
		if d.Name == "" {
			return nil, fmt.Errorf("dependency entry %q has an empty name", entry)
		}
		if d.Min == "" {
			return nil, fmt.Errorf("dependency entry %q has an empty minimum version", entry)
		}
		for _, seg := range segs[1:] {
			attr := strings.TrimSpace(seg)
			ai := strings.IndexByte(attr, ':')
			if ai < 0 {
				continue // unknown shape — ignored, not an error
			}
			name := strings.ToLower(strings.TrimSpace(attr[:ai]))
			val := strings.TrimSpace(strings.TrimPrefix(attr[ai+1:], "="))
			if name == attrResolution && val == attrValOptional {
				d.Optional = true
			}
		}
		deps = append(deps, d)
	}
	return deps, nil
}
