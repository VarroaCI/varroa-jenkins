// Command gen-cell-secrets-fixture writes the confidential-store fixture the
// plugin's JenkinsRule test boots on. Run it from the repository root:
//
//	go run ./hack/gen-cell-secrets-fixture
//
// The fixture is the Go writer's own output, so a format change regenerates
// it and the plugin test proves the new format against a real Jenkins.
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
)

// fixtureDir is where the plugin test reads the fixture.
const fixtureDir = "plugin/src/test/resources/org/jenkinsci/plugins/varroamiteauth/confidentialstore"

// macMessage is the node name the plugin test computes an agent secret for.
const macMessage = "x"

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
		return fmt.Errorf("create the fixture directory: %w", err)
	}

	master := cells.NewMasterKey()
	if err := write(cells.KeyMasterKey, []byte(master)); err != nil {
		return err
	}

	jnlpPayload, err := randomBytes(32)
	if err != nil {
		return err
	}
	if err := writeEncrypted(master, cells.KeyJnlpSecret, jnlpPayload); err != nil {
		return err
	}
	for _, key := range []string{cells.KeyHudsonUtilSecret, cells.KeyInstanceIdentity} {
		payload, err := randomBytes(256)
		if err != nil {
			return err
		}
		if err := writeEncrypted(master, key, payload); err != nil {
			return err
		}
	}

	// HMACConfidentialKey.mac hex encodes an HmacSHA256 over the UTF-8 bytes
	// of the message, keyed with the stored payload. The plugin test asserts
	// this value against a real Jenkins.
	mac := hmac.New(sha256.New, jnlpPayload)
	if _, err := mac.Write([]byte(macMessage)); err != nil {
		return fmt.Errorf("compute the agent mac: %w", err)
	}
	body := fmt.Sprintf("jnlpMacFor%s=%s\n", macMessage, hex.EncodeToString(mac.Sum(nil)))
	return write("expected.properties", []byte(body))
}

// writeEncrypted writes one key in the format DefaultConfidentialStore reads.
func writeEncrypted(master, key string, payload []byte) error {
	value, err := cells.EncryptKey(master, payload)
	if err != nil {
		return fmt.Errorf("encrypt %s: %w", key, err)
	}
	return write(key, value)
}

// write puts one fixture file in place.
func write(name string, body []byte) error {
	path := filepath.Join(fixtureDir, name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// randomBytes returns n bytes from the cryptographic random source.
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("read random bytes: %w", err)
	}
	return b, nil
}
