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
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// maxProxyConnectResponseBytes caps how much of a proxy's CONNECT response
// headers dialTLSThroughProxy will read, mirroring net/http's own default
// response-header cap (its unexported Transport.maxHeaderResponseSize) so a
// misbehaving or malicious proxy can't hold a goroutine reading an unbounded
// header block.
const maxProxyConnectResponseBytes = 10 << 20

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

// proxyForRequest resolves which proxy, if any, a request should use,
// including NO_PROXY handling — the same resolution http.ProxyFromEnvironment
// performs. A package-level variable, not inlined, so a test can substitute
// a fixed proxy URL directly: net/http caches the parsed proxy environment
// for the lifetime of the process the first time ANY Transport's Proxy
// function runs, so a test that instead set HTTP_PROXY/HTTPS_PROXY/NO_PROXY
// would pass or fail depending on test execution order within the binary.
// Not a public SDK setting.
var proxyForRequest = http.ProxyFromEnvironment

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
		Proxy: func(req *http.Request) (*url.URL, error) {
			proxyURL, err := proxyForRequest(req)
			if err != nil || proxyURL == nil {
				return proxyURL, err
			}
			if req.URL != nil && req.URL.Scheme == "https" && proxyURL.Scheme == "http" {
				// Returning nil here tells net/http there is no proxy for
				// this request, which is what makes net/http hand the
				// connection to DialTLSContext below (with addr set to the
				// real target) instead of dialing the proxy itself and
				// driving its own CONNECT-then-TLS tunnel — a path whose
				// dial, CONNECT-response wait, and TLS handshake are each
				// timed separately (the CONNECT wait is a hardcoded
				// net/http-internal 1-minute cap, not ConnectTimeout) and so
				// cannot be bounded as one combined budget. DialTLSContext
				// performs the same proxy dial, CONNECT exchange, and TLS
				// handshake itself, under one shared ConnectTimeout.
				return nil, nil
			}
			return proxyURL, err
		},
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
		// DialTLSContext drives every HTTPS connection for which the Proxy
		// func above returned nil: the common non-proxied case, and the
		// common HTTP-proxy case (an env proxy with an http:// scheme —
		// Proxy returns nil for exactly that combination so net/http hands
		// control here instead of driving its own CONNECT tunnel). Either
		// way, every stage — dial, the proxy CONNECT exchange when one
		// applies, and the TLS handshake — runs under the SAME context
		// deadline, so they share one ConnectTimeout budget rather than
		// each getting its own full one — a 6s dial followed by a 6s
		// handshake must fail a 10s budget, not succeed at ~12s, and the
		// same holds with a proxy dial and CONNECT wait added in between.
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, connectTimeout)
			defer cancel()

			proxyURL, err := proxyForRequest(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
			if err != nil {
				return nil, err
			}
			if proxyURL != nil && proxyURL.Scheme == "http" {
				return dialTLSThroughProxy(ctx, network, addr, proxyURL, idleTimeout)
			}

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
		// TLSClientConfig and TLSHandshakeTimeout are now only reached for
		// the cases DialTLSContext above does not take over: an env proxy
		// with a non-http scheme (an https:// or socks5:// proxy URL), which
		// stay on net/http's own CONNECT-then-TLS path exactly as before
		// this change. Kept as a best-effort bound for those rarer paths;
		// the shared budget in DialTLSContext is the common case (direct,
		// and HTTP-proxied HTTPS).
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

// dialTLSThroughProxy establishes a storage TLS connection via an HTTP
// forward proxy: dialing the proxy, performing the CONNECT exchange to
// addr, and performing the TLS handshake to addr over the resulting tunnel
// — all under the ctx deadline the caller already set. Running the three
// stages under that one caller-supplied deadline, rather than letting each
// stage time itself the way net/http's own CONNECT-tunnel code does, is
// what makes a dial+CONNECT+handshake sequence that exceeds ConnectTimeout
// fail at ConnectTimeout instead of at the sum of each stage's own
// best-effort bound.
func dialTLSThroughProxy(ctx context.Context, network, addr string, proxyURL *url.URL, idleTimeout time.Duration) (net.Conn, error) {
	conn, err := dial(ctx, network, proxyHostPort(proxyURL))
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}

	connectReq := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if u := proxyURL.User; u != nil {
		password, _ := u.Password()
		connectReq.Header.Set("Proxy-Authorization",
			"Basic "+base64.StdEncoding.EncodeToString([]byte(u.Username()+":"+password)))
	}
	if err := connectReq.Write(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}

	resp, err := http.ReadResponse(bufio.NewReader(io.LimitReader(conn, maxProxyConnectResponseBytes)), connectReq)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("transferclient: proxy CONNECT %s: %s", addr, resp.Status)
	}

	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	cfg := tlsConfig.Clone()
	cfg.ServerName = host

	tlsConn := tls.Client(conn, cfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := tlsConn.SetDeadline(time.Now().Add(idleTimeout)); err != nil {
		_ = tlsConn.Close()
		return nil, err
	}
	return &idleConn{Conn: tlsConn, idleTimeout: idleTimeout}, nil
}

// proxyHostPort returns proxyURL's host:port, defaulting the port to 80 —
// the standard port for the http:// proxy URLs this is called for — when
// proxyURL carries none.
func proxyHostPort(proxyURL *url.URL) string {
	if proxyURL.Port() != "" {
		return proxyURL.Host
	}
	return net.JoinHostPort(proxyURL.Hostname(), "80")
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
