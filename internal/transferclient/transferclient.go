// Package transferclient builds the HTTP client used for a direct transfer to
// or from storage — a presigned upload or download — as opposed to the
// client used for calls to the Helix API itself.
//
// The policy is the same for every Helix SDK: a bounded connect phase, then
// an unbounded transfer for as long as it keeps moving bytes. Concretely, a
// connection attempt that takes longer than ConnectTimeout fails, and once
// connected, IdleTimeout bounds how long the connection may go without
// moving a single byte in either direction before it is cut — this covers
// waiting for response headers and both legs of the body transfer, upload
// and download alike. A transfer that keeps moving bytes, however slowly,
// has no total-duration cap: the client built here never sets
// http.Client.Timeout.
package transferclient

import (
	"context"
	"net"
	"net/http"
	"time"
)

// ConnectTimeout bounds how long establishing a connection to a storage
// endpoint (including the TLS handshake) may take. A package-level variable,
// not a constant, so tests can override it to run in milliseconds; it is not
// a public SDK setting.
var ConnectTimeout = 10 * time.Second

// IdleTimeout bounds how long a storage connection, once established, may go
// without moving a single byte in either direction before it is cut. Same
// override rationale as ConnectTimeout.
var IdleTimeout = 60 * time.Second

// New returns an *http.Client dedicated to one direction of a storage
// transfer. It never sets http.Client.Timeout: a healthy transfer that keeps
// moving bytes, however slowly, must not be cut by a total-duration cap.
// Instead, every connection it dials is wrapped so that each successful Read
// or Write pushes the connection's deadline IdleTimeout further out; a
// connection that goes that long without moving any bytes fails its next
// Read or Write with a timeout.
//
// This single mechanism is deliberately used for both legs: it catches a
// slow-to-respond peer (nothing read for IdleTimeout, including while still
// waiting for response headers) exactly as it catches a peer that stops
// draining an upload mid-transfer (the client's pending Write on the
// connection times out) — a response-headers-only timeout would miss the
// latter, since it would not even have started counting while the request
// was still being written.
//
// A caller's own context deadline or cancellation on the request is
// unaffected by any of this and still wins.
func New() *http.Client {
	connectTimeout := ConnectTimeout
	idleTimeout := IdleTimeout

	dialer := &net.Dialer{Timeout: connectTimeout}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if err := conn.SetDeadline(time.Now().Add(idleTimeout)); err != nil {
				_ = conn.Close()
				return nil, err
			}
			return &idleConn{Conn: conn, idleTimeout: idleTimeout}, nil
		},
		TLSHandshakeTimeout: connectTimeout,
		// One connection per transfer: a presigned URL is used once, and
		// disabling reuse keeps an idle deadline set mid-transfer from ever
		// being mistaken for one inherited from a previous request on a
		// pooled connection.
		DisableKeepAlives: true,
	}

	return &http.Client{Transport: transport}
}

// idleConn wraps a net.Conn so every successful Read or Write — the
// connection moving at least one byte in either direction — pushes the
// connection's deadline idleTimeout further out.
type idleConn struct {
	net.Conn
	idleTimeout time.Duration
}

func (c *idleConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		_ = c.Conn.SetDeadline(time.Now().Add(c.idleTimeout))
	}
	return n, err
}

func (c *idleConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		_ = c.Conn.SetDeadline(time.Now().Add(c.idleTimeout))
	}
	return n, err
}
