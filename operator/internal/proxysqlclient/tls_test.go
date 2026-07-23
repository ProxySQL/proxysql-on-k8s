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

package proxysqlclient

import (
	"context"
	"crypto/tls"
	"testing"
)

// TestReloadTLS_IssuesExactStatement pins the SQL the rotation engine sends:
// PROXYSQL RELOAD TLS, nothing more (no LOAD/SAVE cycle — the statement is
// self-contained in ProxySQL).
func TestReloadTLS_IssuesExactStatement(t *testing.T) {
	rec := &recorder{}
	if err := ReloadTLS(context.Background(), rec); err != nil {
		t.Fatalf("ReloadTLS: %v", err)
	}
	if len(rec.queries) != 1 {
		t.Fatalf("queries = %v, want exactly one", rec.queries)
	}
	if rec.queries[0] != "PROXYSQL RELOAD TLS" {
		t.Errorf("query = %q, want %q", rec.queries[0], "PROXYSQL RELOAD TLS")
	}
}

// TestNewWithTLS_NilConfigDialsPlaintext pins the compatibility contract:
// a nil tls.Config must produce a client identical to New's — no TLS config
// registered with the driver (tlsKey empty), so TLS-off clusters dial
// exactly as they did before this option existed.
func TestNewWithTLS_NilConfigDialsPlaintext(t *testing.T) {
	c, err := NewWithTLS("127.0.0.1:16032", "radmin", "pw", nil)
	if err != nil {
		t.Fatalf("NewWithTLS(nil): %v", err)
	}
	defer func() { _ = c.Close() }()
	if c.tlsKey != "" {
		t.Errorf("tlsKey = %q, want empty (no driver TLS config registered for nil)", c.tlsKey)
	}
}

// TestNew_DelegatesToNewWithTLS: New must behave exactly like
// NewWithTLS(..., nil).
func TestNew_DelegatesToNewWithTLS(t *testing.T) {
	c, err := New("127.0.0.1:16032", "radmin", "pw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = c.Close() }()
	if c.tlsKey != "" {
		t.Errorf("tlsKey = %q, want empty", c.tlsKey)
	}
}

// TestNewWithTLS_UniqueKeysPerDial: every TLS dial must register its
// tls.Config under a UNIQUE driver key. The go-sql-driver TLS-config
// registry is process-global; a fixed key (e.g. the cluster name, or worse
// a constant) would let concurrent dials to different clusters clobber each
// other's trust anchors.
func TestNewWithTLS_UniqueKeysPerDial(t *testing.T) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}

	// Same address on purpose: uniqueness must not depend on the addr.
	a, err := NewWithTLS("127.0.0.1:16032", "radmin", "pw", cfg)
	if err != nil {
		t.Fatalf("NewWithTLS a: %v", err)
	}
	defer func() { _ = a.Close() }()
	b, err := NewWithTLS("127.0.0.1:16032", "radmin", "pw", cfg)
	if err != nil {
		t.Fatalf("NewWithTLS b: %v", err)
	}
	defer func() { _ = b.Close() }()

	if a.tlsKey == "" || b.tlsKey == "" {
		t.Fatalf("tlsKey empty (a=%q, b=%q); a non-nil config must register", a.tlsKey, b.tlsKey)
	}
	if a.tlsKey == b.tlsKey {
		t.Errorf("two dials share driver TLS key %q; keys must be unique per dial", a.tlsKey)
	}
}

// TestClient_CloseTwiceAfterTLS: Close must stay idempotent with a
// registered TLS config (it also deregisters the driver key).
func TestClient_CloseTwiceAfterTLS(t *testing.T) {
	c, err := NewWithTLS("127.0.0.1:16032", "radmin", "pw", &tls.Config{MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("NewWithTLS: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
