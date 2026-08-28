package hpi

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// foldedManifest is a BYTE-EXACT fixture: the Plugin-Dependencies value is
// wrapped at 72 bytes with single-space continuations, exactly as
// java.util.jar.Manifest writes it. The fold boundaries deliberately land in
// the middle of a version string ("203.v15" / "e81a_1b_7a_38") and in the
// middle of a name ("jakarta-mail-api:2" / ".1.3-2"), so a parser that splits
// on ':' before unfolding truncates the list and this test fails.
const foldedManifest = "Manifest-Version: 1.0\r\n" +
	"Short-Name: varroa-mite-auth\r\n" +
	"Long-Name: Varroa Security Realm\r\n" +
	"Plugin-Version: 1.0-SNAPSHOT\r\n" +
	"Jenkins-Version: 2.516.3\r\n" +
	"Plugin-Dependencies: mailer:534.v1b_36f5864073,instance-identity:203.v15\r\n" +
	" e81a_1b_7a_38,display-url-api:2.217.va_6b_de84cc74b_,jakarta-mail-api:2\r\n" +
	" .1.3-2,configuration-as-code:2082.vdb_db_4622e9fa_;resolution:=optional\r\n" +
	"\r\n"

func TestParseManifest_FoldedDependencyLineIsReadWhole(t *testing.T) {
	mf, err := ParseManifest([]byte(foldedManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if mf.ShortName != "varroa-mite-auth" {
		t.Errorf("ShortName = %q", mf.ShortName)
	}
	if mf.Version != "1.0-SNAPSHOT" {
		t.Errorf("Version = %q", mf.Version)
	}
	if mf.LongName != "Varroa Security Realm" {
		t.Errorf("LongName = %q", mf.LongName)
	}
	if mf.RequiredCore != "2.516.3" {
		t.Errorf("RequiredCore = %q", mf.RequiredCore)
	}

	want := []Dependency{
		{Name: "mailer", Min: "534.v1b_36f5864073"},
		{Name: "instance-identity", Min: "203.v15e81a_1b_7a_38"},
		{Name: "display-url-api", Min: "2.217.va_6b_de84cc74b_"},
		{Name: "jakarta-mail-api", Min: "2.1.3-2"},
		{Name: "configuration-as-code", Min: "2082.vdb_db_4622e9fa_", Optional: true},
	}
	if len(mf.Dependencies) != len(want) {
		t.Fatalf("got %d dependencies, want %d: %+v", len(mf.Dependencies), len(want), mf.Dependencies)
	}
	for i := range want {
		if mf.Dependencies[i] != want[i] {
			t.Errorf("dependency %d = %+v, want %+v", i, mf.Dependencies[i], want[i])
		}
	}
}

func TestParseManifest_LineTerminators(t *testing.T) {
	base := []string{
		"Manifest-Version: 1.0",
		"Short-Name: demo",
		"Plugin-Version: 1.2.3",
		"Plugin-Dependencies: mailer:534.v1b_36f5864073",
		"",
	}
	for _, tc := range []struct{ name, sep string }{
		{"crlf", "\r\n"},
		{"lf", "\n"},
		{"cr", "\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mf, err := ParseManifest([]byte(strings.Join(base, tc.sep)))
			if err != nil {
				t.Fatalf("ParseManifest: %v", err)
			}
			if mf.ShortName != "demo" || mf.Version != "1.2.3" {
				t.Fatalf("identity = %q/%q", mf.ShortName, mf.Version)
			}
			if len(mf.Dependencies) != 1 || mf.Dependencies[0].Name != "mailer" {
				t.Fatalf("dependencies = %+v", mf.Dependencies)
			}
		})
	}
}

func TestParseManifest_MixedTerminatorsAndFolding(t *testing.T) {
	// A folded value split across a \r and a \n terminator.
	src := "Short-Name: demo\rPlugin-Version: 1.0\nPlugin-Dependencies: mai\r ler:534.v1\n b_36f5864073\n\n"
	mf, err := ParseManifest([]byte(src))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if len(mf.Dependencies) != 1 {
		t.Fatalf("dependencies = %+v", mf.Dependencies)
	}
	if mf.Dependencies[0].Name != "mailer" || mf.Dependencies[0].Min != "534.v1b_36f5864073" {
		t.Fatalf("dependency = %+v", mf.Dependencies[0])
	}
}

func TestParseManifest_MainSectionOnly(t *testing.T) {
	src := "Short-Name: demo\nPlugin-Version: 1.0\n\nName: some/entry\nJenkins-Version: 9.9.9\nLong-Name: Not Mine\n"
	mf, err := ParseManifest([]byte(src))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if mf.RequiredCore != "" {
		t.Errorf("per-entry section leaked Jenkins-Version = %q", mf.RequiredCore)
	}
	if mf.LongName != "" {
		t.Errorf("per-entry section leaked Long-Name = %q", mf.LongName)
	}
}

func TestParseManifest_KeysAreCaseInsensitiveAndFirstDuplicateWins(t *testing.T) {
	src := "SHORT-NAME: demo\nplugin-version: 1.0\nPlugin-Version: 2.0\nlOnG-nAmE: Demo Plugin\n\n"
	mf, err := ParseManifest([]byte(src))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if mf.ShortName != "demo" {
		t.Errorf("ShortName = %q", mf.ShortName)
	}
	if mf.Version != "1.0" {
		t.Errorf("first duplicate should win, got Version = %q", mf.Version)
	}
	if mf.LongName != "Demo Plugin" {
		t.Errorf("LongName = %q", mf.LongName)
	}
}

func TestParseManifest_ValuesAreVerbatimAfterOneSpace(t *testing.T) {
	// Exactly one space is consumed after the colon; a second space is data.
	src := "Short-Name: demo\nPlugin-Version:  1.0\n\n"
	mf, err := ParseManifest([]byte(src))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if mf.Version != " 1.0" {
		t.Errorf("Version = %q, want %q", mf.Version, " 1.0")
	}
}

func TestParseManifest_OptionalFieldsAbsent(t *testing.T) {
	mf, err := ParseManifest([]byte("Short-Name: demo\nPlugin-Version: 1.0\n\n"))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if mf.LongName != "" || mf.RequiredCore != "" {
		t.Errorf("optional fields not zero: %+v", mf)
	}
	if len(mf.Dependencies) != 0 {
		t.Errorf("dependencies = %+v, want empty", mf.Dependencies)
	}
}

func TestParseManifest_IdentityErrors(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"no short-name", "Plugin-Version: 1.0\n\n"},
		{"empty short-name", "Short-Name:\nPlugin-Version: 1.0\n\n"},
		{"no plugin-version", "Short-Name: demo\n\n"},
		{"empty plugin-version", "Short-Name: demo\nPlugin-Version:\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseManifest([]byte(tc.src)); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestParseDependencies(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    []Dependency
		wantErr bool
	}{
		{
			name:  "empty value yields empty list",
			value: "",
			want:  nil,
		},
		{
			name:  "optional marker",
			value: "mailer:534.v1b_36f5864073,configuration-as-code:2082.vdb_db_4622e9fa_;resolution:=optional",
			want: []Dependency{
				{Name: "mailer", Min: "534.v1b_36f5864073"},
				{Name: "configuration-as-code", Min: "2082.vdb_db_4622e9fa_", Optional: true},
			},
		},
		{
			name:  "resolution attribute name is case-insensitive",
			value: "cfg:1.0;RESOLUTION:=optional",
			want:  []Dependency{{Name: "cfg", Min: "1.0", Optional: true}},
		},
		{
			name:  "whitespace around attributes is trimmed",
			value: "cfg:1.0 ; resolution:=optional ",
			want:  []Dependency{{Name: "cfg", Min: "1.0", Optional: true}},
		},
		{
			name:  "unknown attribute is ignored, entry stays mandatory",
			value: "mailer:534.v1b_36f5864073;futureAttr:=whatever",
			want:  []Dependency{{Name: "mailer", Min: "534.v1b_36f5864073"}},
		},
		{
			name:  "resolution to a non-optional value stays mandatory",
			value: "mailer:1.0;resolution:=required",
			want:  []Dependency{{Name: "mailer", Min: "1.0"}},
		},
		{
			name:    "entry without a colon is an error",
			value:   "mailer",
			wantErr: true,
		},
		{
			name:    "empty name is an error",
			value:   ":1.0",
			wantErr: true,
		},
		{
			name:    "empty minimum is an error",
			value:   "mailer:",
			wantErr: true,
		},
		{
			name:    "empty entry is an error",
			value:   "mailer:1.0,,cfg:2.0",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseDependencies(tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDependencies: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("entry %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseDependencies_DeclaredOrderPreserved(t *testing.T) {
	got, err := ParseDependencies("zeta:1,alpha:2,mike:3")
	if err != nil {
		t.Fatalf("ParseDependencies: %v", err)
	}
	want := []string{"zeta", "alpha", "mike"}
	for i, n := range want {
		if got[i].Name != n {
			t.Fatalf("order not preserved: %+v", got)
		}
	}
}

// TestParseDependencies_MinimumsAreVerbatim proves no normalization crept in.
// A hash-suffixed and a '-'-separated form must both survive byte-identically.
func TestParseDependencies_MinimumsAreVerbatim(t *testing.T) {
	const hashed = "534.v1b_36f5864073"
	const dashed = "4.5.14-269.vfa_2321039a_83"
	got, err := ParseDependencies("mailer:" + hashed + ",git:" + dashed)
	if err != nil {
		t.Fatalf("ParseDependencies: %v", err)
	}
	if got[0].Min != hashed {
		t.Errorf("hash-suffixed minimum = %q, want %q", got[0].Min, hashed)
	}
	if got[1].Min != dashed {
		t.Errorf("dash-separated minimum = %q, want %q", got[1].Min, dashed)
	}
}

// --- archive-level entry points ------------------------------------------

// buildHPI produces an in-memory zip holding the given manifest at the given
// entry path.
func buildHPI(t *testing.T, entry, manifest string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(entry)
	if err != nil {
		t.Fatalf("create zip entry: %v", err)
	}
	if _, err := w.Write([]byte(manifest)); err != nil {
		t.Fatalf("write zip entry: %v", err)
	}
	// A second, unrelated entry so the manifest is not the only member.
	w2, err := zw.Create("WEB-INF/lib/demo.jar")
	if err != nil {
		t.Fatalf("create zip entry: %v", err)
	}
	if _, err := w2.Write([]byte("not a real jar")); err != nil {
		t.Fatalf("write zip entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func TestParseHPIBytes_LocatesManifestCaseInsensitively(t *testing.T) {
	for _, entry := range []string{
		"META-INF/MANIFEST.MF",
		"meta-inf/manifest.mf",
		"Meta-Inf/Manifest.Mf",
	} {
		t.Run(entry, func(t *testing.T) {
			mf, err := ParseHPIBytes(buildHPI(t, entry, foldedManifest))
			if err != nil {
				t.Fatalf("ParseHPIBytes: %v", err)
			}
			if mf.ShortName != "varroa-mite-auth" {
				t.Fatalf("ShortName = %q", mf.ShortName)
			}
			if len(mf.Dependencies) != 5 {
				t.Fatalf("dependencies = %+v", mf.Dependencies)
			}
		})
	}
}

func TestParseHPIBytes_NoManifest(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("WEB-INF/lib/demo.jar")
	_, _ = w.Write([]byte("x"))
	_ = zw.Close()
	if _, err := ParseHPIBytes(buf.Bytes()); err == nil {
		t.Fatal("expected an error for an archive without a manifest")
	}
}

func TestParseHPIBytes_NotAZip(t *testing.T) {
	if _, err := ParseHPIBytes([]byte("this is not a zip archive")); err == nil {
		t.Fatal("expected an error for non-zip bytes")
	}
}

func TestParseHPIFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "demo.hpi")
	if err := os.WriteFile(path, buildHPI(t, "META-INF/MANIFEST.MF", foldedManifest), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	mf, err := ParseHPIFile(path)
	if err != nil {
		t.Fatalf("ParseHPIFile: %v", err)
	}
	if mf.ShortName != "varroa-mite-auth" || mf.Version != "1.0-SNAPSHOT" {
		t.Fatalf("manifest = %+v", mf)
	}

	if _, err := ParseHPIFile(filepath.Join(dir, "missing.hpi")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}
