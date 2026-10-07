// Tests for the storage transfer timeout policy applied to the download
// leg: a bounded connect phase and a bounded inactivity window, but no cap
// on total transfer duration. See internal/transferclient for the shared
// mechanism and producer/storage_transfer_timeout_test.go for the upload
// side.
//
// These use a raw TCP stub, not httptest.Server, so a test controls exactly
// when bytes are written (or withheld) on the storage leg — the thing this
// fix bounds. The API leg (metadata, the signed-URL lookup, the outcome
// callback) still uses the existing fakeAPI/httptest helper from
// download_outcome_callback_test.go.
package consumer

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

// consumerAgainstStorageStub builds a Consumer whose API calls go to a
// fakeAPI (metadata, signed-URL issue, outcome callback, KMS) but whose
// signed download URL points at storageURL — a raw stub this test fully
// controls. storageClient is assigned directly, bypassing NewConsumer (which
// would reach real AWS).
func consumerAgainstStorageStub(t *testing.T, storageURL string, storageClient *http.Client) (*Consumer, *fakeAPI) {
	t.Helper()
	f := newFakeAPI(t)
	f.urlInfo = func() *DownloadURLInfo {
		return &DownloadURLInfo{
			DownloadURL: storageURL,
			ExpiresAt:   "2026-05-04T23:00:00Z",
			EventID:     "evt-test-1",
		}
	}
	c := newTestConsumer(f.server.URL)
	c.storageClient = storageClient
	return c, f
}

// TestDownloadDataset_StorageStall_NoReplyAfterAccept is the self-attack's
// "drop at the header-wait stage" for a download: the connection succeeds
// but the peer never answers. Proves the idle timeout bounds waiting for
// response headers, not just a stall mid-body.
func TestDownloadDataset_StorageStall_NoReplyAfterAccept(t *testing.T) {
	overrideStorageIdleTimeout(t, 100*time.Millisecond)

	stall := newRawStub(t, func(conn net.Conn) {
		// Never read or write anything; held open until cleanup closes it.
		_ = conn
	})

	c, _ := consumerAgainstStorageStub(t, stall.url("/object"), transferclient.New())

	out := filepath.Join(t.TempDir(), "out.bin")
	start := time.Now()
	err := c.DownloadDataset(context.Background(), "ds-1", out)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected DownloadDataset to fail against a peer that never answers")
	}
	if !strings.Contains(err.Error(), "failed to download") {
		t.Errorf("error = %q, want it to name the download phase", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("took %v to fail; a 100ms idle timeout should have cut it well before this", elapsed)
	}
}

// TestDownloadDataset_StorageStall_HeadersThenStallMidBody covers the other
// half of the self-attack's stall case: headers arrive, declaring more body
// than ever comes.
func TestDownloadDataset_StorageStall_HeadersThenStallMidBody(t *testing.T) {
	overrideStorageIdleTimeout(t, 100*time.Millisecond)

	stall := newRawStub(t, func(conn net.Conn) {
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 1000000\r\n\r\n"))
		// The declared body never arrives.
	})

	c, _ := consumerAgainstStorageStub(t, stall.url("/object"), transferclient.New())

	out := filepath.Join(t.TempDir(), "out.bin")
	start := time.Now()
	err := c.DownloadDataset(context.Background(), "ds-1", out)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected DownloadDataset to fail on a body that stalls partway")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("took %v to fail; a 100ms idle timeout should have cut it well before this", elapsed)
	}
}

// TestDownloadDataset_StorageDrop_MidBody is a regression check: an abrupt
// connection drop mid-transfer (as opposed to a silent stall) must still
// surface a clean error through the new storage client exactly as it did
// through the old one — the probe that scoped this fix found drops already
// handled; this pins that the new client didn't regress it.
func TestDownloadDataset_StorageDrop_MidBody(t *testing.T) {
	overrideStorageIdleTimeout(t, 2*time.Second) // Idle timeout must not be what catches this.

	drop := newRawStub(t, func(conn net.Conn) {
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 1000000\r\n\r\nSOME"))
		_ = conn.Close() // Abrupt drop, not a stall.
	})

	c, _ := consumerAgainstStorageStub(t, drop.url("/object"), transferclient.New())

	out := filepath.Join(t.TempDir(), "out.bin")
	start := time.Now()
	err := c.DownloadDataset(context.Background(), "ds-1", out)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected DownloadDataset to fail on a connection dropped mid-body")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("took %v; a mid-body drop should surface almost immediately, not wait for the idle timeout", elapsed)
	}
}

// TestDownloadDataset_SlowButHealthy_NoTotalCap is the negative control that
// reproduces the reported bug and proves the fix: the identical trickling
// transfer FAILS through an old-style total-duration-capped client and
// SUCCEEDS through the new idle-based storage client.
func TestDownloadDataset_SlowButHealthy_NoTotalCap(t *testing.T) {
	plaintext := []byte("hello world, this is a healthy slow download")
	object := encryptedObject(plaintext)

	const (
		gap        = 60 * time.Millisecond
		nChunks    = 4
		oldTotal   = 150 * time.Millisecond // stand-in for today's fixed total-duration cap
		idleBudget = 120 * time.Millisecond // > gap, so no single wait trips it
	)

	newTrickleStub := func(t *testing.T) *rawStub {
		return newRawStub(t, func(conn net.Conn) {
			buf := make([]byte, 4096)
			_, _ = conn.Read(buf)
			header := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", len(object))
			_, _ = conn.Write([]byte(header))
			chunkSize := (len(object) + nChunks - 1) / nChunks
			for i := 0; i < len(object); i += chunkSize {
				time.Sleep(gap)
				end := min(i+chunkSize, len(object))
				_, _ = conn.Write(object[i:end])
			}
		})
	}

	t.Run("old total-duration-capped client fails a healthy slow transfer", func(t *testing.T) {
		stub := newTrickleStub(t)
		c, _ := consumerAgainstStorageStub(t, stub.url("/object"), nil)
		// nil storageClient falls back to httpClient (storageHTTPClient's
		// documented fallback) — set httpClient directly to the exact shape
		// of today's architecture: one client, one total-duration Timeout,
		// used for the storage leg.
		c.httpClient = &http.Client{Timeout: oldTotal}

		out := filepath.Join(t.TempDir(), "out.bin")
		err := c.DownloadDataset(context.Background(), "ds-1", out)
		if err == nil {
			t.Fatal("expected a total-duration-capped client to fail a transfer that outlives its cap, reproducing the reported bug")
		}
	})

	t.Run("new idle-based storage client succeeds on the identical transfer", func(t *testing.T) {
		overrideStorageIdleTimeout(t, idleBudget)
		stub := newTrickleStub(t)
		c, _ := consumerAgainstStorageStub(t, stub.url("/object"), transferclient.New())

		out := filepath.Join(t.TempDir(), "out.bin")
		start := time.Now()
		err := c.DownloadDataset(context.Background(), "ds-1", out)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("DownloadDataset failed: %v", err)
		}
		if elapsed <= oldTotal {
			t.Fatalf("transfer finished in %v, want it to outlive the %v old-style cap it must survive", elapsed, oldTotal)
		}

		got, rerr := os.ReadFile(out)
		if rerr != nil {
			t.Fatalf("reading output: %v", rerr)
		}
		if string(got) != string(plaintext) {
			t.Fatalf("downloaded content = %q, want %q", got, plaintext)
		}
	})
}

// TestDownloadDataset_ConcurrentTransfers_ShareOneClientIndependently is the
// self-attack's concurrency case: two Consumers sharing one *http.Client
// built by transferclient.New(), one against a stalling peer and one
// against a healthy slow peer, run at the same time. Proves per-connection
// deadlines are independent — a shared client's idle tracking is not global
// state that lets one request's stall affect another's.
func TestDownloadDataset_ConcurrentTransfers_ShareOneClientIndependently(t *testing.T) {
	overrideStorageIdleTimeout(t, 100*time.Millisecond)
	shared := transferclient.New()

	plaintext := []byte("healthy concurrent download")
	object := encryptedObject(plaintext)

	stall := newRawStub(t, func(conn net.Conn) { _ = conn })
	healthy := newRawStub(t, func(conn net.Conn) {
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		header := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", len(object))
		_, _ = conn.Write([]byte(header))
		half := len(object) / 2
		time.Sleep(30 * time.Millisecond)
		_, _ = conn.Write(object[:half])
		time.Sleep(30 * time.Millisecond)
		_, _ = conn.Write(object[half:])
	})

	cStall, _ := consumerAgainstStorageStub(t, stall.url("/object"), shared)
	cHealthy, _ := consumerAgainstStorageStub(t, healthy.url("/object"), shared)

	var wg sync.WaitGroup
	var stallErr, healthyErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		stallErr = cStall.DownloadDataset(context.Background(), "ds-1", filepath.Join(t.TempDir(), "stall.bin"))
	}()
	go func() {
		defer wg.Done()
		healthyOut := filepath.Join(t.TempDir(), "healthy.bin")
		healthyErr = cHealthy.DownloadDataset(context.Background(), "ds-1", healthyOut)
	}()
	wg.Wait()

	if stallErr == nil {
		t.Error("stalled transfer: expected a timeout error, got none")
	}
	if healthyErr != nil {
		t.Errorf("healthy transfer: expected success, got %v", healthyErr)
	}
}
