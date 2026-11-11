package ingest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Names of the generated certificate files inside the state directory.
const (
	GeneratedCertName = "ingest-cert.pem"
	GeneratedKeyName  = "ingest-key.pem"
)

// Fingerprint returns the lowercase hex SHA-256 of a DER certificate.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// LoadOrCreateCert loads the ingest certificate from certFile/keyFile, or,
// if both are empty, from stateDir, generating a self-signed ECDSA P-256
// certificate there on first use. It returns the certificate and the
// fingerprint of its leaf.
func LoadOrCreateCert(certFile, keyFile, stateDir string) (tls.Certificate, string, error) {
	if certFile == "" && keyFile == "" {
		if stateDir == "" {
			return tls.Certificate{}, "", errors.New("ingest tls: no cert_file/key_file and no state_dir")
		}
		certFile = filepath.Join(stateDir, GeneratedCertName)
		keyFile = filepath.Join(stateDir, GeneratedKeyName)
		if _, err := os.Stat(certFile); errors.Is(err, os.ErrNotExist) {
			if err := generateCert(stateDir, certFile, keyFile); err != nil {
				return tls.Certificate{}, "", fmt.Errorf("ingest tls: generate certificate: %w", err)
			}
		}
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("ingest tls: %w", err)
	}
	return cert, Fingerprint(cert.Certificate[0]), nil
}

func generateCert(dir, certFile, keyFile string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "birdcam-server ingest"},
		DNSNames:     []string{"birdcam-ingest"},
		// Agents pin the certificate hash; validity dates are not checked
		// (a Pi's clock may be wrong at boot), so make it long-lived.
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(50, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	// Write the key first: a cert without key is treated as existing.
	if err := writeFileAtomic(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return writeFileAtomic(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path))
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ServerTLSConfig returns the TLS config used for ingest listeners.
func ServerTLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}
}

// PinnedClientConfig returns a client TLS config that accepts only a server
// leaf certificate with the given SHA-256 fingerprint (agents/tests).
func PinnedClientConfig(fingerprint string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // replaced by the pin check below
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no server certificate")
			}
			if got := Fingerprint(cs.PeerCertificates[0].Raw); got != fingerprint {
				return fmt.Errorf("ingest certificate fingerprint mismatch: got %s, want %s", got, fingerprint)
			}
			return nil
		},
	}
}
