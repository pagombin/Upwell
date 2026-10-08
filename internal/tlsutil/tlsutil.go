// Package tlsutil generates and inspects the serving certificate.
package tlsutil

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
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EnsureSelfSigned creates an ECDSA P-256 certificate valid 397 days with the
// hostname and every local IP as subject alternative names, when missing.
func EnsureSelfSigned(certPath, keyPath string) (bool, error) {
	if _, err := os.Stat(certPath); err == nil {
		if _, err := os.Stat(keyPath); err == nil {
			return false, nil
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return false, err
	}
	host, _ := os.Hostname()
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host, Organization: []string{"Upwell self-signed"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(397 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host, "localhost"},
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				tpl.IPAddresses = append(tpl.IPAddresses, ipn.IP)
			}
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return false, err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0o750); err != nil {
		return false, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return false, err
	}
	return true, os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

// Info describes a certificate.
type Info struct {
	Fingerprint string
	NotAfter    time.Time
	Subject     string
	SelfSigned  bool
}

// Inspect reads the leaf certificate.
func Inspect(certPath string) (Info, error) {
	b, err := os.ReadFile(certPath)
	if err != nil {
		return Info{}, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return Info{}, fmt.Errorf("%s is not PEM", certPath)
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return Info{}, err
	}
	sum := sha256.Sum256(c.Raw)
	hexs := strings.ToUpper(hex.EncodeToString(sum[:]))
	var parts []string
	for i := 0; i < len(hexs); i += 2 {
		parts = append(parts, hexs[i:i+2])
	}
	return Info{Fingerprint: strings.Join(parts, ":"), NotAfter: c.NotAfter, Subject: c.Subject.CommonName, SelfSigned: c.Issuer.String() == c.Subject.String()}, nil
}

// Config returns a TLS 1.2+ server configuration.
func Config(certPath, keyPath string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}, nil
}
