package relay

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestServeAfterShutdownDoesNotStartBackgroundWorkers(t *testing.T) {
	t.Parallel()
	srv, err := NewServer(testRelayConfig(), &visibilityStoreStub{}, zerolog.Nop(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve after shutdown: %v", err)
	}
	if srv.StartedAtUnix() != 0 {
		t.Fatal("shutdown server started background workers")
	}
	if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Serve left the rejected listener open: %v", err)
	}
}

func TestServeConcurrentWithShutdown(t *testing.T) {
	t.Parallel()
	srv, err := NewServer(testRelayConfig(), &visibilityStoreStub{}, zerolog.Nop(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	gate := make(chan struct{})
	served, stopped := make(chan error, 1), make(chan error, 1)
	go func() {
		<-gate
		served <- srv.Serve(ln)
	}()
	go func() {
		<-gate
		stopped <- srv.Shutdown(ctx)
	}()
	close(gate)
	for _, done := range []<-chan error{served, stopped} {
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("concurrent startup and shutdown did not complete")
		}
	}
	if srv.readQueue.ctx.Err() == nil || srv.metricsCtx.Err() == nil {
		t.Fatal("shutdown left background worker contexts active")
	}
}
