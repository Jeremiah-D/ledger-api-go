package main

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestGracefulShutdownDrainsInFlight verifies that cancelling the run context
// (the SIGTERM path) lets an in-flight request finish instead of dropping it,
// and that the listener stops accepting new connections afterwards.
func TestGracefulShutdownDrainsInFlight(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once = make(chan struct{}, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case once <- struct{}{}:
			close(started) // signal: the request is now in flight
		default:
		}
		<-release // simulate a slow in-flight request
		w.WriteHeader(http.StatusOK)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, ln, handler, 5*time.Second) }()

	respCh := make(chan *http.Response, 1)
	reqErr := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + addr)
		if err != nil {
			reqErr <- err
			return
		}
		respCh <- resp
	}()

	// Wait until the handler is actually inside the request: now the drain
	// path is deterministic, no sleeps.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("handler never started serving the request")
	}

	cancel()       // the SIGTERM equivalent
	close(release) // let the in-flight handler finish

	select {
	case err := <-reqErr:
		t.Fatalf("in-flight request failed during shutdown: %v", err)
	case resp := <-respCh:
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("in-flight request status = %d, want 200", resp.StatusCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request was not drained")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServer returned %v, want clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runServer did not return after shutdown")
	}

	// The listener must be closed: no new connections after shutdown.
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatal("listener still accepting connections after shutdown")
	}
}

func TestShutdownTimeoutParsing(t *testing.T) {
	t.Setenv("SHUTDOWN_TIMEOUT", "")
	if got := shutdownTimeout(); got != 10*time.Second {
		t.Errorf("default shutdownTimeout = %v, want 10s", got)
	}
	t.Setenv("SHUTDOWN_TIMEOUT", "25s")
	if got := shutdownTimeout(); got != 25*time.Second {
		t.Errorf("shutdownTimeout = %v, want 25s", got)
	}
	for _, bad := range []string{"nope", "-5s", "0s"} {
		t.Setenv("SHUTDOWN_TIMEOUT", bad)
		if got := shutdownTimeout(); got != 10*time.Second {
			t.Errorf("shutdownTimeout(%q) = %v, want 10s fallback", bad, got)
		}
	}
}
