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
	"database/sql"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Client is a minimal database/sql wrapper bound to a single ProxySQL admin
// endpoint. Construct one per pod-write; close it when done.
type Client struct {
	db   *sql.DB
	addr string
}

// New opens a connection to the ProxySQL admin interface at addr (host:port)
// authenticating as user/pass. The connection pool is sized to 1 — admin
// writes are serial.
func New(addr, user, pass string) (*Client, error) {
	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = pass
	cfg.Net = "tcp"
	cfg.Addr = addr
	cfg.Timeout = 10 * time.Second
	cfg.ReadTimeout = 10 * time.Second
	cfg.WriteTimeout = 10 * time.Second
	// ProxySQL admin doesn't speak full MySQL handshake quirks; keep the
	// driver in its strictest mode and let errors surface verbatim.
	cfg.AllowNativePasswords = true

	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("open proxysql admin %s: %w", addr, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(30 * time.Second)

	return &Client{db: db, addr: addr}, nil
}

// Close releases the underlying connection pool.
func (c *Client) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.db.Close()
}

// Ping verifies the admin port is reachable and the credentials work.
func (c *Client) Ping(ctx context.Context) error {
	return c.db.PingContext(ctx)
}

// Exec runs a non-query statement, wrapping any error with the target addr.
func (c *Client) Exec(ctx context.Context, query string, args ...any) error {
	if _, err := c.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("%s: exec %q: %w", c.addr, snippet(query), err)
	}
	return nil
}

// snippet returns a short prefix of q for use in error messages — full
// queries can be hundreds of lines after batched inserts.
func snippet(q string) string {
	const max = 80
	if len(q) <= max {
		return q
	}
	return q[:max] + "..."
}
