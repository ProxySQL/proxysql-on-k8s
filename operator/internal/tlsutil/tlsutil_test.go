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

package tlsutil

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"reflect"
	"testing"
	"time"
)

func mustParseCert(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatalf("failed to PEM-decode certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse certificate: %v", err)
	}
	return cert
}

func TestNewCA_ProducesSelfSignedCA(t *testing.T) {
	certPEM, keyPEM, err := NewCA("test-ca", 24*time.Hour)
	if err != nil {
		t.Fatalf("NewCA() error = %v", err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		t.Fatalf("NewCA() returned empty PEM: certPEM=%d keyPEM=%d bytes", len(certPEM), len(keyPEM))
	}

	cert := mustParseCert(t, certPEM)
	if !cert.IsCA {
		t.Errorf("CA cert IsCA = false, want true")
	}
	if !cert.BasicConstraintsValid {
		t.Errorf("CA cert BasicConstraintsValid = false, want true")
	}
	if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Errorf("CA cert missing KeyUsageCertSign: %v", cert.KeyUsage)
	}
	if cert.KeyUsage&x509.KeyUsageCRLSign == 0 {
		t.Errorf("CA cert missing KeyUsageCRLSign: %v", cert.KeyUsage)
	}
	if cert.Subject.CommonName != "test-ca" {
		t.Errorf("CA cert CommonName = %q, want %q", cert.Subject.CommonName, "test-ca")
	}

	// Self-signed: the cert must verify against its own public key.
	if err := cert.CheckSignatureFrom(cert); err != nil {
		t.Errorf("CA cert is not self-signed: %v", err)
	}
}

func TestIssueServing_ChainVerifiesAgainstCA(t *testing.T) {
	caCertPEM, caKeyPEM, err := NewCA("test-ca", 24*time.Hour)
	if err != nil {
		t.Fatalf("NewCA() error = %v", err)
	}

	sans := []string{"proxysql.default.svc", "10.0.0.5"}
	servingCertPEM, servingKeyPEM, err := IssueServing(caCertPEM, caKeyPEM, sans, time.Hour)
	if err != nil {
		t.Fatalf("IssueServing() error = %v", err)
	}
	if len(servingCertPEM) == 0 || len(servingKeyPEM) == 0 {
		t.Fatalf("IssueServing() returned empty PEM: certPEM=%d keyPEM=%d bytes", len(servingCertPEM), len(servingKeyPEM))
	}

	leaf := mustParseCert(t, servingCertPEM)
	if leaf.IsCA {
		t.Errorf("serving cert IsCA = true, want false")
	}
	if leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Errorf("serving cert missing KeyUsageDigitalSignature: %v", leaf.KeyUsage)
	}
	found := false
	for _, eku := range leaf.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth {
			found = true
		}
	}
	if !found {
		t.Errorf("serving cert missing ExtKeyUsageServerAuth: %v", leaf.ExtKeyUsage)
	}

	pool := x509.NewCertPool()
	pool.AddCert(mustParseCert(t, caCertPEM))

	// DNS SAN must verify.
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "proxysql.default.svc", Roots: pool}); err != nil {
		t.Errorf("chain verification with DNSName failed: %v", err)
	}

	// IP SAN must be present and match.
	wantIP := net.ParseIP("10.0.0.5")
	ipFound := false
	for _, ip := range leaf.IPAddresses {
		if ip.Equal(wantIP) {
			ipFound = true
		}
	}
	if !ipFound {
		t.Errorf("serving cert IPAddresses = %v, want to contain %v", leaf.IPAddresses, wantIP)
	}

	// A hostname not in the SAN list must fail verification.
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "not-a-san.example.com", Roots: pool}); err == nil {
		t.Errorf("chain verification succeeded for a hostname not in the SAN list, want error")
	}

	// Verification against an unrelated CA pool must fail (proves the
	// chain is actually anchored to the supplied CA, not accepted by
	// accident).
	otherCACertPEM, _, err := NewCA("other-ca", 24*time.Hour)
	if err != nil {
		t.Fatalf("NewCA() error = %v", err)
	}
	otherPool := x509.NewCertPool()
	otherPool.AddCert(mustParseCert(t, otherCACertPEM))
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "proxysql.default.svc", Roots: otherPool}); err == nil {
		t.Errorf("chain verification succeeded against unrelated CA pool, want error")
	}
}

func TestNeedsRenewal(t *testing.T) {
	const renewBefore = 24 * time.Hour

	t.Run("expires within renewBefore window", func(t *testing.T) {
		certPEM, _, err := NewCA("test-ca", renewBefore-time.Hour)
		if err != nil {
			t.Fatalf("NewCA() error = %v", err)
		}
		if !NeedsRenewal(certPEM, renewBefore) {
			t.Errorf("NeedsRenewal() = false, want true (cert expires before renewBefore window)")
		}
	})

	t.Run("expires after renewBefore window", func(t *testing.T) {
		certPEM, _, err := NewCA("test-ca", renewBefore+time.Hour)
		if err != nil {
			t.Fatalf("NewCA() error = %v", err)
		}
		if NeedsRenewal(certPEM, renewBefore) {
			t.Errorf("NeedsRenewal() = true, want false (cert expires after renewBefore window)")
		}
	})

	t.Run("garbage PEM counts as needing renewal", func(t *testing.T) {
		if !NeedsRenewal([]byte("not a certificate"), time.Hour) {
			t.Errorf("NeedsRenewal() = false for unparseable PEM, want true")
		}
	})

	t.Run("empty input counts as needing renewal", func(t *testing.T) {
		if !NeedsRenewal(nil, time.Hour) {
			t.Errorf("NeedsRenewal() = false for nil input, want true")
		}
	})
}

func TestLeafFingerprint(t *testing.T) {
	certPEM1, _, err := NewCA("test-ca", 24*time.Hour)
	if err != nil {
		t.Fatalf("NewCA() error = %v", err)
	}
	certPEM2, _, err := NewCA("other-ca", 24*time.Hour)
	if err != nil {
		t.Fatalf("NewCA() error = %v", err)
	}

	fp1a, err := LeafFingerprint(certPEM1)
	if err != nil {
		t.Fatalf("LeafFingerprint() error = %v", err)
	}
	if fp1a == "" {
		t.Fatalf("LeafFingerprint() returned empty string")
	}

	fp1b, err := LeafFingerprint(certPEM1)
	if err != nil {
		t.Fatalf("LeafFingerprint() error = %v", err)
	}
	if fp1a != fp1b {
		t.Errorf("LeafFingerprint() not stable: %q != %q", fp1a, fp1b)
	}

	fp2, err := LeafFingerprint(certPEM2)
	if err != nil {
		t.Fatalf("LeafFingerprint() error = %v", err)
	}
	if fp1a == fp2 {
		t.Errorf("LeafFingerprint() for different certs collided: %q", fp1a)
	}

	if _, err := LeafFingerprint([]byte("garbage")); err == nil {
		t.Errorf("LeafFingerprint() on garbage PEM: want error, got nil")
	}
}

func TestSANsFor(t *testing.T) {
	got := SANsFor("mycluster", "default", []string{"custom.example.com", "10.0.0.5"})
	want := []string{
		"mycluster",
		"mycluster.default",
		"mycluster.default.svc",
		"*.mycluster-headless.default.svc",
		"mycluster-external",
		"custom.example.com",
		"10.0.0.5",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SANsFor() = %#v, want %#v", got, want)
	}
}

func TestSANsFor_NoExtras(t *testing.T) {
	got := SANsFor("mycluster", "ns1", nil)
	want := []string{
		"mycluster",
		"mycluster.ns1",
		"mycluster.ns1.svc",
		"*.mycluster-headless.ns1.svc",
		"mycluster-external",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SANsFor() = %#v, want %#v", got, want)
	}
}

// TestSANsFor_FeedsIssueServing exercises the documented behavior that IP
// entries produced by SANsFor (or passed as extras) become IP SANs on the
// issued certificate, while everything else (including the wildcard) is a
// DNS SAN.
func TestSANsFor_FeedsIssueServing(t *testing.T) {
	caCertPEM, caKeyPEM, err := NewCA("test-ca", 24*time.Hour)
	if err != nil {
		t.Fatalf("NewCA() error = %v", err)
	}

	sans := SANsFor("mycluster", "default", []string{"10.1.2.3"})
	servingCertPEM, _, err := IssueServing(caCertPEM, caKeyPEM, sans, time.Hour)
	if err != nil {
		t.Fatalf("IssueServing() error = %v", err)
	}
	leaf := mustParseCert(t, servingCertPEM)

	wantDNS := []string{
		"mycluster",
		"mycluster.default",
		"mycluster.default.svc",
		"*.mycluster-headless.default.svc",
		"mycluster-external",
	}
	if !reflect.DeepEqual(leaf.DNSNames, wantDNS) {
		t.Errorf("leaf.DNSNames = %#v, want %#v", leaf.DNSNames, wantDNS)
	}
	if len(leaf.IPAddresses) != 1 || !leaf.IPAddresses[0].Equal(net.ParseIP("10.1.2.3")) {
		t.Errorf("leaf.IPAddresses = %v, want [10.1.2.3]", leaf.IPAddresses)
	}

	pool := x509.NewCertPool()
	pool.AddCert(mustParseCert(t, caCertPEM))
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "mycluster.default.svc", Roots: pool}); err != nil {
		t.Errorf("chain verification failed: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "pod-0.mycluster-headless.default.svc", Roots: pool}); err != nil {
		t.Errorf("wildcard chain verification failed: %v", err)
	}
}
