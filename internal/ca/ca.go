// Package ca implements a simple internal certificate authority that
// generates ed25519 key pairs and x509 certificates for mTLS between
// mite sidecars and the Varroa server.
package ca

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

// CA is an internal certificate authority for mTLS between mite sidecars and
// the Varroa server. It holds an ed25519 private key and self-signed CA
// certificate.
type CA struct {
	privateKey ed25519.PrivateKey
	cert       *x509.Certificate
	certPEM    []byte
	pool       *x509.CertPool
}

// NewCA generates an ed25519 key pair and creates a self-signed CA certificate
// valid for 10 years.
func NewCA() (*CA, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519 key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "Varroa Internal CA",
			Organization: []string{"Varroa"},
		},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("create self-signed CA cert: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	pool := x509.NewCertPool()
	pool.AddCert(cert)

	return &CA{
		privateKey: priv,
		cert:       cert,
		certPEM:    certPEM,
		pool:       pool,
	}, nil
}

// IssueServerCert issues a server certificate for the Varroa gRPC server.
// The certificate's SANs are exactly the names passed as extraSANs (the
// caller — the gateway — supplies its own service DNS). It is valid for 1
// year and returns a tls.Certificate ready for use with crypto/tls.
func (c *CA) IssueServerCert(extraSANs ...string) (tls.Certificate, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate server key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate serial: %w", err)
	}

	now := time.Now()
	dnsNames := make([]string, 0, len(extraSANs))
	dnsNames = append(dnsNames, extraSANs...)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   dnsNames[0],
			Organization: []string{"Varroa"},
		},
		NotBefore: now.Add(-1 * time.Hour),
		NotAfter:  now.Add(365 * 24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
		DNSNames: dnsNames,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, pub, c.privateKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create server cert: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("marshal server key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})

	return tls.X509KeyPair(certPEM, keyPEM)
}

// IssueMiteCert issues a client certificate for a mite sidecar agent using
// the provided ed25519 public key. The CommonName is set to
// "controllerName.namespace" and the certificate is valid for 72 hours.
func (c *CA) IssueMiteCert(controllerName, namespace string, pubkey ed25519.PublicKey) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	cn := fmt.Sprintf("%s.%s", controllerName, namespace)
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   cn,
			Organization: []string{"Varroa"},
		},
		NotBefore: now.Add(-1 * time.Hour),
		NotAfter:  now.Add(71 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
		},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, pubkey, c.privateKey)
	if err != nil {
		return nil, fmt.Errorf("create mite cert: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse mite cert: %w", err)
	}

	return cert, nil
}

// VerifyClientCert verifies a client certificate chain against the CA
// certificate pool. It returns the parsed leaf certificate on success.
func (c *CA) VerifyClientCert(rawCerts [][]byte) (*x509.Certificate, error) {
	if len(rawCerts) == 0 {
		return nil, fmt.Errorf("no certificates provided")
	}

	opts := x509.VerifyOptions{
		Roots:     c.pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return nil, fmt.Errorf("parse client cert: %w", err)
	}

	var intermediates *x509.CertPool
	if len(rawCerts) > 1 {
		intermediates = x509.NewCertPool()
		for _, raw := range rawCerts[1:] {
			ic, err := x509.ParseCertificate(raw)
			if err != nil {
				return nil, fmt.Errorf("parse intermediate cert: %w", err)
			}
			intermediates.AddCert(ic)
		}
	}
	opts.Intermediates = intermediates

	if _, err := leaf.Verify(opts); err != nil {
		return nil, fmt.Errorf("verify client cert: %w", err)
	}

	return leaf, nil
}

// Persist returns the PEM-encoded CA certificate and private key for
// storage in a Kubernetes Secret so the CA survives operator restarts.
func (c *CA) Persist() (certPEM, keyPEM []byte, err error) {
	keyBytes, err := x509.MarshalPKCS8PrivateKey(c.privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal CA private key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	return c.certPEM, keyPEM, nil
}

// LoadCA reconstructs a CA from PEM-encoded certificate and private key bytes
// retrieved from persistent storage (e.g. a Kubernetes Secret).
func LoadCA(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("decode CA cert PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("decode CA key PEM")
	}
	priv, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA key: %w", err)
	}
	privateKey, ok := priv.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("CA key is not ed25519, got %T", priv)
	}

	pool := x509.NewCertPool()
	pool.AddCert(cert)

	return &CA{
		privateKey: privateKey,
		cert:       cert,
		certPEM:    certPEM,
		pool:       pool,
	}, nil
}

// PrivateKey returns the CA's ed25519 private key, used by TokenSigner for
// HMAC-based bootstrap tokens.
func (c *CA) PrivateKey() ed25519.PrivateKey {
	return c.privateKey
}

// BootstrapHMACKey derives a dedicated 32-byte key for signing mite bootstrap tokens,
// independent of the CA's cert-signing private key. It uses HKDF-Expand (RFC 5869 §2.3)
// with the ed25519 seed as the pseudorandom key and a fixed info label, so the bootstrap
// HMAC key can never be confused with — or recovered from — the signing key material.
func (c *CA) BootstrapHMACKey() []byte {
	const info = "varroa/mite-bootstrap-token/hmac-sha256/v1"
	// HKDF-Expand, single output block (L = 32 = SHA-256 size):
	// T(1) = HMAC(PRK, info || 0x01); OKM = T(1).
	mac := hmac.New(sha256.New, c.privateKey.Seed())
	mac.Write([]byte(info))
	mac.Write([]byte{0x01})
	return mac.Sum(nil)
}

// CertPool returns the CA's certificate pool for use in TLS configuration.
func (c *CA) CertPool() *x509.CertPool {
	return c.pool
}

// CAPEM returns the PEM-encoded CA certificate.
func (c *CA) CAPEM() []byte {
	return c.certPEM
}
