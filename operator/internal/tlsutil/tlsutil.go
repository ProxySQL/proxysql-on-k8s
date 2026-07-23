/*
Copyright 2026 ProxySQL.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package tlsutil is a pure (no I/O, no Kubernetes) certificate toolkit:
// it mints a self-signed CA, issues serving certificates signed by that
// CA, and provides the small set of helpers (renewal check, fingerprint,
// SAN derivation) the TLS-management reconciler layers need. It depends
// only on crypto/x509, crypto/tls and the standard library.
package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

// clockSkew backdates NotBefore slightly so certs are immediately valid
// even when the verifier's clock is a little behind the issuer's.
const clockSkew = 5 * time.Minute

// serialBits bounds the random serial number space (RFC 5280 allows up to
// 20 octets / 160 bits; 128 bits of randomness is ample and keeps the
// value well clear of that limit).
const serialBits = 128

// NewCA returns a self-signed CA (PEM cert + key) valid for duration.
func NewCA(commonName string, duration time.Duration) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generating CA key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, nil, fmt.Errorf("generating CA serial: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(duration),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("creating CA certificate: %w", err)
	}

	keyPEM, err = encodeECKey(key)
	if err != nil {
		return nil, nil, err
	}
	return encodeCert(der), keyPEM, nil
}

// IssueServing signs a serving cert for the DNS names/IPs with the CA.
func IssueServing(caCertPEM, caKeyPEM []byte, sans []string, duration time.Duration) (certPEM, keyPEM []byte, err error) {
	caCert, err := parseCert(caCertPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing CA certificate: %w", err)
	}
	caKey, err := parseECKey(caKeyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing CA key: %w", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generating serving key: %w", err)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, nil, fmt.Errorf("generating serving serial: %w", err)
	}

	dnsNames, ipAddrs := splitSANs(sans)
	cn := "proxysql"
	if len(sans) > 0 {
		cn = sans[0]
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     now.Add(duration),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ipAddrs,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("creating serving certificate: %w", err)
	}

	keyPEM, err = encodeECKey(key)
	if err != nil {
		return nil, nil, err
	}
	return encodeCert(der), keyPEM, nil
}

// NeedsRenewal reports whether certPEM expires within renewBefore
// (or fails to parse — parse failure counts as needing renewal).
func NeedsRenewal(certPEM []byte, renewBefore time.Duration) bool {
	cert, err := parseCert(certPEM)
	if err != nil {
		return true
	}
	return time.Until(cert.NotAfter) < renewBefore
}

// LeafFingerprint returns the SHA-256 fingerprint of the first PEM cert.
func LeafFingerprint(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", fmt.Errorf("no PEM block found in certificate data")
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}

// SANsFor derives the full SAN set for a cluster: name, name.ns,
// name.ns.svc, *.<name>-headless.<ns>.svc, <name>-external, plus extras.
// Entries parsing as IPs become IP SANs.
func SANsFor(name, namespace string, extras []string) []string {
	sans := make([]string, 0, 5+len(extras))
	sans = append(sans,
		name,
		name+"."+namespace,
		name+"."+namespace+".svc",
		"*."+name+"-headless."+namespace+".svc",
		name+"-external",
	)
	return append(sans, extras...)
}

// splitSANs partitions a mixed SAN slice into DNS names and IP addresses,
// preserving the order of DNS names.
func splitSANs(sans []string) (dnsNames []string, ips []net.IP) {
	for _, s := range sans {
		if ip := net.ParseIP(s); ip != nil {
			ips = append(ips, ip)
			continue
		}
		dnsNames = append(dnsNames, s)
	}
	return dnsNames, ips
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), serialBits)
	return rand.Int(rand.Reader, limit)
}

func encodeCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func parseCert(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in certificate data")
	}
	return x509.ParseCertificate(block.Bytes)
}

func encodeECKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshaling private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func parseECKey(keyPEM []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in key data")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing PKCS8 key: %w", err)
	}
	ecKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key is not an ECDSA private key: %T", key)
	}
	return ecKey, nil
}
