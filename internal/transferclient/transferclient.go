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
//
// An upload has one further wrinkle: once the whole request body has been
// handed off to net/http, nothing the SDK can observe moves while the
// operating system's own send buffer drains and storage confirms receipt —
// IdleTimeout alone would wrongly cut a large, slow-but-healthy upload right
// there. A caller that wraps its request body with WrapUploadBody gets a
// longer ResponseTimeout for exactly that wait instead; a caller that
// doesn't (every download, and any upload that skips it) keeps using
// IdleTimeout throughout, unchanged.
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
	"sync"
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

// ResponseTimeout bounds how long an upload client (see WrapUploadBody) may
// wait for storage's response after the entire request body has been handed
// off to net/http — the gap IdleTimeout would otherwise (wrongly) bound,
// even though a healthy, large upload's OS-level send buffer can legitimately
// still be draining well past IdleTimeout. Same override rationale as
// ConnectTimeout. Unused by a client whose request body was never wrapped
// with WrapUploadBody — that connection uses IdleTimeout throughout.
var ResponseTimeout = 300 * time.Second

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

// bodyDoneContextKey is the context key a request's context carries a
// *bodyDoneSignal under, once that request's body has been wrapped with
// WrapUploadBody.
type bodyDoneContextKey struct{}

// bodyDoneSignal is the handoff between a wrapped request body and the
// connection carrying it: markDone, called by the body the instant it is
// fully drained, immediately pushes that connection's deadline out to
// ResponseTimeout — not just on the next successful Read or Write, since for
// a Content-Length body the last data-carrying Write to the connection
// already happened (with the old, shorter deadline) before the body's own
// Read ever reports io.EOF; waiting for a subsequent I/O to notice would
// leave the critical gap — the wait for storage's response — governed by
// whatever deadline that last Write set. bind connects the signal to its
// connection once DialContext/DialTLSContext construct it (always before the
// body is read, since net/http dials before it writes). A mutex guards all
// of this because the body is drained on one goroutine (net/http's request
// writer) while the connection may be read concurrently on another (its
// response reader).
type bodyDoneSignal struct {
	mu              sync.Mutex
	conn            net.Conn
	responseTimeout time.Duration
	done            bool
}

func (s *bodyDoneSignal) bind(conn net.Conn, responseTimeout time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conn = conn
	s.responseTimeout = responseTimeout
}

func (s *bodyDoneSignal) markDone() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = true
	if s.conn != nil {
		_ = s.conn.SetDeadline(time.Now().Add(s.responseTimeout))
	}
}

func (s *bodyDoneSignal) isDone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// bodyDoneFromContext returns the *bodyDoneSignal a prior WrapUploadBody
// call attached to ctx, or nil if the request's body was never wrapped —
// the common case for every download and any upload that doesn't opt in,
// for which the returned nil means "use IdleTimeout throughout," unchanged.
func bodyDoneFromContext(ctx context.Context) *bodyDoneSignal {
	signal, _ := ctx.Value(bodyDoneContextKey{}).(*bodyDoneSignal)
	return signal
}

// WrapUploadBody returns ctx carrying a body-completion signal, paired with
// body wrapped so that signal fires the instant body.Read reports io.EOF —
// the point at which net/http has read the entire request body and will
// make no further Read calls on it. Build the request with
// http.NewRequestWithContext using the returned ctx and body
// (http.NewRequestWithContext(ctx, method, url, body)): the *http.Client New
// returns reads the signal off that request's context and, once it fires,
// switches the connection's deadline from IdleTimeout to the longer
// ResponseTimeout for the wait that follows — bounding how long it waits for
// storage's response after the body is fully sent, without a still-draining
// OS send buffer tripping the shorter per-byte window first. A request whose
// body is never wrapped this way is unaffected: its connection just keeps
// using IdleTimeout throughout, exactly as before this existed.
func WrapUploadBody(ctx context.Context, body io.Reader) (context.Context, io.Reader) {
	signal := &bodyDoneSignal{}
	wrapped := &uploadBodyReader{r: body, signal: signal}
	return context.WithValue(ctx, bodyDoneContextKey{}, signal), wrapped
}

// uploadBodyReader marks its bodyDoneSignal the moment the wrapped reader
// reports io.EOF.
type uploadBodyReader struct {
	r      io.Reader
	signal *bodyDoneSignal
}

func (b *uploadBodyReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err == io.EOF {
		b.signal.markDone()
	}
	return n, err
}

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
			if req.URL != nil && req.URL.Scheme == "https" && isForwardProxyScheme(proxyURL.Scheme) {
				// Returning nil here tells net/http there is no proxy for
				// this request, which is what makes net/http hand the
				// connection to DialTLSContext below (with addr set to the
				// real target) instead of dialing the proxy itself and
				// driving its own CONNECT-then-TLS tunnel — a path whose
				// dial, the proxy's own TLS handshake when it has one,
				// CONNECT-response wait, and the target TLS handshake are
				// each timed separately (the CONNECT wait is a hardcoded
				// net/http-internal 1-minute cap, not ConnectTimeout) and so
				// cannot be bounded as one combined budget. DialTLSContext
				// performs every one of those stages itself, under one
				// shared ConnectTimeout. A socks5/socks5h proxy URL is
				// deliberately excluded: net/http dials it as a plain TCP
				// connection (not TLS) and performs its own handshake
				// afterward on that connection regardless of what Proxy
				// returns here, so there is no CONNECT-tunnel path to
				// bypass — see dialTLSThroughProxy's doc comment for how
				// that case is still bounded.
				return nil, nil
			}
			return proxyURL, err
		},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			bodyDone := bodyDoneFromContext(ctx)

			dialCtx, cancel := context.WithTimeout(ctx, connectTimeout)
			defer cancel()

			conn, err := dial(dialCtx, network, addr)
			if err != nil {
				return nil, err
			}
			if err := conn.SetDeadline(time.Now().Add(idleTimeout)); err != nil {
				_ = conn.Close()
				return nil, err
			}
			ic := &idleConn{Conn: conn, idleTimeout: idleTimeout, bodyDone: bodyDone, responseTimeout: ResponseTimeout}
			if bodyDone != nil {
				bodyDone.bind(ic, ResponseTimeout)
			}
			return ic, nil
		},
		// DialTLSContext drives every HTTPS connection for which the Proxy
		// func above returned nil: the common non-proxied case, and the
		// common HTTP- or HTTPS-proxy case (an env proxy with an http:// or
		// https:// scheme — Proxy returns nil for exactly that combination
		// so net/http hands control here instead of driving its own
		// CONNECT tunnel). Either way, every stage — dial, the proxy's own
		// TLS handshake when it has one, the CONNECT exchange, and the
		// target TLS handshake — runs under the SAME context deadline, so
		// they share one ConnectTimeout budget rather than each getting its
		// own full one — a 6s dial followed by a 6s handshake must fail a
		// 10s budget, not succeed at ~12s, and the same holds with a proxy
		// dial, its own TLS handshake, and a CONNECT wait added in between.
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			bodyDone := bodyDoneFromContext(ctx)

			dialCtx, cancel := context.WithTimeout(ctx, connectTimeout)
			defer cancel()

			proxyURL, err := proxyForRequest(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
			if err != nil {
				return nil, err
			}
			if proxyURL != nil && isForwardProxyScheme(proxyURL.Scheme) {
				return dialTLSThroughProxy(dialCtx, network, addr, proxyURL, idleTimeout, bodyDone)
			}

			rawConn, err := dial(dialCtx, network, addr)
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
			if err := tlsConn.HandshakeContext(dialCtx); err != nil {
				_ = rawConn.Close()
				return nil, err
			}
			if err := tlsConn.SetDeadline(time.Now().Add(idleTimeout)); err != nil {
				_ = tlsConn.Close()
				return nil, err
			}
			ic := &idleConn{Conn: tlsConn, idleTimeout: idleTimeout, bodyDone: bodyDone, responseTimeout: ResponseTimeout}
			if bodyDone != nil {
				bodyDone.bind(ic, ResponseTimeout)
			}
			return ic, nil
		},
		// TLSClientConfig and TLSHandshakeTimeout are now only reached for
		// the one case DialTLSContext above does not take over: an env
		// proxy with a socks5:// or socks5h:// scheme. net/http dials that
		// proxy as a plain TCP connection via our own DialContext — so the
		// dial itself is already bound by ConnectTimeout exactly like every
		// other case — then performs the SOCKS5 handshake and, for an https
		// target, the target TLS handshake itself, outside any hook this
		// package controls. That handshake pair is not unified into the
		// same cumulative ConnectTimeout budget the http/https-proxy and
		// direct paths now share: the SOCKS5 handshake rides whatever
		// deadline idleConn last set on the connection (IdleTimeout, once
		// the preceding dial succeeds — see DialContext), and the following
		// target TLS handshake, if any, falls back to TLSHandshakeTimeout
		// here. Neither is unbounded, so a SOCKS5 proxy can never hang a
		// transfer forever; it just doesn't get the same single combined
		// connect-phase guarantee. Unifying it would require intercepting
		// net/http's internal SOCKS5 dial, which has no extension point in
		// http.Transport.
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

// isForwardProxyScheme reports whether scheme is a forward-proxy scheme this
// package unifies under one ConnectTimeout budget via DialTLSContext and
// dialTLSThroughProxy: a plain HTTP proxy, or an HTTPS proxy (the proxy
// connection itself is TLS, with the CONNECT exchange and the target TLS
// handshake running inside that tunnel). It deliberately excludes
// socks5/socks5h — see the TLSClientConfig/TLSHandshakeTimeout doc comment
// in New for why that scheme is left on net/http's own path.
func isForwardProxyScheme(scheme string) bool {
	return scheme == "http" || scheme == "https"
}

// dialTLSThroughProxy establishes a storage TLS connection via an HTTP or
// HTTPS forward proxy: dialing the proxy (TLS-handshaking with the proxy
// itself first when proxyURL's scheme is https), performing the CONNECT
// exchange to addr over that connection, and performing the TLS handshake to
// addr over the resulting tunnel — all under the ctx deadline the caller
// already set. Running every stage under that one caller-supplied deadline,
// rather than letting each stage time itself the way net/http's own
// CONNECT-tunnel code does, is what makes a dial+[proxy handshake+]CONNECT+
// handshake sequence that exceeds ConnectTimeout fail at ConnectTimeout
// instead of at the sum of each stage's own best-effort bound. bodyDone, if
// non-nil, is wired into the returned connection exactly like DialContext
// and DialTLSContext wire it directly — see WrapUploadBody.
func dialTLSThroughProxy(ctx context.Context, network, addr string, proxyURL *url.URL, idleTimeout time.Duration, bodyDone *bodyDoneSignal) (net.Conn, error) {
	defaultPort := "80"
	if proxyURL.Scheme == "https" {
		defaultPort = "443"
	}
	rawConn, err := dial(ctx, network, proxyHostPort(proxyURL, defaultPort))
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := rawConn.SetDeadline(deadline); err != nil {
			_ = rawConn.Close()
			return nil, err
		}
	}

	// conn is the connection the CONNECT request/response travel over: the
	// raw TCP connection for an HTTP proxy, or a TLS connection to the proxy
	// itself for an HTTPS proxy — the proxy's own identity is verified here
	// exactly like the target's is below, just against the proxy's
	// hostname instead.
	conn := rawConn
	if proxyURL.Scheme == "https" {
		proxyCfg := tlsConfig.Clone()
		proxyCfg.ServerName = proxyURL.Hostname()
		proxyTLSConn := tls.Client(rawConn, proxyCfg)
		if err := proxyTLSConn.HandshakeContext(ctx); err != nil {
			_ = rawConn.Close()
			return nil, err
		}
		conn = proxyTLSConn
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
	ic := &idleConn{Conn: tlsConn, idleTimeout: idleTimeout, bodyDone: bodyDone, responseTimeout: ResponseTimeout}
	if bodyDone != nil {
		bodyDone.bind(ic, ResponseTimeout)
	}
	return ic, nil
}

// proxyHostPort returns proxyURL's host:port, defaulting the port to
// defaultPort (the standard port for proxyURL's scheme — "80" for an http://
// proxy, "443" for an https:// one) when proxyURL carries none.
func proxyHostPort(proxyURL *url.URL, defaultPort string) string {
	if proxyURL.Port() != "" {
		return proxyURL.Host
	}
	return net.JoinHostPort(proxyURL.Hostname(), defaultPort)
}

// idleConn wraps a net.Conn so every successful Read or Write — the
// connection moving at least one byte in either direction — pushes the
// connection's deadline further out: by idleTimeout normally, or by
// responseTimeout once bodyDone (nil unless the request's body was wrapped
// with WrapUploadBody) reports the whole request body handed off. See
// bodyDoneSignal's doc comment for why markDone itself, not just this
// extension-on-success path, also pushes the deadline out directly — this
// path alone would miss the one gap the whole mechanism exists to cover.
type idleConn struct {
	net.Conn
	idleTimeout     time.Duration
	bodyDone        *bodyDoneSignal
	responseTimeout time.Duration
}

func (c *idleConn) nextTimeout() time.Duration {
	if c.bodyDone != nil && c.bodyDone.isDone() {
		return c.responseTimeout
	}
	return c.idleTimeout
}

func (c *idleConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		_ = c.Conn.SetDeadline(time.Now().Add(c.nextTimeout()))
	}
	return n, err
}

func (c *idleConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		_ = c.Conn.SetDeadline(time.Now().Add(c.nextTimeout()))
	}
	return n, err
}
