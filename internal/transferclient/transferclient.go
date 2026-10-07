// Package transferclient builds the HTTP client used for a direct transfer to
// or from storage — a presigned upload or download — as opposed to the
// client used for calls to the Helix API itself.
//
// The policy is the same for every Helix SDK: a bounded connect phase, then
// an unbounded transfer for as long as it keeps moving bytes. Concretely, a
// connection attempt that takes longer than ConnectTimeout fails — including
// every stage of connecting through a configured proxy, of any scheme this
// package understands, as a single combined budget rather than each stage
// getting its own — and once connected, IdleTimeout bounds how long the
// connection may go without moving a single byte in either direction before
// it is cut. This covers waiting for response headers and both legs of the
// body transfer, upload and download alike. A transfer that keeps moving
// bytes, however slowly, has no total-duration cap: the client built here
// never sets http.Client.Timeout.
//
// An upload has one further wrinkle: once the whole request body has been
// handed off to net/http, nothing the SDK can observe moves while the
// operating system's own send buffer drains and storage confirms receipt —
// IdleTimeout alone would wrongly cut a large, slow-but-healthy upload right
// there. A caller that wraps its request body with WrapUploadBody gets a
// longer, but FIXED, ResponseTimeout for exactly that wait instead: unlike
// IdleTimeout, it is not pushed further out by bytes arriving in the
// meantime, so a peer cannot keep an incomplete response's headers
// trickling in to extend the wait indefinitely. Once those headers are
// fully read — reported back via MarkResponseHeadersReceived — the
// connection returns to the normal IdleTimeout for whatever (typically
// small) response body follows. A caller that never wraps its body this
// way (every download, and any upload that skips it) keeps using
// IdleTimeout throughout, unchanged.
package transferclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
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
// wait for storage's response headers to arrive after the entire request
// body has been handed off to net/http — the gap IdleTimeout would
// otherwise (wrongly) bound, even though a healthy, large upload's
// OS-level send buffer can legitimately still be draining well past
// IdleTimeout. Unlike IdleTimeout, this window is FIXED from the moment the
// body finishes, not extended by bytes arriving in the meantime — see
// bodyDoneSignal's doc comment for why. Same override rationale as
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
// connection carrying it. markDone, called by the body the instant it is
// fully drained, immediately sets the connection's deadline to a FIXED
// point in time, now+responseTimeout — not just on the next successful
// Read or Write, since for a Content-Length body the last data-carrying
// Write to the connection already happened (with the old, shorter deadline)
// before the body's own Read ever reports io.EOF; waiting for a subsequent
// I/O to notice would leave the critical gap — the wait for storage's
// response — governed by whatever deadline that last Write set.
//
// That deadline is deliberately FIXED, not pushed forward by every
// subsequent successful read, for as long as storage's response headers
// remain incomplete: a peer could otherwise drip-feed one header byte every
// few seconds and extend the wait indefinitely, which is exactly the shape
// of a slow-but-healthy transfer this package otherwise exists to tolerate
// — but tolerating it here would mean no bound at all on how long a caller
// waits for a response to even begin arriving. Once MarkResponseHeadersReceived
// reports those headers are fully read, idleConn.extendDeadline goes back
// to extending the deadline by IdleTimeout on every successful read, exactly
// as it does for every read before the request body was ever handed off —
// the (typically small) response body that follows is bound the same way a
// download's body is, never held to the now-elapsed-or-elapsing fixed
// window that only ever governed the wait for headers.
//
// bind connects the signal to its connection once DialContext/DialTLSContext
// construct it (always before the body is read, since net/http dials before
// it writes), capturing the ResponseTimeout package variable at that same
// moment for markDone to use later — not read again at markDone's own,
// later call time. Only a caller that actually gets as far as dialing a
// connection through this package's own DialContext/DialTLSContext (that
// is, one using the *http.Client New returns) ever touches ResponseTimeout
// at all; one that supplies a different http.Client but still wraps its
// body with WrapUploadBody (as the self-attack and negative-control tests
// deliberately do, to compare against the pre-fix client shape) must not
// — reading it unconditionally from the body reader itself, regardless of
// which client is in play, raced one such test's leaked goroutine against
// a later test's override of the same package variable. A mutex guards all
// of this because the body is drained on one goroutine (net/http's request
// writer) while the connection may be read concurrently on another (its
// response reader).
type bodyDoneSignal struct {
	mu              sync.Mutex
	conn            *idleConn
	responseTimeout time.Duration
	// responseDeadline is the fixed point in time markDone computed for the
	// wait on response headers; meaningful only once done is true.
	responseDeadline time.Time
	headersDone      bool
	done             bool
}

func (s *bodyDoneSignal) bind(conn *idleConn, responseTimeout time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conn = conn
	s.responseTimeout = responseTimeout
}

// markDone is called by the wrapped body the instant it is fully drained.
// It fixes responseDeadline unconditionally (idleConn.extendDeadline reads
// it only once headersDone is false, so a stale value sitting unused is
// harmless), but only force-sets it on the connection when headers have not
// already arrived: a storage service that responds before the request body
// has finished sending (an early validation error, say) can call
// markHeadersReceived before this fires, and the connection's deadline is
// already correctly back on the normal IdleTimeout basis by then — forcing
// it down to the (likely much shorter) fixed window here would wrongly
// re-impose the header-wait bound after the headers it was ever meant to
// bound have already arrived.
func (s *bodyDoneSignal) markDone() {
	s.mu.Lock()
	s.done = true
	s.responseDeadline = time.Now().Add(s.responseTimeout)
	headersAlreadyDone := s.headersDone
	conn := s.conn
	deadline := s.responseDeadline
	s.mu.Unlock()
	if conn != nil && !headersAlreadyDone {
		_ = conn.SetDeadline(deadline)
	}
}

// markHeadersReceived records that storage's response status line and
// headers have fully arrived — see MarkResponseHeadersReceived, the
// exported function that calls this through a request's context — and
// immediately re-arms the connection's deadline to a fresh IdleTimeout
// window itself, rather than waiting for the next successful Read or Write
// to notice headersDone flipped. Without this, the connection would still
// be sitting at whatever point in time the fixed, non-extending
// header-wait deadline landed on — which a header wait that ran close to
// the full window would leave on the verge of expiring, or already
// expired, wrongly timing out a response body that is merely slow to
// start, not stalled.
func (s *bodyDoneSignal) markHeadersReceived() {
	s.mu.Lock()
	s.headersDone = true
	conn := s.conn
	s.mu.Unlock()
	if conn != nil {
		_ = conn.SetDeadline(time.Now().Add(conn.idleTimeout))
	}
}

// state reports whether the body has been fully handed off, the fixed
// deadline markDone computed for that case, and whether markHeadersReceived
// has since fired — the three pieces idleConn.extendDeadline needs to pick
// the right deadline on each Read or Write.
func (s *bodyDoneSignal) state() (done bool, deadline time.Time, headersDone bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done, s.responseDeadline, s.headersDone
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
// switches the connection's deadline from IdleTimeout to the longer, fixed
// ResponseTimeout for the wait that follows — bounding how long it waits for
// storage's response headers after the body is fully sent, without a still-
// draining OS send buffer tripping the shorter per-byte window first. Once
// the caller sees the response (see MarkResponseHeadersReceived), the
// connection returns to IdleTimeout for whatever follows. A request whose
// body is never wrapped this way is unaffected: its connection just keeps
// using IdleTimeout throughout, exactly as before this existed.
func WrapUploadBody(ctx context.Context, body io.Reader) (context.Context, io.Reader) {
	signal := &bodyDoneSignal{}
	wrapped := &uploadBodyReader{r: body, signal: signal}
	return context.WithValue(ctx, bodyDoneContextKey{}, signal), wrapped
}

// MarkResponseHeadersReceived tells the connection carrying ctx's upload
// body-completion signal (set by a prior WrapUploadBody call on this same
// ctx) that storage's response status line and headers have now been fully
// read. Call it the moment a client.Do or Transport.RoundTrip made with
// that ctx returns a non-nil *http.Response — that return is exactly the
// "headers complete" event — before reading resp.Body, if at all. From this
// point on, any further read on the connection (a response body, however
// small) is governed by IdleTimeout again, not held at the fixed window
// that bounded only the preceding wait for those headers to arrive (see
// bodyDoneSignal's doc comment). A no-op when ctx was never wrapped with
// WrapUploadBody, or when the request body was never actually fully handed
// off (for example, the request failed before that point) — in both cases
// there is no response-window deadline to release.
func MarkResponseHeadersReceived(ctx context.Context) {
	if signal := bodyDoneFromContext(ctx); signal != nil {
		signal.markHeadersReceived()
	}
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
			if req.URL != nil && req.URL.Scheme == "https" && (isForwardProxyScheme(proxyURL.Scheme) || isSOCKS5Scheme(proxyURL.Scheme)) {
				// Returning nil here tells net/http there is no proxy for
				// this request, which is what makes net/http hand the
				// connection to DialTLSContext below (with addr set to the
				// real target) instead of driving its own proxy path for
				// it — a CONNECT-then-TLS tunnel for an http(s):// proxy,
				// or its own built-in SOCKS5 client for a socks5(h):// one
				// — each of which times its own stages separately (the
				// CONNECT wait is a hardcoded net/http-internal 1-minute
				// cap, not ConnectTimeout; the SOCKS5 handshake rides
				// whatever deadline idleConn last set, and a target TLS
				// handshake after it gets a separate TLSHandshakeTimeout on
				// top) and so cannot be bounded as one combined budget.
				// DialTLSContext performs every stage itself, under one
				// shared ConnectTimeout, for either proxy shape — see
				// dialTLSThroughProxy and dialThroughSOCKS5Proxy.
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
			ic := &idleConn{Conn: conn, idleTimeout: idleTimeout, bodyDone: bodyDone}
			if bodyDone != nil {
				bodyDone.bind(ic, ResponseTimeout)
			}
			return ic, nil
		},
		// DialTLSContext drives every HTTPS connection for which the Proxy
		// func above returned nil: the common non-proxied case, and every
		// proxied case this package unifies under one ConnectTimeout
		// budget — an env proxy with an http://, https://, socks5://, or
		// socks5h:// scheme. Either way, every stage — dial, the proxy's
		// own TLS handshake or SOCKS5 handshake when it has one, the
		// CONNECT exchange when it has one, and the target TLS handshake —
		// runs under the SAME context deadline, so they share one
		// ConnectTimeout budget rather than each getting its own full
		// one — a 6s dial followed by a 6s handshake must fail a 10s
		// budget, not succeed at ~12s, and the same holds with a proxy
		// dial, its own handshake, and any wait in between added.
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			bodyDone := bodyDoneFromContext(ctx)

			dialCtx, cancel := context.WithTimeout(ctx, connectTimeout)
			defer cancel()

			proxyURL, err := proxyForRequest(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
			if err != nil {
				return nil, err
			}
			if proxyURL != nil {
				if isForwardProxyScheme(proxyURL.Scheme) {
					return dialTLSThroughProxy(dialCtx, network, addr, proxyURL, idleTimeout, bodyDone)
				}
				if isSOCKS5Scheme(proxyURL.Scheme) {
					return dialThroughSOCKS5Proxy(dialCtx, network, addr, proxyURL, idleTimeout, bodyDone)
				}
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
			ic := &idleConn{Conn: tlsConn, idleTimeout: idleTimeout, bodyDone: bodyDone}
			if bodyDone != nil {
				bodyDone.bind(ic, ResponseTimeout)
			}
			return ic, nil
		},
		// TLSClientConfig and TLSHandshakeTimeout are, in practice, never
		// reached: DialTLSContext above now takes over every HTTPS
		// connection this package ever makes, proxied through any scheme it
		// understands (http, https, socks5, socks5h) or not proxied at all.
		// They are kept set anyway as a defensive fallback for whatever
		// case that claim turns out to be wrong about — net/http only
		// takes the generic dial-then-handshake path these fields govern
		// when DialTLSContext is unset, which it never is here.
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

// isForwardProxyScheme reports whether scheme is an HTTP or HTTPS
// forward-proxy scheme this package unifies under one ConnectTimeout budget
// via DialTLSContext and dialTLSThroughProxy: a plain HTTP proxy, or an
// HTTPS proxy (the proxy connection itself is TLS, with the CONNECT exchange
// and the target TLS handshake running inside that tunnel). A socks5 or
// socks5h proxy is unified the same way but through a different dial
// function — see isSOCKS5Scheme and dialThroughSOCKS5Proxy — because
// reaching the target through it isn't an HTTP CONNECT exchange.
func isForwardProxyScheme(scheme string) bool {
	return scheme == "http" || scheme == "https"
}

// isSOCKS5Scheme reports whether scheme is a SOCKS5 forward-proxy scheme
// this package unifies under one ConnectTimeout budget via DialTLSContext
// and dialThroughSOCKS5Proxy. net/http treats socks5 and socks5h
// identically (resolving the target hostname at the proxy either way, per
// its own doc comment on Transport.Proxy), so this package does too.
func isSOCKS5Scheme(scheme string) bool {
	return scheme == "socks5" || scheme == "socks5h"
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
	ic := &idleConn{Conn: tlsConn, idleTimeout: idleTimeout, bodyDone: bodyDone}
	if bodyDone != nil {
		bodyDone.bind(ic, ResponseTimeout)
	}
	return ic, nil
}

// dialThroughSOCKS5Proxy establishes a storage TLS connection via a SOCKS5
// forward proxy (a socks5:// or socks5h:// env proxy URL): dialing the
// proxy, performing the SOCKS5 handshake to addr, and performing the TLS
// handshake to addr over the resulting tunnel — all under the ctx deadline
// the caller already set, exactly as dialTLSThroughProxy does for an HTTP or
// HTTPS forward proxy. This replaces relying on net/http's own built-in
// SOCKS5 support, which has no extension point to bound the SOCKS5
// handshake and the following TLS handshake under one shared budget:
// net/http dials the proxy under our own DialContext (already bound by
// ConnectTimeout) but then drives the SOCKS5 handshake under whatever
// deadline idleConn last set on that connection — IdleTimeout, not
// ConnectTimeout — and, for an HTTPS target, the TLS handshake that follows
// under a separate TLSHandshakeTimeout on top of that — letting the two
// together run far longer in total than ConnectTimeout permits for every
// other proxy shape this package handles. bodyDone, if non-nil, is wired
// into the returned connection exactly like every other dial path — see
// WrapUploadBody.
func dialThroughSOCKS5Proxy(ctx context.Context, network, addr string, proxyURL *url.URL, idleTimeout time.Duration, bodyDone *bodyDoneSignal) (net.Conn, error) {
	rawConn, err := dial(ctx, network, proxyHostPort(proxyURL, "1080"))
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := rawConn.SetDeadline(deadline); err != nil {
			_ = rawConn.Close()
			return nil, err
		}
	}

	if err := socks5Handshake(rawConn, addr, proxyURL.User); err != nil {
		_ = rawConn.Close()
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
	ic := &idleConn{Conn: tlsConn, idleTimeout: idleTimeout, bodyDone: bodyDone}
	if bodyDone != nil {
		bodyDone.bind(ic, ResponseTimeout)
	}
	return ic, nil
}

// socks5Handshake performs the client side of a SOCKS5 CONNECT handshake
// (RFC 1928, plus RFC 1929 username/password authentication when proxyUser
// carries credentials) over conn, asking the proxy to connect to addr. conn
// must already have whatever deadline governs the overall connect budget
// set on it by the caller (see dialThroughSOCKS5Proxy) — this function
// performs no I/O timing of its own, exactly like dialTLSThroughProxy's own
// CONNECT exchange. It mirrors net/http's own built-in SOCKS5 client wire
// behavior (golang.org/x/net/internal/socks, vendored into the Go standard
// library): the same address-type selection (IPv4 or IPv6 when host parses
// as an IP, a fully-qualified domain name otherwise, so a hostname is
// resolved by the proxy exactly as it is for both the socks5:// and
// socks5h:// schemes) and the same two supported authentication methods —
// switching from net/http's handling to this one changes only how the
// connect phase is timed, never the proxy wire protocol.
func socks5Handshake(conn net.Conn, addr string, proxyUser *url.Userinfo) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 0xffff {
		return fmt.Errorf("transferclient: socks5 target port %q out of range", portStr)
	}

	methods := []byte{0x00}
	if proxyUser != nil {
		methods = append(methods, 0x02)
	}
	greeting := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := conn.Write(greeting); err != nil {
		return err
	}

	selected := make([]byte, 2)
	if _, err := io.ReadFull(conn, selected); err != nil {
		return err
	}
	if selected[0] != 0x05 {
		return fmt.Errorf("transferclient: socks5 proxy: unexpected version %d", selected[0])
	}
	switch selected[1] {
	case 0x00:
		// No authentication required.
	case 0x02:
		if proxyUser == nil {
			return errors.New("transferclient: socks5 proxy requires username/password authentication")
		}
		password, _ := proxyUser.Password()
		if err := socks5Authenticate(conn, proxyUser.Username(), password); err != nil {
			return err
		}
	case 0xff:
		return errors.New("transferclient: socks5 proxy: no acceptable authentication method")
	default:
		return fmt.Errorf("transferclient: socks5 proxy: unsupported authentication method %d", selected[1])
	}

	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, 0x01)
			req = append(req, ip4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else if len(host) > 255 {
		return errors.New("transferclient: socks5 target hostname too long")
	} else {
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	req = append(req, byte(port>>8), byte(port))
	if _, err := conn.Write(req); err != nil {
		return err
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return err
	}
	if head[0] != 0x05 {
		return fmt.Errorf("transferclient: socks5 proxy: unexpected version %d", head[0])
	}
	if head[1] != 0x00 {
		return fmt.Errorf("transferclient: socks5 proxy: CONNECT failed with status %d", head[1])
	}

	var boundAddrLen int
	switch head[3] {
	case 0x01:
		boundAddrLen = net.IPv4len
	case 0x04:
		boundAddrLen = net.IPv6len
	case 0x03:
		lenByte := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenByte); err != nil {
			return err
		}
		boundAddrLen = int(lenByte[0])
	default:
		return fmt.Errorf("transferclient: socks5 proxy: unknown bound address type %d", head[3])
	}
	// The bound address and port are discarded; this package never needs
	// them (it always dials the single target address it was given).
	_, err = io.ReadFull(conn, make([]byte, boundAddrLen+2))
	return err
}

// socks5Authenticate performs the RFC 1929 username/password subnegotiation
// after the proxy has selected that method during socks5Handshake's initial
// greeting.
func socks5Authenticate(conn net.Conn, username, password string) error {
	if len(username) == 0 || len(username) > 255 || len(password) > 255 {
		return errors.New("transferclient: socks5 proxy: invalid username/password length")
	}
	req := make([]byte, 0, 3+len(username)+len(password))
	req = append(req, 0x01, byte(len(username)))
	req = append(req, username...)
	req = append(req, byte(len(password)))
	req = append(req, password...)
	if _, err := conn.Write(req); err != nil {
		return err
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return err
	}
	if reply[0] != 0x01 {
		return fmt.Errorf("transferclient: socks5 proxy: unexpected username/password auth version %d", reply[0])
	}
	if reply[1] != 0x00 {
		return errors.New("transferclient: socks5 proxy: username/password authentication failed")
	}
	return nil
}

// proxyHostPort returns proxyURL's host:port, defaulting the port to
// defaultPort (the standard port for proxyURL's scheme — "80" for an http://
// proxy, "443" for an https:// one, "1080" for a socks5:// or socks5h:// one)
// when proxyURL carries none.
func proxyHostPort(proxyURL *url.URL, defaultPort string) string {
	if proxyURL.Port() != "" {
		return proxyURL.Host
	}
	return net.JoinHostPort(proxyURL.Hostname(), defaultPort)
}

// idleConn wraps a net.Conn so every successful Read or Write — the
// connection moving at least one byte in either direction — pushes the
// connection's deadline IdleTimeout further out. Once bodyDone (nil unless
// the request's body was wrapped with WrapUploadBody) reports the whole
// request body handed off, extendDeadline instead holds the connection at
// the FIXED, non-extending deadline bodyDone's markDone set — a trickle of
// bytes while storage's response headers are still incomplete must not push
// that deadline any further out, or a peer drip-feeding header bytes could
// extend the wait forever (see bodyDoneSignal's doc comment). Once
// MarkResponseHeadersReceived reports those headers are fully read,
// extendDeadline goes back to the normal sliding IdleTimeout for whatever
// follows — a response body, however small — exactly as if bodyDone had
// never been set.
type idleConn struct {
	net.Conn
	idleTimeout time.Duration
	bodyDone    *bodyDoneSignal
}

// extendDeadline picks the deadline the next Read or Write's success should
// leave in place — see idleConn's doc comment for why the fixed,
// header-wait deadline is deliberately not recomputed here the way the
// normal IdleTimeout-based one is.
func (c *idleConn) extendDeadline() {
	if c.bodyDone != nil {
		if done, deadline, headersDone := c.bodyDone.state(); done && !headersDone {
			_ = c.SetDeadline(deadline)
			return
		}
	}
	_ = c.SetDeadline(time.Now().Add(c.idleTimeout))
}

func (c *idleConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.extendDeadline()
	}
	return n, err
}

func (c *idleConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.extendDeadline()
	}
	return n, err
}
