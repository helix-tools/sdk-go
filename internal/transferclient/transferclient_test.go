package transferclient

import (
	"io"
	"net"
	"net/http"
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

// TestNew_ConnectTimeoutWiredToTransport pins that ConnectTimeout actually
// reaches the transport construction (the TLS handshake bound is a directly
// inspectable *http.Transport field; the dial timeout itself is exercised
// behaviorally by net.Dialer, which this package relies on rather than
// re-proving).
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
