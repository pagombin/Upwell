package tlsutil

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Certificate upload (overnight item 12). The configured pair (tls_cert,
// tls_key) stays as installed; an uploaded pair lives in the data directory
// (writable under ProtectSystem=strict) and takes precedence. The previous
// pair is kept for rollback, and the serving certificate is swapped in
// memory, without a restart.

// Holder serves the current certificate and swaps it.
type Holder struct {
	mu       sync.RWMutex
	opMu     sync.Mutex // A5: one install or rollback at a time
	cert     *tls.Certificate
	leaf     *x509.Certificate
	baseCert string
	baseKey  string
	dir      string // uploaded pairs: current.{crt,key}, previous.{crt,key}
	source   string // "configured" or "uploaded"
}

// NewHolder loads the uploaded pair if there is a valid one, else the configured pair.
func NewHolder(baseCert, baseKey, dataDir string) (*Holder, error) {
	h := &Holder{baseCert: baseCert, baseKey: baseKey, dir: filepath.Join(dataDir, "tls")}
	if c, err := tls.LoadX509KeyPair(h.path("current", "crt"), h.path("current", "key")); err == nil {
		h.set(&c, "uploaded")
		return h, nil
	}
	// A5: the key and certificate are two files; a crash between their
	// writes leaves a current pair that does not match. The previous pair is
	// what was served before that install.
	if _, err := os.Stat(h.path("current", "crt")); err == nil {
		if c, err := tls.LoadX509KeyPair(h.path("previous", "crt"), h.path("previous", "key")); err == nil {
			h.set(&c, "uploaded")
			return h, nil
		}
	}
	c, err := tls.LoadX509KeyPair(baseCert, baseKey)
	if err != nil {
		return nil, err
	}
	h.set(&c, "configured")
	return h, nil
}

func (h *Holder) path(which, ext string) string { return filepath.Join(h.dir, which+"."+ext) }

func (h *Holder) set(c *tls.Certificate, source string) {
	leaf, _ := x509.ParseCertificate(c.Certificate[0])
	h.mu.Lock()
	h.cert, h.leaf, h.source = c, leaf, source
	h.mu.Unlock()
}

// GetCertificate is for tls.Config.
func (h *Holder) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cert, nil
}

// Config is a TLS 1.2+ server configuration serving the holder's certificate.
func (h *Holder) Config() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: h.GetCertificate}
}

// Info describes the serving certificate.
func (h *Holder) Info() Info {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return infoOf(h.leaf)
}

// Source is "configured" or "uploaded".
func (h *Holder) Source() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.source
}

// Previous describes the pair a rollback would restore, if any.
func (h *Holder) Previous() (*Info, error) {
	c, err := tls.LoadX509KeyPair(h.path("previous", "crt"), h.path("previous", "key"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return nil, err
	}
	i := infoOf(leaf)
	return &i, nil
}

func infoOf(c *x509.Certificate) Info {
	if c == nil {
		return Info{}
	}
	sum := sha256.Sum256(c.Raw)
	hexs := strings.ToUpper(hex.EncodeToString(sum[:]))
	var parts []string
	for i := 0; i < len(hexs); i += 2 {
		parts = append(parts, hexs[i:i+2])
	}
	return Info{Fingerprint: strings.Join(parts, ":"), NotAfter: c.NotAfter, Subject: c.Subject.CommonName, SelfSigned: c.Issuer.String() == c.Subject.String()}
}

// Validation is what an uploaded pair was found to be.
type Validation struct {
	Subject         string    `json:"subject"`
	Names           []string  `json:"names"`
	Issuer          string    `json:"issuer"`
	NotBefore       time.Time `json:"not_before"`
	NotAfter        time.Time `json:"not_after"`
	Fingerprint     string    `json:"fingerprint"`
	ChainLength     int       `json:"chain_length"`
	SelfSigned      bool      `json:"self_signed"`
	TrustedBySystem bool      `json:"trusted_by_system"`
	Warnings        []string  `json:"warnings"`
}

func pemCerts(b []byte) ([]*x509.Certificate, [][]byte, error) {
	var certs []*x509.Certificate
	var ders [][]byte
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, nil, fmt.Errorf("a certificate could not be read: %w", err)
		}
		certs = append(certs, c)
		ders = append(ders, blk.Bytes)
	}
	return certs, ders, nil
}

// Validate checks an uploaded pair before anything is swapped: the key
// matches the certificate, the certificate is valid now and is for servers,
// and each certificate in the chain is signed by the next. It returns the
// full chain PEM to install.
func Validate(certPEM, keyPEM, chainPEM []byte, now time.Time) (Validation, []byte, error) {
	var v Validation
	full := append(append(bytes.TrimSpace(certPEM), '\n'), bytes.TrimSpace(chainPEM)...)
	certs, _, err := pemCerts(full)
	if err != nil {
		return v, nil, err
	}
	if len(certs) == 0 {
		return v, nil, errors.New("no certificate found: paste the PEM text (-----BEGIN CERTIFICATE-----)")
	}
	if _, err := tls.X509KeyPair(full, keyPEM); err != nil {
		return v, nil, fmt.Errorf("the private key does not match the certificate or cannot be read: %w", err)
	}
	leaf := certs[0]
	v = Validation{Subject: leaf.Subject.CommonName, Names: leaf.DNSNames, Issuer: leaf.Issuer.String(), NotBefore: leaf.NotBefore, NotAfter: leaf.NotAfter,
		Fingerprint: infoOf(leaf).Fingerprint, ChainLength: len(certs), SelfSigned: leaf.Issuer.String() == leaf.Subject.String(), Warnings: []string{}}
	for _, ip := range leaf.IPAddresses {
		v.Names = append(v.Names, ip.String())
	}
	switch {
	case now.Before(leaf.NotBefore):
		return v, nil, fmt.Errorf("the certificate is not valid until %s", leaf.NotBefore.UTC().Format(time.RFC3339))
	case now.After(leaf.NotAfter):
		return v, nil, fmt.Errorf("the certificate expired on %s", leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	if len(leaf.ExtKeyUsage) > 0 {
		server := false
		for _, u := range leaf.ExtKeyUsage {
			if u == x509.ExtKeyUsageServerAuth || u == x509.ExtKeyUsageAny {
				server = true
			}
		}
		if !server {
			return v, nil, errors.New("the certificate is not for servers (no TLS Web Server Authentication usage)")
		}
	}
	for i := 0; i+1 < len(certs); i++ {
		if err := certs[i].CheckSignatureFrom(certs[i+1]); err != nil {
			return v, nil, fmt.Errorf("the chain is broken: certificate %d (%s) is not signed by certificate %d (%s): %v", i+1, certs[i].Subject.CommonName, i+2, certs[i+1].Subject.CommonName, err)
		}
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Intermediates: inter, CurrentTime: now}); err == nil {
		v.TrustedBySystem = true
	} else {
		v.Warnings = append(v.Warnings, "Browsers that trust only public certificate authorities will warn about this certificate ("+err.Error()+"); that is expected for a private CA or a self-signed certificate.")
	}
	if leaf.NotAfter.Sub(now) < 30*24*time.Hour {
		v.Warnings = append(v.Warnings, fmt.Sprintf("It expires in %d days.", int(leaf.NotAfter.Sub(now).Hours()/24)))
	}
	if len(v.Names) == 0 {
		v.Warnings = append(v.Warnings, "It names no host (no subject alternative names); browsers will reject it.")
	}
	return v, full, nil
}

func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Install validates and installs a pair, keeps the current one as the
// previous pair, and starts serving the new one.
func (h *Holder) Install(certPEM, keyPEM, chainPEM []byte, now time.Time) (Validation, error) {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	v, full, err := Validate(certPEM, keyPEM, chainPEM, now)
	if err != nil {
		return v, err
	}
	c, err := tls.X509KeyPair(full, keyPEM)
	if err != nil {
		return v, err
	}
	if err := os.MkdirAll(h.dir, 0o700); err != nil {
		return v, err
	}
	// The pair being replaced becomes the previous one (the configured pair
	// is copied, never moved).
	curCrt, curKey := h.path("current", "crt"), h.path("current", "key")
	if h.Source() == "configured" {
		curCrt, curKey = h.baseCert, h.baseKey
	}
	oc, err1 := os.ReadFile(curCrt)
	ok, err2 := os.ReadFile(curKey)
	if err1 == nil && err2 == nil {
		if err := writeFileAtomic(h.path("previous", "crt"), oc, 0o600); err != nil {
			return v, err
		}
		if err := writeFileAtomic(h.path("previous", "key"), ok, 0o600); err != nil {
			return v, err
		}
	}
	if err := writeFileAtomic(h.path("current", "key"), keyPEM, 0o600); err != nil {
		return v, err
	}
	if err := writeFileAtomic(h.path("current", "crt"), full, 0o600); err != nil {
		return v, err
	}
	h.set(&c, "uploaded")
	return v, nil
}

// Rollback serves the previous pair again; the replaced pair becomes the
// previous one, so a second rollback undoes the first.
func (h *Holder) Rollback() (Info, error) {
	h.opMu.Lock()
	defer h.opMu.Unlock()
	pc, err1 := os.ReadFile(h.path("previous", "crt"))
	pk, err2 := os.ReadFile(h.path("previous", "key"))
	if err1 != nil || err2 != nil {
		return Info{}, errors.New("there is no previous certificate to go back to")
	}
	c, err := tls.X509KeyPair(pc, pk)
	if err != nil {
		return Info{}, fmt.Errorf("the previous certificate can no longer be loaded: %w", err)
	}
	cc, err1 := os.ReadFile(h.path("current", "crt"))
	ck, err2 := os.ReadFile(h.path("current", "key"))
	if err := writeFileAtomic(h.path("current", "crt"), pc, 0o600); err != nil {
		return Info{}, err
	}
	if err := writeFileAtomic(h.path("current", "key"), pk, 0o600); err != nil {
		return Info{}, err
	}
	if err1 == nil && err2 == nil {
		writeFileAtomic(h.path("previous", "crt"), cc, 0o600) //nolint:errcheck
		writeFileAtomic(h.path("previous", "key"), ck, 0o600) //nolint:errcheck
	}
	h.set(&c, "uploaded")
	return h.Info(), nil
}
