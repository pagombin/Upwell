package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type kp struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
	kpem []byte
}

func mk(t *testing.T, cn string, parent *kp, ca bool, from, to time.Time) kp {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn}, NotBefore: from, NotAfter: to,
		DNSNames: []string{cn}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, IsCA: ca, BasicConstraintsValid: true}
	signer, signKey := tpl, key
	if parent != nil {
		signer, signKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, signer, &key.PublicKey, signKey)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	kb, _ := x509.MarshalECPrivateKey(key)
	return kp{c, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})}
}

// TestCertificateUpload (overnight item 12): validation refuses a key that
// does not match, an expired certificate and a broken chain without
// changing anything; a valid pair is served at once; rollback restores the
// previous pair and a second rollback undoes it.
func TestCertificateUpload(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	base := mk(t, "upwell.local", nil, false, now.Add(-time.Hour), now.Add(365*24*time.Hour))
	os.WriteFile(filepath.Join(dir, "cert.pem"), base.pem, 0o600)
	os.WriteFile(filepath.Join(dir, "key.pem"), base.kpem, 0o600)
	h, err := NewHolder(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	baseFP := h.Info().Fingerprint
	ca := mk(t, "Example CA", nil, true, now.Add(-time.Hour), now.Add(10*365*24*time.Hour))
	inter := mk(t, "Example Intermediate", &ca, true, now.Add(-time.Hour), now.Add(5*365*24*time.Hour))
	leaf := mk(t, "console.example.com", &inter, false, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	other := mk(t, "other.example.com", &inter, false, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	expired := mk(t, "old.example.com", &inter, false, now.Add(-48*time.Hour), now.Add(-time.Hour))

	for _, c := range []struct {
		name             string
		cert, key, chain []byte
		want             string
	}{
		{"mismatched key", leaf.pem, other.kpem, inter.pem, "does not match"},
		{"expired", expired.pem, expired.kpem, inter.pem, "expired"},
		{"broken chain", leaf.pem, leaf.kpem, ca.pem, "chain is broken"},
		{"not PEM", []byte("hello"), leaf.kpem, nil, "no certificate"},
	} {
		if _, err := h.Install(c.cert, c.key, c.chain, now); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
		if h.Info().Fingerprint != baseFP {
			t.Fatalf("%s changed the served certificate", c.name)
		}
	}
	v, err := h.Install(leaf.pem, leaf.kpem, inter.pem, now)
	if err != nil {
		t.Fatal(err)
	}
	if v.ChainLength != 2 || v.Subject != "console.example.com" || len(v.Warnings) == 0 {
		t.Fatalf("validation: %+v", v)
	}
	got, _ := h.GetCertificate(nil)
	if len(got.Certificate) != 2 || h.Info().Subject != "console.example.com" || h.Source() != "uploaded" {
		t.Fatalf("not serving the new pair: %s", h.Info().Subject)
	}
	if p, _ := h.Previous(); p == nil || p.Fingerprint != baseFP {
		t.Fatalf("previous pair: %+v", p)
	}
	// A new holder (a restart) serves the uploaded pair.
	h2, _ := NewHolder(filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem"), filepath.Join(dir, "data"))
	if h2.Info().Subject != "console.example.com" {
		t.Fatalf("after a restart: %s", h2.Info().Subject)
	}
	if _, err := h.Rollback(); err != nil || h.Info().Fingerprint != baseFP {
		t.Fatalf("rollback: %v, serving %s", err, h.Info().Subject)
	}
	if _, err := h.Rollback(); err != nil || h.Info().Subject != "console.example.com" {
		t.Fatalf("second rollback: %v, serving %s", err, h.Info().Subject)
	}
}
