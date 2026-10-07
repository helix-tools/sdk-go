package transferclient

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// stub is a raw TCP server on 127.0.0.1, used instead of httptest.Server so a
// test controls exactly when bytes are written (or withheld) on the wire —
// httptest/http.Server would read and buffer a request before a handler ever
// runs, hiding the timing this package exists to bound.
type stub struct {
	ln    net.Listener
	addr  string
	mu    sync.Mutex
	conns []net.Conn
}

// newStub starts a listener and runs handle in its own goroutine for every
// accepted connection. Every accepted connection is force-closed on test
// cleanup, so a handler that intentionally never closes its end (to hold a
// connection open and silent) cannot leak past the test.
func newStub(t *testing.T, handle func(net.Conn)) *stub {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	s := &stub{ln: ln, addr: ln.Addr().String()}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, conn)
			s.mu.Unlock()
			go handle(conn)
		}
	}()

	t.Cleanup(func() {
		_ = ln.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, c := range s.conns {
			_ = c.Close()
		}
	})

	return s
}

func (s *stub) url(path string) string {
	return "http://" + s.addr + path
}

// overrideIdleTimeout sets IdleTimeout for the duration of one test and
// restores it on cleanup — the internal seam this package exposes so a test
// runs in milliseconds instead of the real 60s default.
func overrideIdleTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := IdleTimeout
	IdleTimeout = d
	t.Cleanup(func() { IdleTimeout = orig })
}

func overrideConnectTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := ConnectTimeout
	ConnectTimeout = d
	t.Cleanup(func() { ConnectTimeout = orig })
}

// overrideResponseTimeout sets ResponseTimeout for the duration of one test
// and restores it on cleanup.
func overrideResponseTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := ResponseTimeout
	ResponseTimeout = d
	t.Cleanup(func() { ResponseTimeout = orig })
}

// overrideDial swaps the package's dial function so a test can inject a
// deterministic connect-phase delay — real TCP dialing over loopback
// completes in well under a millisecond, too fast to prove anything about a
// shared dial+handshake budget without this seam.
func overrideDial(t *testing.T, fn func(ctx context.Context, network, addr string) (net.Conn, error)) {
	t.Helper()
	orig := dial
	dial = fn
	t.Cleanup(func() { dial = orig })
}

// overrideTLSConfig swaps the base TLS config used for every storage TLS
// connection so a test can supply trust roots for a local, self-signed stub
// server.
func overrideTLSConfig(t *testing.T, cfg *tls.Config) {
	t.Helper()
	orig := tlsConfig
	tlsConfig = cfg
	t.Cleanup(func() { tlsConfig = orig })
}

// overrideProxyForRequest swaps the package's proxy-resolution function so a
// test can point every request at a fixed proxy URL, never by setting
// HTTP_PROXY/HTTPS_PROXY/NO_PROXY — see TestNew_PreservesEnvironmentProxy for
// why a real env-var-based proxy test would depend on execution order within
// the binary.
func overrideProxyForRequest(t *testing.T, fn func(*http.Request) (*url.URL, error)) {
	t.Helper()
	orig := proxyForRequest
	proxyForRequest = fn
	t.Cleanup(func() { proxyForRequest = orig })
}

// readUntilBlankLine reads directly from conn, one byte at a time, until it
// has seen the "\r\n\r\n" that ends an HTTP header block, then returns with
// conn's read position exactly at the first byte after it. A bufio.Reader
// would risk buffering ahead past that point and swallowing bytes that
// belong to whatever comes next on the same connection (here, the client's
// TLS ClientHello) — a plain net.Conn has no way to hand back over-read
// bytes once buffered.
func readUntilBlankLine(conn net.Conn) error {
	var last4 [4]byte
	buf := make([]byte, 1)
	for {
		if _, err := conn.Read(buf); err != nil {
			return err
		}
		last4[0], last4[1], last4[2], last4[3] = last4[1], last4[2], last4[3], buf[0]
		if last4 == [4]byte{'\r', '\n', '\r', '\n'} {
			return nil
		}
	}
}

// newProxyTLSStub starts a stub that speaks the server side of an HTTP
// forward proxy's CONNECT tunnel for exactly one connection: read the
// CONNECT request, wait connectDelay, reply 200, wait handshakeDelay, then
// hand the still-raw tunneled connection to handle to drive the TLS
// handshake — the same contract as newTLSStub, but through a CONNECT tunnel
// first, the shape a request through a configured HTTP proxy actually takes
// on the wire.
func newProxyTLSStub(t *testing.T, connectDelay, handshakeDelay time.Duration, handle func(net.Conn, tls.Certificate)) *stub {
	t.Helper()

	cert := newSelfSignedCert(t)
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	overrideTLSConfig(t, &tls.Config{RootCAs: pool})

	return newStub(t, func(conn net.Conn) {
		if err := readUntilBlankLine(conn); err != nil {
			return
		}
		time.Sleep(connectDelay)
		if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			return
		}
		time.Sleep(handshakeDelay)
		handle(conn, cert)
	})
}

// newHTTPSProxyTLSStub starts a stub that speaks the server side of an
// HTTPS forward proxy for exactly one connection: perform the outer TLS
// handshake (the connection to the proxy itself), wait proxyHandshakeDelay,
// read the CONNECT request sent over that tunnel, wait connectDelay, reply
// 200 over the same tunnel, wait targetHandshakeDelay, then hand the
// still-open tunnel to handle to drive the INNER TLS handshake to the
// simulated target — the "double TLS" shape a request through a configured
// HTTPS forward proxy actually takes on the wire. Proxy and target share one
// self-signed certificate here because both resolve to this same stub
// address in these tests (a real deployment would use two different
// certificates, for two different hostnames); that distinction isn't needed
// to prove the combined budget unifies all four stages.
func newHTTPSProxyTLSStub(t *testing.T, proxyHandshakeDelay, connectDelay, targetHandshakeDelay time.Duration, handle func(net.Conn, tls.Certificate)) *stub {
	t.Helper()

	cert := newSelfSignedCert(t)
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	overrideTLSConfig(t, &tls.Config{RootCAs: pool})

	return newStub(t, func(conn net.Conn) {
		time.Sleep(proxyHandshakeDelay)
		outer := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
		if err := outer.Handshake(); err != nil {
			return
		}
		if err := readUntilBlankLine(outer); err != nil {
			return
		}
		time.Sleep(connectDelay)
		if _, err := outer.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			return
		}
		time.Sleep(targetHandshakeDelay)
		handle(outer, cert)
	})
}

// newSelfSignedCert generates an ephemeral certificate valid for 127.0.0.1,
// used by newTLSStub so TLS tests verify against a real trust root instead
// of disabling certificate verification.
func newSelfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "transferclient test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	cert.Leaf, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return cert
}

// newTLSStub starts a stub like newStub, but hands the accepted connection
// to handle BEFORE any TLS handshake happens — the handler drives (or
// delays) the server side of the handshake itself, so a test controls its
// timing exactly. It also points overrideTLSConfig's trust root at the
// generated certificate for the duration of the test.
func newTLSStub(t *testing.T, handle func(net.Conn, tls.Certificate)) *stub {
	t.Helper()

	cert := newSelfSignedCert(t)

	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	overrideTLSConfig(t, &tls.Config{RootCAs: pool})

	return newStub(t, func(conn net.Conn) { handle(conn, cert) })
}

func (s *stub) tlsURL(path string) string {
	return "https://" + s.addr + path
}

// TestNew_NoClientLevelTimeout pins the core guarantee this package exists
// to provide: nothing bounds total transfer duration. If a future change
// reintroduces http.Client.Timeout here, a healthy slow transfer would fail
// again exactly like the bug this package fixes.
func TestNew_NoClientLevelTimeout(t *testing.T) {
	c := New()
	if c.Timeout != 0 {
		t.Fatalf("New().Timeout = %v, want 0 (no total-duration cap)", c.Timeout)
	}
}

// TestNew_ConnectTimeoutWiredToTransport pins that ConnectTimeout reaches
// http.Transport.TLSHandshakeTimeout — the field Go falls back to only for
// an HTTPS connection through a non-http-scheme proxy (an https:// or
// socks5:// proxy URL). DialTLSContext drives every other case, including
// the common HTTP-proxy one, and its own combined budget is exercised
// behaviorally by TestNew_TLSConnectBudget_SharedAcrossDialAndHandshake and
// TestNew_ProxiedTLS_ConnectBudget_SharedAcrossDialConnectAndHandshake, not
// by inspecting a field.
func TestNew_ConnectTimeoutWiredToTransport(t *testing.T) {
	overrideConnectTimeout(t, 7*time.Second)

	c := New()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", c.Transport)
	}
	if tr.TLSHandshakeTimeout != 7*time.Second {
		t.Fatalf("TLSHandshakeTimeout = %v, want %v", tr.TLSHandshakeTimeout, 7*time.Second)
	}
}

// TestNew_IdleTimeout_NoReplyAfterAccept is the self-attack's "drop at the
// header-wait stage": the connection succeeds but the peer never answers.
// The client's own request write succeeds (it is tiny and fits the kernel
// send buffer even though nothing on the other end ever reads it), so this
// pins that waiting for response headers is itself bounded by IdleTimeout.
func TestNew_IdleTimeout_NoReplyAfterAccept(t *testing.T) {
	overrideIdleTimeout(t, 100*time.Millisecond)

	s := newStub(t, func(conn net.Conn) {
		// Intentionally never read or write anything; the connection is
		// held open (and force-closed by the stub's own cleanup).
		_ = conn
	})

	client := New()
	req, err := http.NewRequest(http.MethodGet, s.url("/object"), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected an error from a peer that never answers")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("took %v to fail; a 100ms idle timeout should have cut it well before this", elapsed)
	}
}

// TestNew_IdleTimeout_StallMidBody is the self-attack's "stall mid-body" for
// a download: response headers arrive promptly, declaring more body than the
// peer ever sends, and then nothing further ever arrives.
func TestNew_IdleTimeout_StallMidBody(t *testing.T) {
	overrideIdleTimeout(t, 100*time.Millisecond)

	s := newStub(t, func(conn net.Conn) {
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf) // drain the request line/headers
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 1000000\r\n\r\n"))
		// Then stall: the declared body never arrives.
	})

	client := New()
	req, err := http.NewRequest(http.MethodGet, s.url("/object"), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	buf := make([]byte, 16)
	_, err = resp.Body.Read(buf)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected reading the stalled body to fail")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("took %v to fail; a 100ms idle timeout should have cut it well before this", elapsed)
	}
}

// TestNew_SlowButHealthyTransfer_Succeeds is the policy's other half: a
// transfer that keeps moving bytes, however slowly, has no total-duration
// cap. Ten bytes trickle in, each gap well under IdleTimeout, but the whole
// transfer runs for several multiples of IdleTimeout — the exact shape of
// the reported bug (a healthy download killed by a total cap).
func TestNew_SlowButHealthyTransfer_Succeeds(t *testing.T) {
	overrideIdleTimeout(t, 150*time.Millisecond)

	const (
		gap     = 40 * time.Millisecond
		nBytes  = 10
		payload = "0123456789"
	)

	s := newStub(t, func(conn net.Conn) {
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\n"))
		for i := 0; i < nBytes; i++ {
			time.Sleep(gap)
			_, _ = conn.Write([]byte{payload[i]})
		}
	})

	client := New()
	req, err := http.NewRequest(http.MethodGet, s.url("/object"), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	got := make([]byte, 0, nBytes)
	buf := make([]byte, 1)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			got = append(got, buf[:n]...)
		}
		if rerr != nil {
			break
		}
	}
	elapsed := time.Since(start)

	if string(got) != payload {
		t.Fatalf("got body %q, want %q", got, payload)
	}
	// The whole transfer (nBytes*gap ~= 400ms) ran for well over two
	// IdleTimeout windows (150ms each) without being cut — proof there is no
	// total-duration cap, only a per-gap one.
	if elapsed < 2*150*time.Millisecond {
		t.Fatalf("transfer finished in %v; expected it to span multiple idle-timeout windows to be a meaningful proof", elapsed)
	}
}

// TestIdleConn_WriteTimesOutWhenPeerNeverReads is the self-attack's "stall
// mid-body" case for an upload's write side: the peer accepts the
// connection but never reads anything, so the client's Write eventually
// blocks waiting for the peer to drain its receive buffer. net.Pipe gives a
// fully synchronous, unbuffered connection — a Write on one end always
// blocks until a Read happens on the other — so this blocks deterministically
// on every platform without depending on kernel socket buffer sizes, unlike
// a real TCP socket where the exact payload size needed to force a block
// varies by OS and environment.
//
// This is the scenario a response-headers-only timeout (Go's
// http.Transport.ResponseHeaderTimeout) cannot catch: that timer starts
// only once the request has been FULLY written, so a stall while still
// writing the body would never even start it. idleConn's write-side deadline
// is what catches it instead.
func TestIdleConn_WriteTimesOutWhenPeerNeverReads(t *testing.T) {
	client, _ := net.Pipe()
	defer func() { _ = client.Close() }()

	wrapped := &idleConn{Conn: client, idleTimeout: 80 * time.Millisecond}
	if err := wrapped.SetDeadline(time.Now().Add(wrapped.idleTimeout)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	start := time.Now()
	_, err := wrapped.Write([]byte("this write has no reader on the other end"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the write to time out with no reader ever draining the pipe")
	}
	netErr, ok := err.(net.Error)
	if !ok || !netErr.Timeout() {
		t.Fatalf("err = %v (%T), want a net.Error reporting Timeout() == true", err, err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %v to time out; an 80ms idle timeout should have cut it well before this", elapsed)
	}
}

// TestNew_ConcurrentTransfers_IndependentIdleTimers is the self-attack's
// concurrency case: a stalled transfer and a healthy slow one running at the
// same time through clients built by this package must not affect each
// other — each connection's deadline is its own.
func TestNew_ConcurrentTransfers_IndependentIdleTimers(t *testing.T) {
	overrideIdleTimeout(t, 100*time.Millisecond)

	stalled := newStub(t, func(conn net.Conn) {
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 1000000\r\n\r\n"))
	})
	healthy := newStub(t, func(conn net.Conn) {
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\n"))
		for _, b := range []byte("abc") {
			time.Sleep(30 * time.Millisecond)
			_, _ = conn.Write([]byte{b})
		}
	})

	var wg sync.WaitGroup
	results := make(chan struct {
		name string
		err  error
		body string
	}, 2)

	wg.Add(2)
	go func() {
		defer wg.Done()
		client := New()
		req, _ := http.NewRequest(http.MethodGet, stalled.url("/object"), nil)
		resp, err := client.Do(req)
		if err == nil {
			buf := make([]byte, 16)
			_, rerr := resp.Body.Read(buf)
			_ = resp.Body.Close()
			results <- struct {
				name string
				err  error
				body string
			}{"stalled", rerr, ""}
			return
		}
		results <- struct {
			name string
			err  error
			body string
		}{"stalled", err, ""}
	}()
	go func() {
		defer wg.Done()
		client := New()
		req, _ := http.NewRequest(http.MethodGet, healthy.url("/object"), nil)
		resp, err := client.Do(req)
		if err != nil {
			results <- struct {
				name string
				err  error
				body string
			}{"healthy", err, ""}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		buf := make([]byte, 3)
		n, _ := io.ReadFull(resp.Body, buf)
		results <- struct {
			name string
			err  error
			body string
		}{"healthy", nil, string(buf[:n])}
	}()

	wg.Wait()
	close(results)

	for r := range results {
		switch r.name {
		case "stalled":
			if r.err == nil {
				t.Error("stalled transfer: expected a timeout error, got none")
			}
		case "healthy":
			if r.err != nil {
				t.Errorf("healthy transfer: expected success, got %v", r.err)
			}
			if r.body != "abc" {
				t.Errorf("healthy transfer: body = %q, want %q", r.body, "abc")
			}
		}
	}
}

// TestProxyForRequest_DefaultsToHTTPProxyFromEnvironment pins that the
// package's own proxy-resolution seam defaults to the real
// http.ProxyFromEnvironment in production — the actual mechanism by which a
// customer's HTTP_PROXY/HTTPS_PROXY/NO_PROXY still governs a storage
// transfer, exactly like the http.Client this package replaced (which
// inherited http.DefaultTransport's default Proxy). New()'s Transport.Proxy
// field is no longer this function directly (see
// TestNew_ProxyFunc_DelegatesByScheme below): it wraps it to hand the
// https-target-behind-an-http-proxy combination to DialTLSContext instead,
// so identity has to be checked one level down, against the seam itself.
//
// This checks identity against the exact function net/http uses for its own
// default transport, rather than setting the environment and making a
// request through a stub proxy: net/http caches the parsed proxy
// environment for the lifetime of the process the first time ANY
// Transport's Proxy function actually runs (the unexported envProxyOnce in
// net/http), and several other tests in this file already call client.Do
// before this one would run — a behavioral test here would pass or fail
// depending on test execution order within this binary, not on this
// package's own code.
func TestProxyForRequest_DefaultsToHTTPProxyFromEnvironment(t *testing.T) {
	got := reflect.ValueOf(proxyForRequest).Pointer()
	want := reflect.ValueOf(http.ProxyFromEnvironment).Pointer()
	if got != want {
		t.Fatal("proxyForRequest is not http.ProxyFromEnvironment by default — a storage transfer would bypass a customer's configured proxy entirely")
	}
}

// TestNew_ProxyFunc_DelegatesByScheme exercises New()'s Transport.Proxy
// wrapper across every combination of target scheme and resolved-proxy
// scheme it has to tell apart. It must return nil for an HTTPS target behind
// an http://, https://, socks5://, or socks5h:// proxy — handing every one
// of those combinations to DialTLSContext's single shared ConnectTimeout
// budget (see dialTLSThroughProxy and dialThroughSOCKS5Proxy) — and
// otherwise must pass whatever proxyForRequest resolved straight through
// unchanged, preserving NO_PROXY/HTTP_PROXY/HTTPS_PROXY semantics for every
// other combination exactly like the bare http.ProxyFromEnvironment this
// replaces. A plain HTTP target behind a socks5 proxy is deliberately left
// passing through unchanged: it has no TLS handshake stage for this
// package to need to unify, so net/http's own built-in SOCKS5 handling
// (reached via the unmodified DialContext path, which is already bound by
// ConnectTimeout for the dial itself) is left in place for it, exactly as
// review round 4 found it.
func TestNew_ProxyFunc_DelegatesByScheme(t *testing.T) {
	httpProxy := &url.URL{Scheme: "http", Host: "proxy.example:3128"}
	httpsProxy := &url.URL{Scheme: "https", Host: "proxy.example:3129"}
	socks5Proxy := &url.URL{Scheme: "socks5", Host: "proxy.example:1080"}
	socks5hProxy := &url.URL{Scheme: "socks5h", Host: "proxy.example:1080"}
	resolveErr := errors.New("boom")

	tests := []struct {
		name          string
		targetScheme  string
		resolvedProxy *url.URL
		resolvedErr   error
		wantNilProxy  bool
	}{
		{"no proxy configured, https target", "https", nil, nil, true},
		{"no proxy configured, http target", "http", nil, nil, true},
		{"https target behind http proxy delegates to DialTLSContext", "https", httpProxy, nil, true},
		{"http target behind http proxy passes through unchanged", "http", httpProxy, nil, false},
		{"https target behind https proxy delegates to DialTLSContext", "https", httpsProxy, nil, true},
		{"http target behind https proxy passes through unchanged", "http", httpsProxy, nil, false},
		{"https target behind socks5 proxy delegates to DialTLSContext", "https", socks5Proxy, nil, true},
		{"https target behind socks5h proxy delegates to DialTLSContext", "https", socks5hProxy, nil, true},
		{"http target behind socks5 proxy passes through unchanged", "http", socks5Proxy, nil, false},
		{"resolution error propagates", "https", nil, resolveErr, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			overrideProxyForRequest(t, func(*http.Request) (*url.URL, error) {
				return tc.resolvedProxy, tc.resolvedErr
			})

			c := New()
			tr, ok := c.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("Transport is %T, want *http.Transport", c.Transport)
			}
			req := &http.Request{URL: &url.URL{Scheme: tc.targetScheme, Host: "storage.example:443"}}

			got, err := tr.Proxy(req)

			if tc.resolvedErr != nil {
				if !errors.Is(err, tc.resolvedErr) {
					t.Fatalf("err = %v, want %v", err, tc.resolvedErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if tc.wantNilProxy {
				if got != nil {
					t.Fatalf("Proxy(...) = %v, want nil (net/http should fall through to DialTLSContext)", got)
				}
				return
			}
			if got != tc.resolvedProxy {
				t.Fatalf("Proxy(...) = %v, want %v unchanged", got, tc.resolvedProxy)
			}
		})
	}
}

// TestNew_TLS_HappyPath_Succeeds is the baseline sanity check that
// DialTLSContext's manual dial-then-handshake path actually serves a normal
// HTTPS storage transfer — none of the other tests in this file exercise
// TLS at all.
func TestNew_TLS_HappyPath_Succeeds(t *testing.T) {
	s := newTLSStub(t, func(conn net.Conn, cert tls.Certificate) {
		tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		defer func() { _ = tlsConn.Close() }()
		buf := make([]byte, 4096)
		_, _ = tlsConn.Read(buf)
		_, _ = tlsConn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
	})

	client := New()
	req, err := http.NewRequest(http.MethodGet, s.tlsURL("/object"), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want %q", body, "ok")
	}
}

// slowDial wraps the real dialer with a fixed delay before it ever attempts
// the TCP connection — the deterministic stand-in for a slow dial phase that
// loopback TCP cannot produce on its own (see the dial doc comment in
// transferclient.go).
func slowDial(delay time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
}

// TestNew_TLSConnectBudget_SucceedsWithinBudget is the companion sanity
// check to TestNew_TLSConnectBudget_SharedAcrossDialAndHandshake: a dial
// delay plus a handshake delay that together stay under ConnectTimeout must
// still succeed, proving the combined budget didn't just make every TLS
// connect fail.
func TestNew_TLSConnectBudget_SucceedsWithinBudget(t *testing.T) {
	const (
		connectTimeout = 400 * time.Millisecond
		dialDelay      = 50 * time.Millisecond
		handshakeDelay = 50 * time.Millisecond
	)
	overrideConnectTimeout(t, connectTimeout)
	overrideDial(t, slowDial(dialDelay))

	s := newTLSStub(t, func(conn net.Conn, cert tls.Certificate) {
		time.Sleep(handshakeDelay)
		tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		defer func() { _ = tlsConn.Close() }()
		buf := make([]byte, 4096)
		_, _ = tlsConn.Read(buf)
		_, _ = tlsConn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
	})

	client := New()
	req, err := http.NewRequest(http.MethodGet, s.tlsURL("/object"), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v (dial %v + handshake %v is well within the %v combined budget)", err, dialDelay, handshakeDelay, connectTimeout)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want %q", body, "ok")
	}
}

// TestNew_TLSConnectBudget_SharedAcrossDialAndHandshake is the behavioral
// proof that the dial phase and the TLS handshake phase share ONE
// ConnectTimeout budget rather than each getting its own full one. A dial
// delay and a handshake delay that are each individually well under
// ConnectTimeout, but whose sum exceeds it, must still fail — and must fail
// close to ConnectTimeout, not close to dialDelay+handshakeDelay (which is
// what a separate-budget bug would let through).
func TestNew_TLSConnectBudget_SharedAcrossDialAndHandshake(t *testing.T) {
	const (
		connectTimeout = 300 * time.Millisecond
		dialDelay      = 200 * time.Millisecond
		handshakeDelay = 200 * time.Millisecond
	)
	overrideConnectTimeout(t, connectTimeout)
	overrideDial(t, slowDial(dialDelay))

	s := newTLSStub(t, func(conn net.Conn, cert tls.Certificate) {
		time.Sleep(handshakeDelay)
		tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
		_ = tlsConn.Handshake()
	})

	client := New()
	req, err := http.NewRequest(http.MethodGet, s.tlsURL("/object"), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("expected the connect phase to fail: dial (%v) + handshake (%v) together exceed the %v combined budget", dialDelay, handshakeDelay, connectTimeout)
	}
	// A dial-only or handshake-only budget (each getting its own full
	// ConnectTimeout) would let this succeed at roughly dialDelay+handshakeDelay
	// (~400ms). A single shared budget must cut it off close to ConnectTimeout
	// (~300ms) instead.
	if elapsed > connectTimeout+200*time.Millisecond {
		t.Fatalf("took %v to fail; a %v combined connect budget should have cut it well before dial+handshake's %v", elapsed, connectTimeout, dialDelay+handshakeDelay)
	}
}

// TestNew_ProxiedTLS_SucceedsWithinBudget is the proxied-path companion to
// TestNew_TLSConnectBudget_SucceedsWithinBudget: a proxy dial, a CONNECT
// reply delay, and a TLS handshake delay that together stay under
// ConnectTimeout must still succeed through an env-configured HTTP proxy —
// proving the shared budget didn't just make every proxied TLS connect fail.
func TestNew_ProxiedTLS_SucceedsWithinBudget(t *testing.T) {
	const (
		connectTimeout = 600 * time.Millisecond
		dialDelay      = 50 * time.Millisecond
		connectDelay   = 50 * time.Millisecond
		handshakeDelay = 50 * time.Millisecond
	)
	overrideConnectTimeout(t, connectTimeout)
	overrideDial(t, slowDial(dialDelay))

	s := newProxyTLSStub(t, connectDelay, handshakeDelay, func(conn net.Conn, cert tls.Certificate) {
		tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		defer func() { _ = tlsConn.Close() }()
		buf := make([]byte, 4096)
		_, _ = tlsConn.Read(buf)
		_, _ = tlsConn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
	})
	overrideProxyForRequest(t, func(*http.Request) (*url.URL, error) {
		return &url.URL{Scheme: "http", Host: s.addr}, nil
	})

	client := New()
	req, err := http.NewRequest(http.MethodGet, s.tlsURL("/object"), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v (dial %v + CONNECT reply %v + handshake %v is well within the %v combined budget)",
			err, dialDelay, connectDelay, handshakeDelay, connectTimeout)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want %q", body, "ok")
	}
}

// TestNew_ProxiedTLS_ConnectBudget_SharedAcrossDialConnectAndHandshake is the
// proxied-path companion to TestNew_TLSConnectBudget_SharedAcrossDialAndHandshake:
// with an HTTP proxy configured, the proxy dial, the CONNECT response wait,
// and the TLS handshake must share ONE ConnectTimeout budget. Each of the
// three stages here is individually well under ConnectTimeout, but their sum
// is not — a client that times the dial, the CONNECT wait, and the handshake
// separately (the CONNECT wait, left to net/http's own built-in handling,
// rides its hardcoded 1-minute cap rather than ConnectTimeout) would let
// this succeed at roughly their sum; a single shared budget must cut it off
// close to ConnectTimeout instead.
func TestNew_ProxiedTLS_ConnectBudget_SharedAcrossDialConnectAndHandshake(t *testing.T) {
	const (
		connectTimeout = 300 * time.Millisecond
		dialDelay      = 150 * time.Millisecond
		connectDelay   = 150 * time.Millisecond
		handshakeDelay = 150 * time.Millisecond
	)
	overrideConnectTimeout(t, connectTimeout)
	overrideDial(t, slowDial(dialDelay))

	s := newProxyTLSStub(t, connectDelay, handshakeDelay, func(conn net.Conn, cert tls.Certificate) {
		tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
		_ = tlsConn.Handshake()
	})
	overrideProxyForRequest(t, func(*http.Request) (*url.URL, error) {
		return &url.URL{Scheme: "http", Host: s.addr}, nil
	})

	client := New()
	req, err := http.NewRequest(http.MethodGet, s.tlsURL("/object"), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("expected the proxied connect phase to fail: dial (%v) + CONNECT reply (%v) + handshake (%v) together exceed the %v combined budget",
			dialDelay, connectDelay, handshakeDelay, connectTimeout)
	}
	// Separately-timed stages would let this succeed at roughly
	// dialDelay+connectDelay+handshakeDelay (~450ms) or later. A single
	// shared budget must cut it off close to ConnectTimeout (~300ms).
	if elapsed > connectTimeout+200*time.Millisecond {
		t.Fatalf("took %v to fail; a %v combined proxied-connect budget should have cut it well before dial+CONNECT+handshake's %v",
			elapsed, connectTimeout, dialDelay+connectDelay+handshakeDelay)
	}
}

// TestNew_ProxiedHTTPSProxy_SucceedsWithinBudget is the HTTPS-forward-proxy
// companion to TestNew_ProxiedTLS_SucceedsWithinBudget: a dial, the proxy's
// own TLS handshake, a CONNECT reply delay, and the target TLS handshake
// that together stay under ConnectTimeout must still succeed through an
// env-configured HTTPS proxy — proving the shared budget didn't just make
// every HTTPS-proxied TLS connect fail.
func TestNew_ProxiedHTTPSProxy_SucceedsWithinBudget(t *testing.T) {
	const (
		connectTimeout       = 800 * time.Millisecond
		dialDelay            = 50 * time.Millisecond
		proxyHandshakeDelay  = 50 * time.Millisecond
		connectDelay         = 50 * time.Millisecond
		targetHandshakeDelay = 50 * time.Millisecond
	)
	overrideConnectTimeout(t, connectTimeout)
	overrideDial(t, slowDial(dialDelay))

	s := newHTTPSProxyTLSStub(t, proxyHandshakeDelay, connectDelay, targetHandshakeDelay, func(conn net.Conn, cert tls.Certificate) {
		tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		defer func() { _ = tlsConn.Close() }()
		buf := make([]byte, 4096)
		_, _ = tlsConn.Read(buf)
		_, _ = tlsConn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
	})
	overrideProxyForRequest(t, func(*http.Request) (*url.URL, error) {
		return &url.URL{Scheme: "https", Host: s.addr}, nil
	})

	client := New()
	req, err := http.NewRequest(http.MethodGet, s.tlsURL("/object"), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v (dial %v + proxy handshake %v + CONNECT reply %v + target handshake %v is well within the %v combined budget)",
			err, dialDelay, proxyHandshakeDelay, connectDelay, targetHandshakeDelay, connectTimeout)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want %q", body, "ok")
	}
}

// TestNew_ProxiedHTTPSProxy_ConnectBudget_SharedAcrossAllStages is the
// HTTPS-forward-proxy companion to
// TestNew_ProxiedTLS_ConnectBudget_SharedAcrossDialConnectAndHandshake: with
// an HTTPS proxy configured, the proxy dial, the proxy's own TLS handshake,
// the CONNECT response wait, and the target TLS handshake must share ONE
// ConnectTimeout budget. Each of the four stages here is individually well
// under ConnectTimeout, but their sum is not — a client that times any stage
// separately (as net/http's own built-in HTTPS-proxy handling does: the
// CONNECT wait alone rides its hardcoded 1-minute cap rather than
// ConnectTimeout) would let this succeed at roughly their sum; a single
// shared budget must cut it off close to ConnectTimeout instead. This test
// is what review round 3 flagged as missing: the pre-fix code bypassed the
// shared budget entirely for an https:// proxy (TestNew_ProxyFunc_Delegates
// ByScheme's "https target behind https proxy passes through unchanged"
// case), so this scenario used to succeed at roughly the stages' sum instead
// of failing near ConnectTimeout.
func TestNew_ProxiedHTTPSProxy_ConnectBudget_SharedAcrossAllStages(t *testing.T) {
	const (
		connectTimeout       = 300 * time.Millisecond
		dialDelay            = 100 * time.Millisecond
		proxyHandshakeDelay  = 100 * time.Millisecond
		connectDelay         = 100 * time.Millisecond
		targetHandshakeDelay = 100 * time.Millisecond
	)
	overrideConnectTimeout(t, connectTimeout)
	overrideDial(t, slowDial(dialDelay))

	s := newHTTPSProxyTLSStub(t, proxyHandshakeDelay, connectDelay, targetHandshakeDelay, func(conn net.Conn, cert tls.Certificate) {
		tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
		_ = tlsConn.Handshake()
	})
	overrideProxyForRequest(t, func(*http.Request) (*url.URL, error) {
		return &url.URL{Scheme: "https", Host: s.addr}, nil
	})

	client := New()
	req, err := http.NewRequest(http.MethodGet, s.tlsURL("/object"), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("expected the HTTPS-proxied connect phase to fail: dial (%v) + proxy handshake (%v) + CONNECT reply (%v) + target handshake (%v) together exceed the %v combined budget",
			dialDelay, proxyHandshakeDelay, connectDelay, targetHandshakeDelay, connectTimeout)
	}
	// Separately-timed stages would let this succeed at roughly the four
	// stages' sum (~400ms) or later (and net/http's own CONNECT-wait cap is
	// a full minute). A single shared budget must cut it off close to
	// ConnectTimeout (~300ms).
	if elapsed > connectTimeout+200*time.Millisecond {
		t.Fatalf("took %v to fail; a %v combined HTTPS-proxied-connect budget should have cut it well before the stages' sum of %v",
			elapsed, connectTimeout, dialDelay+proxyHandshakeDelay+connectDelay+targetHandshakeDelay)
	}
}

// newSOCKS5TLSStub starts a stub that speaks the server side of a SOCKS5
// proxy handshake for exactly one connection: read the client's method
// greeting and reply "no authentication required", wait handshakeDelay,
// read the CONNECT request, wait connectDelay, reply with success, then
// hand the still-raw tunneled connection to handle to drive the TLS
// handshake — the SOCKS5 analogue of newProxyTLSStub's HTTP CONNECT tunnel.
func newSOCKS5TLSStub(t *testing.T, handshakeDelay, connectDelay time.Duration, handle func(net.Conn, tls.Certificate)) *stub {
	t.Helper()

	cert := newSelfSignedCert(t)
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	overrideTLSConfig(t, &tls.Config{RootCAs: pool})

	return newStub(t, func(conn net.Conn) {
		greeting := make([]byte, 2)
		if _, err := io.ReadFull(conn, greeting); err != nil {
			return
		}
		methods := make([]byte, greeting[1])
		if _, err := io.ReadFull(conn, methods); err != nil {
			return
		}
		if _, err := conn.Write([]byte{0x05, 0x00}); err != nil { // version 5, no auth required
			return
		}
		time.Sleep(handshakeDelay)

		head := make([]byte, 4)
		if _, err := io.ReadFull(conn, head); err != nil {
			return
		}
		var addrLen int
		switch head[3] {
		case 0x01:
			addrLen = net.IPv4len
		case 0x04:
			addrLen = net.IPv6len
		case 0x03:
			lenByte := make([]byte, 1)
			if _, err := io.ReadFull(conn, lenByte); err != nil {
				return
			}
			addrLen = int(lenByte[0])
		default:
			return
		}
		if _, err := io.ReadFull(conn, make([]byte, addrLen+2)); err != nil {
			return
		}
		time.Sleep(connectDelay)
		// version 5, succeeded, reserved, IPv4 bound address 0.0.0.0:0.
		if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
			return
		}
		handle(conn, cert)
	})
}

// TestNew_SOCKS5TLS_SucceedsWithinBudget is the SOCKS5-proxy companion to
// TestNew_ProxiedTLS_SucceedsWithinBudget: a dial, the SOCKS5 handshake, and
// the target TLS handshake that together stay under ConnectTimeout must
// still succeed through an env-configured SOCKS5 proxy — proving the
// shared budget didn't just make every SOCKS5-proxied TLS connect fail.
func TestNew_SOCKS5TLS_SucceedsWithinBudget(t *testing.T) {
	const (
		connectTimeout       = 600 * time.Millisecond
		dialDelay            = 50 * time.Millisecond
		handshakeDelay       = 50 * time.Millisecond
		targetHandshakeDelay = 50 * time.Millisecond
	)
	overrideConnectTimeout(t, connectTimeout)
	overrideDial(t, slowDial(dialDelay))

	s := newSOCKS5TLSStub(t, handshakeDelay, targetHandshakeDelay, func(conn net.Conn, cert tls.Certificate) {
		tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		defer func() { _ = tlsConn.Close() }()
		buf := make([]byte, 4096)
		_, _ = tlsConn.Read(buf)
		_, _ = tlsConn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
	})
	overrideProxyForRequest(t, func(*http.Request) (*url.URL, error) {
		return &url.URL{Scheme: "socks5", Host: s.addr}, nil
	})

	client := New()
	req, err := http.NewRequest(http.MethodGet, s.tlsURL("/object"), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v (dial %v + socks5 handshake %v + target TLS handshake %v is well within the %v combined budget)",
			err, dialDelay, handshakeDelay, targetHandshakeDelay, connectTimeout)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want %q", body, "ok")
	}
}

// TestNew_SOCKS5TLS_ConnectBudget_SharedAcrossDialHandshakeAndTLS is the
// SOCKS5-proxy companion to
// TestNew_ProxiedTLS_ConnectBudget_SharedAcrossDialConnectAndHandshake, and
// review round 4's required test: with a socks5 proxy configured, the
// proxy dial, the SOCKS5 handshake, and the target TLS handshake must share
// ONE ConnectTimeout budget. Each of the three stages here is individually
// well under ConnectTimeout, but their sum is not — net/http's own built-in
// SOCKS5 handling (what this package left in place for this combination
// before this fix) let the SOCKS5 handshake ride IdleTimeout and gave the
// TLS handshake that follows a separate TLSHandshakeTimeout on top, so this
// scenario used to succeed at roughly the stages' sum instead of failing
// near ConnectTimeout.
func TestNew_SOCKS5TLS_ConnectBudget_SharedAcrossDialHandshakeAndTLS(t *testing.T) {
	const (
		connectTimeout       = 300 * time.Millisecond
		dialDelay            = 150 * time.Millisecond
		handshakeDelay       = 150 * time.Millisecond
		targetHandshakeDelay = 150 * time.Millisecond
	)
	overrideConnectTimeout(t, connectTimeout)
	overrideDial(t, slowDial(dialDelay))

	s := newSOCKS5TLSStub(t, handshakeDelay, targetHandshakeDelay, func(conn net.Conn, cert tls.Certificate) {
		tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
		_ = tlsConn.Handshake()
	})
	overrideProxyForRequest(t, func(*http.Request) (*url.URL, error) {
		return &url.URL{Scheme: "socks5", Host: s.addr}, nil
	})

	client := New()
	req, err := http.NewRequest(http.MethodGet, s.tlsURL("/object"), nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("expected the SOCKS5-proxied connect phase to fail: dial (%v) + socks5 handshake (%v) + target TLS handshake (%v) together exceed the %v combined budget",
			dialDelay, handshakeDelay, targetHandshakeDelay, connectTimeout)
	}
	// Separately-timed stages (net/http's own built-in SOCKS5 handling)
	// would let this succeed at roughly the three stages' sum (~450ms) or
	// later. A single shared budget must cut it off close to ConnectTimeout
	// (~300ms).
	if elapsed > connectTimeout+200*time.Millisecond {
		t.Fatalf("took %v to fail; a %v combined SOCKS5-proxied-connect budget should have cut it well before the stages' sum of %v",
			elapsed, connectTimeout, dialDelay+handshakeDelay+targetHandshakeDelay)
	}
}

// TestIdleConn_MidBodyStall_UsesIdleTimeout is the self-attack's "upload
// stalls before the whole body is handed off" case for the response-window
// policy: a Write that blocks because the peer never reads must still fail
// at ~IdleTimeout, not ResponseTimeout, because bodyDone has not fired yet
// — the response window must never cover a stall that happens before the
// body finishes sending, only the wait that follows it. net.Pipe is used
// for the same determinism reason as TestIdleConn_WriteTimesOutWhenPeerNeverReads:
// a real TCP socket's kernel send buffer makes it unreliable to force a
// Write to actually block.
func TestIdleConn_MidBodyStall_UsesIdleTimeout(t *testing.T) {
	client, _ := net.Pipe()
	defer func() { _ = client.Close() }()

	signal := &bodyDoneSignal{} // never marked done: this is mid-body.
	wrapped := &idleConn{
		Conn:        client,
		idleTimeout: 80 * time.Millisecond,
		bodyDone:    signal,
	}
	signal.bind(wrapped, 10*time.Second)
	if err := wrapped.SetDeadline(time.Now().Add(wrapped.idleTimeout)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	start := time.Now()
	_, err := wrapped.Write([]byte("mid-body bytes with no reader on the other end"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the write to time out with no reader ever draining the pipe")
	}
	netErr, ok := err.(net.Error)
	if !ok || !netErr.Timeout() {
		t.Fatalf("err = %v (%T), want a net.Error reporting Timeout() == true", err, err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %v to time out; an 80ms idle timeout should have cut it well before this (bodyDone unmarked means the fixed post-handoff window must not apply)", elapsed)
	}
}

// TestIdleConn_PostHandoff_ExtendsToResponseTimeout is the policy fix
// itself, proven directly on idleConn (see
// TestIdleConn_MidBodyStall_UsesIdleTimeout's doc comment for why a real
// socket isn't used here): once bodyDone reports the whole request body
// handed off, the wait that follows — the gap between the last successful
// Write and the next successful Read — is governed by the fixed
// post-handoff window markDone computed, not idleTimeout, even though the
// deadline active entering that gap was set by an EARLIER mid-body write
// under the shorter basis.
func TestIdleConn_PostHandoff_ExtendsToResponseTimeout(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	signal := &bodyDoneSignal{}
	wrapped := &idleConn{
		Conn:        client,
		idleTimeout: 80 * time.Millisecond,
		bodyDone:    signal,
	}
	signal.bind(wrapped, 400*time.Millisecond)
	if err := wrapped.SetDeadline(time.Now().Add(wrapped.idleTimeout)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}

	// A mid-body write, drained immediately: succeeds under idleTimeout and
	// leaves the deadline set on that basis, exactly like every write
	// before the body finishes (bodyDone is not yet marked).
	go func() { buf := make([]byte, 64); _, _ = server.Read(buf) }()
	if _, err := wrapped.Write([]byte("mid-body chunk")); err != nil {
		t.Fatalf("mid-body Write: %v", err)
	}

	// The body is now fully handed off — this is the final write. markDone
	// fixes the post-handoff deadline at now+400ms (the responseTimeout
	// bind captured above).
	signal.markDone()
	go func() { buf := make([]byte, 64); _, _ = server.Read(buf) }()
	if _, err := wrapped.Write([]byte("final flush")); err != nil {
		t.Fatalf("final-flush Write: %v", err)
	}

	// The wait for the response. A 150ms gap exceeds idleTimeout (80ms) but
	// stays under the fixed 400ms post-handoff window — it can only
	// succeed if the deadline really did switch bases when markDone fired
	// above.
	go func() {
		time.Sleep(150 * time.Millisecond)
		_, _ = server.Write([]byte("resp"))
	}()
	start := time.Now()
	buf := make([]byte, 4)
	_, err := wrapped.Read(buf)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Read: %v (a 150ms gap is within the fixed 400ms post-handoff window; an idleTimeout-bound deadline would have failed at ~80ms)", err)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("Read returned in %v, expected it to have waited for the delayed response (~150ms)", elapsed)
	}
}

// TestIdleConn_PostHandoff_FailsAtResponseTimeout is the other required
// shape: once bodyDone fires, a response that never arrives at all must
// still fail — bounded by the fixed post-handoff window, not left to hang
// forever.
func TestIdleConn_PostHandoff_FailsAtResponseTimeout(t *testing.T) {
	client, _ := net.Pipe()
	defer func() { _ = client.Close() }()

	signal := &bodyDoneSignal{}
	wrapped := &idleConn{
		Conn: client,
		// Deliberately long so only the fixed post-handoff window could
		// plausibly cut this off.
		idleTimeout: 3 * time.Second,
		bodyDone:    signal,
	}
	signal.bind(wrapped, 100*time.Millisecond)
	signal.markDone()

	start := time.Now()
	buf := make([]byte, 4)
	_, err := wrapped.Read(buf)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected the read to time out: nothing ever writes a response on the other end of the pipe")
	}
	netErr, ok := err.(net.Error)
	if !ok || !netErr.Timeout() {
		t.Fatalf("err = %v (%T), want a net.Error reporting Timeout() == true", err, err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %v to time out; a 100ms fixed post-handoff window should have cut it well before this", elapsed)
	}
}

// TestIdleConn_PostHandoff_HeaderTrickle_CannotExtendFixedWindow is the
// required negative case for the fix (review round 4's finding): once
// bodyDone fires, a peer that keeps trickling a byte every gap — each gap
// individually well under the fixed post-handoff window — must still fail
// once the window's absolute point in time passes, instead of each
// successful read pushing that deadline further out the way an
// IdleTimeout-style sliding deadline would. idleTimeout is deliberately set
// far longer than the window so only the fixed window could plausibly cut
// this off: an IdleTimeout-based extension on every trickled byte would let
// this run far past 2s.
func TestIdleConn_PostHandoff_HeaderTrickle_CannotExtendFixedWindow(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	signal := &bodyDoneSignal{}
	wrapped := &idleConn{
		Conn:        client,
		idleTimeout: 3 * time.Second,
		bodyDone:    signal,
	}
	const window = 200 * time.Millisecond
	signal.bind(wrapped, window)
	signal.markDone()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for i := byte(0); ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := server.Write([]byte{i}); err != nil {
				return
			}
			time.Sleep(60 * time.Millisecond)
		}
	}()

	start := time.Now()
	buf := make([]byte, 1)
	var err error
	for {
		_, err = wrapped.Read(buf)
		if err != nil {
			break
		}
		if time.Since(start) > 2*time.Second {
			t.Fatal("trickled bytes kept the read alive for over 2s; the fixed post-handoff window should have cut it off at ~200ms regardless of the trickle")
		}
	}
	elapsed := time.Since(start)

	netErr, ok := err.(net.Error)
	if !ok || !netErr.Timeout() {
		t.Fatalf("err = %v (%T), want a net.Error reporting Timeout() == true", err, err)
	}
	if elapsed > window+800*time.Millisecond {
		t.Fatalf("took %v to time out; the fixed %v post-handoff window should have cut it off regardless of the trickle, not been extended by it", elapsed, window)
	}
}

// TestIdleConn_PostHandoff_AfterHeadersReceived_UsesIdleTimeoutForBody is
// the fix's other required half: once markHeadersReceived reports the
// response's headers are fully read, a response BODY that arrives slowly —
// each gap exceeding the now-irrelevant fixed post-handoff window but
// comfortably under idleTimeout — must still succeed, because the
// connection switches back to the normal sliding idleTimeout immediately
// when headers are marked received (not only on the next successful read,
// which would otherwise race against a header-wait deadline already on the
// verge of (or past) expiring).
func TestIdleConn_PostHandoff_AfterHeadersReceived_UsesIdleTimeoutForBody(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	signal := &bodyDoneSignal{}
	wrapped := &idleConn{
		Conn:        client,
		idleTimeout: 150 * time.Millisecond,
		bodyDone:    signal,
	}
	// A short fixed window: letting it keep governing reads after headers
	// are marked received would starve this test almost immediately.
	signal.bind(wrapped, 80*time.Millisecond)
	signal.markDone()
	signal.markHeadersReceived()

	go func() {
		for i := byte(0); i < 3; i++ {
			time.Sleep(100 * time.Millisecond)
			_, _ = server.Write([]byte{i})
		}
	}()

	start := time.Now()
	buf := make([]byte, 1)
	for i := 0; i < 3; i++ {
		if _, err := wrapped.Read(buf); err != nil {
			t.Fatalf("Read chunk %d: %v (each 100ms gap is within the 150ms idleTimeout that should govern a post-headers body read; the already-elapsed 80ms fixed window must not apply anymore)", i, err)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 250*time.Millisecond {
		t.Fatalf("finished in %v; expected it to span all three 100ms gaps (well past the 80ms fixed window) to be a meaningful proof", elapsed)
	}
}

// TestIdleConn_HeadersReceivedBeforeBodyDone_MarkDoneDoesNotShortenDeadline
// is a self-attack input the fix must not regress on: a storage service
// that answers (an early validation error, say) before the request body
// has finished sending, so markHeadersReceived fires BEFORE markDone does
// — the reverse of every other test's ordering. markDone must not then
// force the connection back down to the short, fixed header-wait window:
// headers have already arrived, so the connection's deadline is already
// correctly on the normal IdleTimeout basis, and markDone firing later must
// leave it there.
func TestIdleConn_HeadersReceivedBeforeBodyDone_MarkDoneDoesNotShortenDeadline(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	signal := &bodyDoneSignal{}
	wrapped := &idleConn{
		Conn:        client,
		idleTimeout: 400 * time.Millisecond,
		bodyDone:    signal,
	}
	// A short fixed window: if markDone wrongly re-imposed it below, the
	// 200ms wait further down would fail.
	signal.bind(wrapped, 80*time.Millisecond)

	signal.markHeadersReceived() // headers arrive first...
	signal.markDone()            // ...then the body finishes being sent.

	go func() {
		time.Sleep(200 * time.Millisecond) // > the 80ms fixed window, < the 400ms idleTimeout
		_, _ = server.Write([]byte("x"))
	}()

	buf := make([]byte, 1)
	if _, err := wrapped.Read(buf); err != nil {
		t.Fatalf("Read: %v (a 200ms gap is within the 400ms idleTimeout that should still govern this read; markDone must not have forced the deadline back down to the 80ms fixed window after headers already arrived)", err)
	}
}

// TestNew_Upload_PostHandoff_SlowResponseSucceeds is the end-to-end proof,
// through a real client.Do() PUT request wired via WrapUploadBody, that a
// storage response arriving after longer than IdleTimeout but within
// ResponseTimeout succeeds — the exact shape of the reported bug (a
// slow-but-moving upload whose OS-buffer drain plus storage confirmation
// legitimately takes longer than the per-byte inactivity window once the
// client has handed off every byte). On the pre-fix code this is exactly
// TestNew_Upload_PostHandoff_NoResponseFailsAtResponseTimeout's shape with a
// shorter delay, and it would fail at ~IdleTimeout instead of succeeding.
func TestNew_Upload_PostHandoff_SlowResponseSucceeds(t *testing.T) {
	overrideIdleTimeout(t, 80*time.Millisecond)
	overrideResponseTimeout(t, 1*time.Second)

	const payload = "the entire request body, all of it, handed off at once"

	s := newStub(t, func(conn net.Conn) {
		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, req.Body) // drain the whole body
		time.Sleep(200 * time.Millisecond)   // > IdleTimeout, < ResponseTimeout
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
	})

	client := New()
	ctx, body := WrapUploadBody(context.Background(), strings.NewReader(payload))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.url("/object"), body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.ContentLength = int64(len(payload))

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Do: %v (a 200ms post-handoff wait is within the 1s ResponseTimeout; an 80ms IdleTimeout-bound wait would have failed)", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(respBody) != "ok" {
		t.Fatalf("body = %q, want %q", respBody, "ok")
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("Do returned in %v; expected it to have spanned the 200ms post-handoff delay to be a meaningful proof", elapsed)
	}
}

// TestNew_Upload_PostHandoff_NoResponseFailsAtResponseTimeout is the other
// required shape at the full client.Do() level: a server that reads the
// entire request body and then never responds must fail at ~ResponseTimeout
// — not hang forever, and not fail at ~IdleTimeout (which no longer governs
// this exact wait once the whole body has been handed off).
func TestNew_Upload_PostHandoff_NoResponseFailsAtResponseTimeout(t *testing.T) {
	overrideIdleTimeout(t, 2*time.Second) // deliberately long: only ResponseTimeout should cut this off
	overrideResponseTimeout(t, 150*time.Millisecond)

	const payload = "the entire request body, all of it, handed off at once"

	s := newStub(t, func(conn net.Conn) {
		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, req.Body) // drain the whole body, then stay silent
	})

	client := New()
	ctx, body := WrapUploadBody(context.Background(), strings.NewReader(payload))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.url("/object"), body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.ContentLength = int64(len(payload))

	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected the upload to fail: the server read the whole body and never responded")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %v to fail; a 150ms ResponseTimeout should have cut it well before this (and well before the 2s IdleTimeout)", elapsed)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("took %v to fail; too fast to have been cut by the 150ms ResponseTimeout rather than something else", elapsed)
	}
}
