// Tests for the storage transfer timeout policy applied to the upload leg:
// a bounded connect phase and a bounded inactivity window, but no cap on
// total transfer duration. See internal/transferclient for the shared
// mechanism and consumer/storage_transfer_timeout_test.go for the download
// side.
//
// These use a raw TCP stub, not httptest.Server, so a test controls exactly
// when bytes are written (or withheld) on the storage leg — the thing this
// fix bounds.
package producer

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helix-tools/sdk-go/v2/internal/transferclient"
)

// rawStub is a bare TCP listener on 127.0.0.1. handle runs in its own
// goroutine per accepted connection; every accepted connection is
// force-closed on test cleanup so a handler that intentionally never closes
// its end (to hold a connection open and silent) cannot leak past the test.
type rawStub struct {
	ln    net.Listener
	addr  string
	mu    sync.Mutex
	conns []net.Conn
}

func newRawStub(t *testing.T, handle func(net.Conn)) *rawStub {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	s := &rawStub{ln: ln, addr: ln.Addr().String()}

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

func (s *rawStub) url(path string) string { return "http://" + s.addr + path }

// overrideStorageIdleTimeout sets transferclient.IdleTimeout for one test and
// restores it on cleanup — the internal seam this fix exposes so a test runs
// in milliseconds instead of the real 60s default.
func overrideStorageIdleTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := transferclient.IdleTimeout
	transferclient.IdleTimeout = d
	t.Cleanup(func() { transferclient.IdleTimeout = orig })
}

// TestUploadToPresignedURL_UnboundedClient_HangsOnStall_NegativeControl is
// the negative control: it pins today's bug by showing the pre-fix client
// shape (httpClient: &http.Client{}, no timeout of any kind) is STILL
// blocked 400ms after the peer accepted the connection and went silent — a
// peer that never answers hangs this client forever. The next test is the
// same stall, same probe path, through the fix.
func TestUploadToPresignedURL_UnboundedClient_HangsOnStall_NegativeControl(t *testing.T) {
	stall := newRawStub(t, func(conn net.Conn) {
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf) // drains the small PUT body; never responds
	})

	p := &Producer{httpClient: &http.Client{}} // the pre-fix shape

	done := make(chan error, 1)
	go func() {
		done <- p.uploadToPresignedURL(context.Background(), stall.url("/object"), []byte("payload"))
	}()

	select {
	case err := <-done:
		t.Fatalf("expected the unbounded pre-fix client to still be blocked after 400ms; it returned (err=%v) instead, so this negative control no longer demonstrates the bug", err)
	case <-time.After(400 * time.Millisecond):
		// Confirmed still blocked. Test cleanup closes the stub's accepted
		// connection, which unblocks the leaked goroutine above so it does
		// not outlive the test.
	}
}

// TestUploadToPresignedURL_StorageStall_NoReplyAfterAccept is the fix: the
// identical stall, through the dedicated storage client, fails cleanly
// within the (overridden, for test speed) idle timeout — instead of hanging
// forever as the previous test proved the old shape does.
func TestUploadToPresignedURL_StorageStall_NoReplyAfterAccept(t *testing.T) {
	overrideStorageIdleTimeout(t, 100*time.Millisecond)

	stall := newRawStub(t, func(conn net.Conn) {
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
	})

	p := &Producer{storageClient: transferclient.New()}

	start := time.Now()
	err := p.uploadToPresignedURL(context.Background(), stall.url("/object"), []byte("payload"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected uploadToPresignedURL to fail against a peer that never answers")
	}
	if !strings.Contains(err.Error(), "failed to upload to presigned URL") {
		t.Errorf("error = %q, want it to name the upload phase", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("took %v to fail; a 100ms idle timeout should have cut it well before this", elapsed)
	}
}

// TestUploadToPresignedURL_StorageDrop_MidResponse is a regression check: an
// abrupt connection drop (as opposed to a silent stall) must still surface a
// clean error through the new storage client exactly as it did through the
// old one — the probe that scoped this fix found drops already handled;
// this pins that the new client didn't regress it.
func TestUploadToPresignedURL_StorageDrop_MidResponse(t *testing.T) {
	overrideStorageIdleTimeout(t, 2*time.Second) // Idle timeout must not be what catches this.

	drop := newRawStub(t, func(conn net.Conn) {
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		_, _ = conn.Write([]byte("HTTP/1.1 200 O")) // Partial status line.
		_ = conn.Close()                            // Abrupt drop, not a stall.
	})

	p := &Producer{storageClient: transferclient.New()}

	start := time.Now()
	err := p.uploadToPresignedURL(context.Background(), drop.url("/object"), []byte("payload"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected uploadToPresignedURL to fail on a connection dropped mid-response")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %v; a mid-response drop should surface almost immediately, not wait for the idle timeout", elapsed)
	}
}

// TestUploadToPresignedURL_SlowButHealthy_Succeeds proves the fix does not
// newly truncate an upload whose response is merely slow to arrive: the
// server writes its (otherwise instant) response in small chunks, each gap
// under the idle timeout, summing to well over it.
func TestUploadToPresignedURL_SlowButHealthy_Succeeds(t *testing.T) {
	overrideStorageIdleTimeout(t, 80*time.Millisecond)

	const gap = 25 * time.Millisecond
	resp := "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"

	healthy := newRawStub(t, func(conn net.Conn) {
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		for i := 0; i < len(resp); i += 5 {
			time.Sleep(gap)
			end := min(i+5, len(resp))
			_, _ = conn.Write([]byte(resp[i:end]))
		}
	})

	p := &Producer{storageClient: transferclient.New()}

	start := time.Now()
	err := p.uploadToPresignedURL(context.Background(), healthy.url("/object"), []byte("payload"))
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("uploadToPresignedURL failed: %v", err)
	}
	if elapsed < 2*80*time.Millisecond {
		t.Fatalf("upload finished in %v; expected it to span multiple idle-timeout windows to be a meaningful proof", elapsed)
	}
}

// TestUploadToPresignedURL_ConcurrentTransfers_ShareOneClientIndependently
// is the self-attack's concurrency case: two uploads sharing one
// *http.Client built by transferclient.New(), one against a stalling peer
// and one against a healthy slow peer, run at the same time. Proves
// per-connection deadlines are independent — a shared client's idle
// tracking is not global state that lets one upload's stall affect
// another's.
func TestUploadToPresignedURL_ConcurrentTransfers_ShareOneClientIndependently(t *testing.T) {
	overrideStorageIdleTimeout(t, 100*time.Millisecond)
	shared := transferclient.New()

	stall := newRawStub(t, func(conn net.Conn) {
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
	})
	healthy := newRawStub(t, func(conn net.Conn) {
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		time.Sleep(30 * time.Millisecond)
		_, _ = conn.Write([]byte("HTTP/1.1 200 O"))
		time.Sleep(30 * time.Millisecond)
		_, _ = conn.Write([]byte("K\r\nContent-Length: 0\r\n\r\n"))
	})

	pStall := &Producer{storageClient: shared}
	pHealthy := &Producer{storageClient: shared}

	var wg sync.WaitGroup
	var stallErr, healthyErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		stallErr = pStall.uploadToPresignedURL(context.Background(), stall.url("/object"), []byte("payload"))
	}()
	go func() {
		defer wg.Done()
		healthyErr = pHealthy.uploadToPresignedURL(context.Background(), healthy.url("/object"), []byte("payload"))
	}()
	wg.Wait()

	if stallErr == nil {
		t.Error("stalled upload: expected a timeout error, got none")
	}
	if healthyErr != nil {
		t.Errorf("healthy upload: expected success, got %v", healthyErr)
	}
}
