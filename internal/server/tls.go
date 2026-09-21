package server

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
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const maxTLSFileSize = 1024 * 1024

type TLSIdentity struct {
	directory string
	mu        sync.RWMutex
	cert      *tls.Certificate
}

func LoadOrCreateTLSIdentity(directory string) (*TLSIdentity, string, error) {
	if err := ensureCertificate(directory); err != nil {
		return nil, "", err
	}
	identity := &TLSIdentity{directory: directory}
	if err := identity.reload(); err != nil {
		return nil, "", err
	}
	fingerprint, err := certificateFingerprint(filepath.Join(directory, "cert.pem"))
	return identity, fingerprint, err
}

func (i *TLSIdentity) Config() *tls.Config {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.cert == nil {
		return &tls.Config{MinVersion: tls.VersionTLS13}
	}
	certificate := *i.cert
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
	}
}

func (i *TLSIdentity) reload() error {
	certPath := filepath.Join(i.directory, "cert.pem")
	keyPath := filepath.Join(i.directory, "key.pem")
	if err := checkFileSize(certPath); err != nil {
		return err
	}
	if err := checkFileSize(keyPath); err != nil {
		return err
	}
	certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("load TLS identity: %w", err)
	}
	i.mu.Lock()
	i.cert = &certificate
	i.mu.Unlock()
	return nil
}

func ensureCertificate(directory string) error {
	certPath := filepath.Join(directory, "cert.pem")
	keyPath := filepath.Join(directory, "key.pem")
	certExists := fileExists(certPath)
	keyExists := fileExists(keyPath)
	if certExists || keyExists {
		if !certExists || !keyExists {
			return fmt.Errorf("incomplete TLS identity in %s", directory)
		}
		return nil
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 159)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{"Faro"}, CommonName: "Faro Server"},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.AddDate(5, 0, 0),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return err
	}
	if err := atomicWrite(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return atomicWrite(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func certificateFingerprint(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("invalid TLS certificate")
	}
	digest := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(digest[:]), nil
}

func checkFileSize(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() > maxTLSFileSize {
		return fmt.Errorf("TLS file exceeds %d bytes: %s", maxTLSFileSize, path)
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".faro-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}
