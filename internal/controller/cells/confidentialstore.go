package cells

import (
	"bytes"
	"crypto/aes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// The four keys of a pool's Jenkins confidential store. Each name is also
// the file name Jenkins reads inside $JENKINS_HOME/secrets/, because
// DefaultConfidentialStore names each file after the ConfidentialKey id.
const (
	// KeyMasterKey holds the master secret that encrypts every other key.
	KeyMasterKey = "master.key"
	// KeyJnlpSecret holds the HMAC key that derives each inbound agent's
	// secret.
	KeyJnlpSecret = "jenkins.slaves.JnlpSlaveAgentProtocol.secret"
	// KeyHudsonUtilSecret holds the key that encrypts every Secret value in
	// a config file.
	KeyHudsonUtilSecret = "hudson.util.Secret"
	// KeyInstanceIdentity holds the key that encrypts the instance identity.
	// The RSA key pair itself lives in $JENKINS_HOME/identity.key.enc, which
	// the pool does not share.
	KeyInstanceIdentity = "org.jenkinsci.main.modules.instance_identity.InstanceIdentity.KEY"
)

// PoolSecretPrefix is the name prefix of a pool's confidential store. The
// full name is PoolSecretPrefix + the pool name, in the operator namespace.
const PoolSecretPrefix = "varroa-cell-secrets-"

// confidentialStoreMagic is the ASCII trailer DefaultConfidentialStore
// appends to a payload before it encrypts, and verifies after it decrypts. A
// file whose trailer does not verify is silently replaced with a fresh key.
const confidentialStoreMagic = "::::MAGIC::::"

// The payload lengths Jenkins itself would generate for each key.
// HMACConfidentialKey stores an HmacSHA256 key, and CryptoConfidentialKey
// stores 256 random bytes of which it uses the first 16.
const (
	masterKeyRandomBytes   = 128
	jnlpSecretPayloadBytes = 32
	cryptoKeyPayloadBytes  = 256
)

// ErrNoMasterKey reports a store that holds another key and no master.key.
// The operator cannot read the other keys, and a new master.key would
// invalidate them, so the pass writes nothing.
var ErrNoMasterKey = errors.New("the pool secret holds no master.key")

// encryptedKeyPayloadBytes gives the payload length of each encrypted key.
var encryptedKeyPayloadBytes = map[string]int{
	KeyJnlpSecret:       jnlpSecretPayloadBytes,
	KeyHudsonUtilSecret: cryptoKeyPayloadBytes,
	KeyInstanceIdentity: cryptoKeyPayloadBytes,
}

// encryptedKeyOrder fixes the order in which the writer builds the encrypted
// keys, so a mint and a repair are deterministic.
var encryptedKeyOrder = []string{KeyJnlpSecret, KeyHudsonUtilSecret, KeyInstanceIdentity}

// PoolSecretName returns the name of a pool's confidential store.
func PoolSecretName(pool string) string {
	return PoolSecretPrefix + pool
}

// StoreKeys returns the four key names a complete store holds.
func StoreKeys() []string {
	return []string{KeyMasterKey, KeyJnlpSecret, KeyHudsonUtilSecret, KeyInstanceIdentity}
}

// NewMasterKey returns the content of a master.key file: 128 random bytes as
// lowercase hexadecimal, with no trailing newline.
func NewMasterKey() string {
	return hex.EncodeToString(randomBytes(masterKeyRandomBytes))
}

// EncryptKey returns the file content DefaultConfidentialStore reads for one
// key. It derives the AES key from the master.key text, appends the magic
// trailer to payload, and encrypts with AES in ECB mode and PKCS#7 padding.
// PKCS#7 is byte-identical to the PKCS#5 padding the Java cipher applies for
// a 16-byte block.
func EncryptKey(masterKeyHex string, payload []byte) ([]byte, error) {
	digest := sha256.Sum256([]byte(masterKeyHex))
	block, err := aes.NewCipher(digest[:aes.BlockSize])
	if err != nil {
		return nil, fmt.Errorf("build the aes cipher: %w", err)
	}
	plain := make([]byte, 0, len(payload)+len(confidentialStoreMagic)+aes.BlockSize)
	plain = append(plain, payload...)
	plain = append(plain, confidentialStoreMagic...)
	plain = pkcs7Pad(plain, aes.BlockSize)
	out := make([]byte, len(plain))
	for i := 0; i < len(plain); i += aes.BlockSize {
		block.Encrypt(out[i:i+aes.BlockSize], plain[i:i+aes.BlockSize])
	}
	return out, nil
}

// MintStore returns a complete store: a fresh master.key and the three
// encrypted keys built under it.
func MintStore() (map[string][]byte, error) {
	master := NewMasterKey()
	out := map[string][]byte{KeyMasterKey: []byte(master)}
	for _, key := range encryptedKeyOrder {
		value, err := newEncryptedKey(master, key)
		if err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, nil
}

// RepairStore returns only the keys existing lacks. It builds each one under
// the store's own master.key, so it changes no key that already holds a
// value. It returns ErrNoMasterKey for a store with no master.key.
func RepairStore(existing map[string][]byte) (map[string][]byte, error) {
	// Jenkins reads the master key with readTrim, so the derivation drops
	// the surrounding whitespace a hand edit can add.
	master := strings.TrimSpace(string(existing[KeyMasterKey]))
	if master == "" {
		return nil, ErrNoMasterKey
	}
	out := map[string][]byte{}
	for _, key := range encryptedKeyOrder {
		if len(existing[key]) > 0 {
			continue
		}
		value, err := newEncryptedKey(master, key)
		if err != nil {
			return nil, err
		}
		out[key] = value
	}
	return out, nil
}

// VerifyStore reports whether every encrypted key of a store decrypts under
// the store's own master.key. A key that does not decrypt is worse than an
// absent key: the presence test passes, and Jenkins then silently replaces
// that key with a fresh one on every cell, which breaks the pool's own
// guarantee. It returns ErrNoMasterKey for a store with no master.key, and a
// named error for the first key that does not verify.
func VerifyStore(data map[string][]byte) error {
	master := strings.TrimSpace(string(data[KeyMasterKey]))
	if master == "" {
		return ErrNoMasterKey
	}
	for _, key := range encryptedKeyOrder {
		if _, err := DecryptKey(master, data[key]); err != nil {
			return fmt.Errorf("the key %q does not decrypt: %w", key, err)
		}
	}
	return nil
}

// DecryptKey reverses EncryptKey. It answers the payload with the magic
// trailer removed, and an error for a ciphertext that is not a whole number
// of AES blocks, that carries invalid padding, or that carries no trailer.
func DecryptKey(masterKeyHex string, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("the ciphertext is %d bytes, which is not a whole number of blocks", len(ciphertext))
	}
	digest := sha256.Sum256([]byte(masterKeyHex))
	block, err := aes.NewCipher(digest[:aes.BlockSize])
	if err != nil {
		return nil, fmt.Errorf("build the aes cipher: %w", err)
	}
	plain := make([]byte, len(ciphertext))
	for i := 0; i < len(ciphertext); i += aes.BlockSize {
		block.Decrypt(plain[i:i+aes.BlockSize], ciphertext[i:i+aes.BlockSize])
	}
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(plain) {
		return nil, fmt.Errorf("the padding byte is %d, which is out of range", pad)
	}
	for _, b := range plain[len(plain)-pad:] {
		if int(b) != pad {
			return nil, errors.New("the padding bytes are not equal")
		}
	}
	plain = plain[:len(plain)-pad]
	if !bytes.HasSuffix(plain, []byte(confidentialStoreMagic)) {
		return nil, errors.New("the plaintext carries no magic trailer")
	}
	return plain[:len(plain)-len(confidentialStoreMagic)], nil
}

// newEncryptedKey builds one encrypted key from a fresh random payload of
// the length Jenkins itself would generate.
func newEncryptedKey(masterKeyHex, key string) ([]byte, error) {
	size, ok := encryptedKeyPayloadBytes[key]
	if !ok {
		return nil, fmt.Errorf("no payload length for the key %q", key)
	}
	return EncryptKey(masterKeyHex, randomBytes(size))
}

// randomBytes returns n bytes from the cryptographic random source. That
// source never fails: it panics inside the runtime rather than answering a
// short read.
func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// pkcs7Pad appends the padding the Java cipher expects. A plaintext whose
// length is already a whole number of blocks gains one full block of
// padding.
func pkcs7Pad(plain []byte, blockSize int) []byte {
	pad := blockSize - len(plain)%blockSize
	for i := 0; i < pad; i++ {
		plain = append(plain, byte(pad))
	}
	return plain
}
