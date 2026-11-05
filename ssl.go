package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Local CA (like mkcert) whose name constraints only allow .<tld>, localhost and loopback IPs,
// so its key can't be abused to impersonate real sites. Apache uses a leaf signed by it.
func (a *App) sslFiles() (caCrt, caKey, crt, key string) {
	return a.path(`etc\ssl\devpanel-ca.crt`), a.path(`etc\ssl\devpanel-ca.key`), a.path(`etc\ssl\devpanel.crt`), a.path(`etc\ssl\devpanel.key`)
}

func writePEM(path, typ string, der []byte, mode os.FileMode) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), mode)
}

func readCert(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("bad PEM: " + path)
	}
	return x509.ParseCertificate(blk.Bytes)
}

func readKey(path string) (crypto.Signer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("bad PEM: " + path)
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	s, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("unsupported key")
	}
	return s, nil
}

func newKey(path string) (crypto.Signer, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	return k, writePEM(path, "PRIVATE KEY", der, 0600)
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

var loopback = []*net.IPNet{{IP: net.IPv4(127, 0, 0, 0).To4(), Mask: net.CIDRMask(8, 32)}, {IP: net.IPv6loopback, Mask: net.CIDRMask(128, 128)}}

func (a *App) ensureCA() (*x509.Certificate, crypto.Signer, error) {
	caCrt, caKey, _, _ := a.sslFiles()
	if c, err := readCert(caCrt); err == nil {
		if k, err := readKey(caKey); err == nil {
			return c, k, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(caKey), 0755); err != nil {
		return nil, nil, err
	}
	k, err := newKey(caKey)
	if err != nil {
		return nil, nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:                serial(),
		Subject:                     pkix.Name{CommonName: "DevPanel Local CA", Organization: []string{"DevPanel"}},
		NotBefore:                   time.Now().Add(-time.Hour),
		NotAfter:                    time.Now().AddDate(10, 0, 0),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{a.cfg.TLD, "localhost"},
		PermittedIPRanges:           loopback,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, k.Public(), k)
	if err != nil {
		return nil, nil, err
	}
	if err := writePEM(caCrt, "CERTIFICATE", der, 0644); err != nil {
		return nil, nil, err
	}
	c, err := x509.ParseCertificate(der)
	return c, k, err
}

// sslNames returns the SANs for the given hosts; hosts outside the CA's allowed domains are skipped.
func (a *App) sslNames(hosts []string) (names, skipped []string) {
	names = []string{"localhost"}
	for _, h := range hosts {
		if h == a.cfg.TLD || !strings.HasSuffix(h, "."+a.cfg.TLD) {
			skipped = append(skipped, h)
			continue
		}
		names = append(names, h, "*."+h)
	}
	return names, skipped
}

// writeLeaf (re)issues the site certificate when the host list changed or it expires within 30 days.
func (a *App) writeLeaf(hosts []string) (changed bool, err error) {
	ca, caKey, err := a.ensureCA()
	if err != nil {
		return false, err
	}
	_, _, crtPath, keyPath := a.sslFiles()
	names, _ := a.sslNames(hosts)
	if old, err := readCert(crtPath); err == nil {
		_, kerr := readKey(keyPath)
		have, want := slices.Clone(old.DNSNames), slices.Clone(names)
		slices.Sort(have)
		slices.Sort(want)
		if kerr == nil && slices.Equal(have, want) && old.CheckSignatureFrom(ca) == nil && time.Until(old.NotAfter) > 30*24*time.Hour {
			return false, nil
		}
	}
	k, err := newKey(keyPath)
	if err != nil {
		return false, err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "DevPanel sites", Organization: []string{"DevPanel"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 0, 825),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     names,
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, k.Public(), caKey)
	if err != nil {
		return false, err
	}
	return true, writePEM(crtPath, "CERTIFICATE", der, 0644)
}

type SSLStatus struct {
	Trusted  bool     `json:"trusted"`
	Expires  string   `json:"expires"`
	Names    int      `json:"names"`
	Skipped  []string `json:"skipped"` // hosts that can't get a trusted cert (outside .<tld>)
	CertPath string   `json:"certPath"`
}

// SSL reports whether Windows trusts the site certificate (via the OS verifier).
func (a *App) SSL() SSLStatus {
	caCrt, _, crt, _ := a.sslFiles()
	st := SSLStatus{CertPath: caCrt}
	if list, err := a.sites(); err == nil {
		_, st.Skipped = a.sslNames(allHosts(list))
	}
	leaf, err := readCert(crt)
	if err != nil {
		return st
	}
	st.Expires, st.Names = leaf.NotAfter.Format("2006-01-02"), len(leaf.DNSNames)
	ca, err := readCert(caCrt)
	if err != nil {
		return st
	}
	inter := x509.NewCertPool()
	inter.AddCert(ca)
	_, err = leaf.Verify(x509.VerifyOptions{DNSName: "localhost", Intermediates: inter})
	st.Trusted = err == nil
	return st
}

// TrustCA adds the DevPanel CA to the current user's Trusted Root store (Windows shows a confirmation).
func (a *App) TrustCA() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, _, err := a.ensureCA(); err != nil {
		return "", err
	}
	caCrt, _, _, _ := a.sslFiles()
	out, err := exec.Command("certutil", "-user", "-addstore", "Root", caCrt).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("certutil: %v %s", err, strings.TrimSpace(string(out)))
	}
	return "CA trusted. Restart the browser; Firefox needs security.enterprise_roots.enabled=true.", nil
}
