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
	"crypto/tls"
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

// dial establishes the TCP connection for a storage transfer. A
// package-level variable — not inlined — so a test can wrap it with an
// artificial delay and prove that the connect budget (the TCP dial and, for
// TLS, the handshake that follows it) shares a single ConnectTimeout rather
// than each getting its own: a real TCP dial over loopback completes too
// fast for a test to produce that timing any other way. Not a public SDK
// setting.
var dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

// tlsConfig is cloned as the base configuration for every storage TLS
// connection (only ServerName is then set, per connection). A package-level
// variable so a test can supply trust roots for a local stub TLS server; not
// a public SDK setting.
var tlsConfig = &tls.Config{}

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
// Connecting also still honors a proxy configured via HTTP_PROXY/HTTPS_PROXY/
// NO_PROXY, exactly like the client this one replaced — a customer behind a
// corporate proxy is not cut off from storage transfers specifically.
//
// A caller's own context deadline or cancellation on the request is
// unaffected by any of this and still wins.
func New() *http.Client {
	connectTimeout := ConnectTimeout
	idleTimeout := IdleTimeout

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, connectTimeout)
			defer cancel()

			conn, err := dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if err := conn.SetDeadline(time.Now().Add(idleTimeout)); err != nil {
				_ = conn.Close()
				return nil, err
			}
			return &idleConn{Conn: conn, idleTimeout: idleTimeout}, nil
		},
		// DialTLSContext drives every non-proxied HTTPS connection (the
		// common case for a presigned storage URL). It dials and performs
		// the TLS handshake under the SAME context deadline, so the two
		// phases share one ConnectTimeout budget rather than each getting
		// its own full one — a 6s dial followed by a 6s handshake must fail
		// a 10s budget, not succeed at ~12s.
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, connectTimeout)
			defer cancel()

			rawConn, err := dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}

			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
			}
			cfg := tlsConfig.Clone()
			cfg.ServerName = host

			tlsConn := tls.Client(rawConn, cfg)
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				_ = rawConn.Close()
				return nil, err
			}
			if err := tlsConn.SetDeadline(time.Now().Add(idleTimeout)); err != nil {
				_ = tlsConn.Close()
				return nil, err
			}
			return &idleConn{Conn: tlsConn, idleTimeout: idleTimeout}, nil
		},
		// TLSClientConfig and TLSHandshakeTimeout are used only when a
		// request goes through an HTTP(S) proxy: Go tunnels via DialContext
		// (a CONNECT request) and then performs its own TLS handshake over
		// that tunnel, bypassing DialTLSContext entirely — DialTLSContext
		// only drives non-proxied TLS connections. Kept as a best-effort
		// bound for that path; the combined dial+handshake budget above is
		// the common, non-proxied case.
		TLSClientConfig:     tlsConfig.Clone(),
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
