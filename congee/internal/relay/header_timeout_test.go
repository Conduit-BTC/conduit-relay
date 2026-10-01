package relay

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestRelayHeaderTimeoutClosesIncompleteHandshake(t *testing.T) {
	srv, err := NewServer(testRelayConfig(), &visibilityStoreStub{}, zerolog.Nop(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.metricsCancel)
	if srv.http.ReadHeaderTimeout <= 0 || srv.http.ReadHeaderTimeout > 30*time.Second {
		t.Fatalf("unsafe header timeout: %v", srv.http.ReadHeaderTimeout)
	}
	srv.http.ReadHeaderTimeout = 50 * time.Millisecond
	ts := httptest.NewUnstartedServer(srv.http.Handler)
	ts.Config = srv.http
	ts.Start()
	t.Cleanup(ts.Close)
	c, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: relay.example\r\nX-Incomplete: "); err != nil {
		t.Fatal(err)
	}
	if err := c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(bufio.NewReader(c))
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("incomplete headers stayed open beyond the header deadline")
	}
	if srv.OpenConnections() != 0 {
		t.Fatal("incomplete handshake reached WebSocket admission")
	}
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthy requests failed after stalled handshake: %d", resp.StatusCode)
	}
}
