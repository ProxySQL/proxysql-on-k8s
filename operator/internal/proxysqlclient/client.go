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

// Query runs a SELECT and returns all rows as strings. ProxySQL's admin
// interface returns everything as text anyway; string rows keep the caller
// trivial and the fake trivial-er.
func (c *Client) Query(ctx context.Context, query string) ([][]string, error) {
	rows, err := c.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("%s: query %q: %w", c.addr, snippet(query), err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]string
	for rows.Next() {
		raw := make([]sql.RawBytes, len(cols))
		ptrs := make([]any, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		rec := make([]string, len(cols))
		for i, v := range raw {
			rec[i] = string(v)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// snippet returns a short, redacted prefix of q for use in error messages —
// full queries can be hundreds of lines after batched inserts, and single-
// quoted literals frequently carry secrets (passwords pushed via
// UPDATE global_variables / INSERT INTO mysql_users, etc). Errors built from
// this are logged by controller-runtime and can surface in status
// conditions, so literals must never reach them verbatim.
func snippet(q string) string {
	const max = 80
	redacted := redactQuotedLiterals(q)
	if len(redacted) <= max {
		return redacted
	}
	return redacted[:max] + "..."
}

// redactQuotedLiterals replaces the contents of every single-quoted SQL
// string literal in q with "***", preserving the surrounding statement
// shape (verb, table, column names) for debuggability. It understands SQL's
// doubled-quote escape (two single quotes inside a literal mean one literal
// single quote, not the end of the literal) so it won't terminate a literal
// early and leak the remainder of a secret.
func redactQuotedLiterals(q string) string {
	var out []rune
	runes := []rune(q)
	inLiteral := false

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if !inLiteral {
			out = append(out, r)
			if r == '\'' {
				inLiteral = true
				out = append(out, '*', '*', '*')
			}
			continue
		}

		// Inside a literal: everything is masked already ("***" was emitted
		// when we entered). Look for the terminating quote, being careful
		// that a doubled '' is an escaped quote, not the end of the literal.
		if r == '\'' {
			if i+1 < len(runes) && runes[i+1] == '\'' {
				// Escaped quote inside the literal: consume both runes,
				// stay inside the literal, emit nothing more (already
				// masked).
				i++
				continue
			}
			// End of literal.
			inLiteral = false
			out = append(out, '\'')
		}
		// else: masked literal content, drop the original rune.
	}

	return string(out)
}
