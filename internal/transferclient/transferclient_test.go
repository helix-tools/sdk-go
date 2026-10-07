package transferclient

import (
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
// scheme it has to tell apart. It must return nil ONLY for an HTTPS target
// behind an http:// proxy — handing that one combination to DialTLSContext's
// single shared ConnectTimeout budget — and otherwise must pass whatever
// proxyForRequest resolved straight through unchanged, preserving
// NO_PROXY/HTTP_PROXY/HTTPS_PROXY semantics for every other combination
// exactly like the bare http.ProxyFromEnvironment this replaces.
func TestNew_ProxyFunc_DelegatesByScheme(t *testing.T) {
	httpProxy := &url.URL{Scheme: "http", Host: "proxy.example:3128"}
	httpsProxy := &url.URL{Scheme: "https", Host: "proxy.example:3129"}
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
		{"https target behind https proxy passes through unchanged", "https", httpsProxy, nil, false},
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
