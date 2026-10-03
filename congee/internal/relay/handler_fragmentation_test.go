package relay

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsflate"
	"github.com/michmich112/congee/internal/nostr"
)

type fragmentationTransport struct {
	name       string
	negotiate  bool
	compressed bool
}

var fragmentationTransports = []fragmentationTransport{
	{name: "plain"},
	{name: "deflate/plain-message", negotiate: true},
	{name: "deflate/compressed-message", negotiate: true, compressed: true},
}

func fragmentedWirePayload(t *testing.T, payload []byte, compressed bool) []byte {
	t.Helper()
	if !compressed {
		return payload
	}
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	encoded := bytes.Clone(buf.Bytes())
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if len(encoded) < 4 || !bytes.Equal(encoded[len(encoded)-4:], []byte{0, 0, 255, 255}) {
		t.Fatal("fixture lacks permessage-deflate sync-flush tail")
	}
	return encoded[:len(encoded)-4]
}

func writeFragmentFrame(t *testing.T, conn net.Conn, frame ws.Frame) {
	t.Helper()
	if err := ws.WriteFrame(conn, ws.MaskFrame(frame)); err != nil {
		t.Fatal(err)
	}
}

func readFragmentReply(t *testing.T, conn net.Conn) ws.Frame {
	t.Helper()
	f, err := ws.ReadFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// The ping response is a wire barrier: the relay must handle controls before
// the final continuation arrives, without dispatching partial Nostr JSON.
func writeFragmentedText(t *testing.T, conn net.Conn, payload []byte, compressed bool) {
	t.Helper()
	encoded := fragmentedWirePayload(t, payload, compressed)
	firstEnd := len(encoded) / 3
	// Split a UTF-8 code point across plain frames. Validation belongs to the
	// complete text message, not to individual fragments.
	if !compressed {
		if i := bytes.Index(encoded, []byte("é")); i >= 0 {
			firstEnd = i + 1
		}
	}
	first := ws.NewFrame(ws.OpText, false, encoded[:firstEnd])
	if compressed {
		var err error
		first.Header, err = wsflate.SetBit(first.Header)
		if err != nil {
			t.Fatal(err)
		}
	}
	writeFragmentFrame(t, conn, first)
	writeFragmentFrame(t, conn, ws.NewPingFrame([]byte("fragment-barrier")))
	pong := readFragmentReply(t, conn)
	if pong.Header.OpCode != ws.OpPong || string(pong.Payload) != "fragment-barrier" {
		t.Fatalf("reply before final fragment = opcode %v, payload %q; want matching PONG", pong.Header.OpCode, pong.Payload)
	}
	writeFragmentFrame(t, conn, ws.NewFrame(ws.OpContinuation, false, nil))
	middleEnd := firstEnd + (len(encoded)-firstEnd)/2
	writeFragmentFrame(t, conn, ws.NewFrame(ws.OpContinuation, false, encoded[firstEnd:middleEnd]))
	writeFragmentFrame(t, conn, ws.NewPongFrame([]byte("unsolicited")))
	writeFragmentFrame(t, conn, ws.NewFrame(ws.OpContinuation, true, encoded[middleEnd:]))
}

func TestFragmentedNostrMessages(t *testing.T) {
	priv, _ := btcec.PrivKeyFromBytes([]byte{1})
	ev := signedTestEvent(t, priv, 1)
	ev.Content = "fragmented café 🛒"
	if err := ev.Sign(priv); err != nil {
		t.Fatal(err)
	}
	eventPayload, err := json.Marshal([]any{"EVENT", ev})
	if err != nil {
		t.Fatal(err)
	}
	for _, transport := range fragmentationTransports {
		t.Run(transport.name, func(t *testing.T) {
			srv, ts := newConduitTestServer(t, transport.negotiate, 60)
			received := make(chan any, 4)
			srv.RegisterMessageHandler("EVENT", func(_ context.Context, c *Conn, msg any) error {
				received <- msg
				return c.sendOK(msg.(*nostr.EventMessage).Event.ID, true, "")
			})
			srv.RegisterMessageHandler("REQ", func(_ context.Context, c *Conn, msg any) error {
				received <- msg
				return c.sendEOSE(msg.(*nostr.ReqMessage).SubID)
			})
			conn, resp, err := dialRelayOrigin(ts.URL, transport.negotiate, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if resp.Body != nil {
				defer resp.Body.Close()
			}
			if negotiated := strings.Contains(resp.Header.Get("Sec-WebSocket-Extensions"), "permessage-deflate"); negotiated != transport.negotiate {
				t.Fatalf("compression negotiated=%t, want %t", negotiated, transport.negotiate)
			}
			nc := conn.UnderlyingConn()
			if err := nc.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				// Reuse one connection and the flate reader across messages.
				writeFragmentedText(t, nc, eventPayload, transport.compressed)
				wantOK := fmt.Sprintf(`["OK",%q,true,""]`, ev.ID)
				if f := readFragmentReply(t, nc); f.Header.OpCode != ws.OpText || string(f.Payload) != wantOK {
					t.Fatalf("fragmented EVENT reply = %q, want %q", f.Payload, wantOK)
				}
				gotEvent, ok := (<-received).(*nostr.EventMessage)
				if !ok || gotEvent.Event.ID != ev.ID || gotEvent.Event.Content != ev.Content {
					t.Fatalf("EVENT dispatch lost data: %#v", gotEvent)
				}
				if err := gotEvent.Event.VerifySig(); err != nil {
					t.Fatalf("reassembled EVENT signature error=%v", err)
				}
				writeFragmentedText(t, nc, []byte(`["REQ","fragmented",{"kinds":[1],"#t":["café"]}]`), transport.compressed)
				if f := readFragmentReply(t, nc); f.Header.OpCode != ws.OpText || string(f.Payload) != `["EOSE","fragmented"]` {
					t.Fatalf("fragmented REQ reply = %q", f.Payload)
				}
				gotReq, ok := (<-received).(*nostr.ReqMessage)
				if !ok || gotReq.SubID != "fragmented" || len(gotReq.Filters) != 1 || len(gotReq.Filters[0].Kinds) != 1 || gotReq.Filters[0].Kinds[0] != 1 || len(gotReq.Filters[0].Tag["#t"]) != 1 || gotReq.Filters[0].Tag["#t"][0] != "café" {
					t.Fatalf("REQ dispatch lost data: %#v", gotReq)
				}
			}
			if len(received) != 0 {
				t.Fatal("fragmented messages dispatched more than once")
			}
		})
	}
}

func TestFragmentedMessageAggregateLimit(t *testing.T) {
	const limit = 512
	for _, transport := range fragmentationTransports {
		for _, size := range []int{limit - 1, limit, limit + 1} {
			t.Run(fmt.Sprintf("%s/%d", transport.name, size), func(t *testing.T) {
				srv, ts := newConduitTestServer(t, transport.negotiate, 60)
				srv.cfg.WebSocket.MaxMessageBytes = limit
				var dispatched atomic.Int32
				srv.RegisterMessageHandler("REQ", func(_ context.Context, c *Conn, msg any) error {
					dispatched.Add(1)
					return c.sendEOSE(msg.(*nostr.ReqMessage).SubID)
				})
				conn, resp, err := dialRelayOrigin(ts.URL, transport.negotiate, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				if resp.Body != nil {
					defer resp.Body.Close()
				}
				nc := conn.UnderlyingConn()
				if err := nc.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Fatal(err)
				}
				payload := []byte(`["REQ","limit",{"kinds":[1]}]`)
				payload = append(payload, bytes.Repeat([]byte(" "), size-len(payload))...)
				writeFragmentedText(t, nc, payload, transport.compressed)
				f, err := ws.ReadFrame(nc)
				if size > limit {
					if err == nil {
						t.Fatalf("oversized aggregate accepted: opcode=%v payload=%q", f.Header.OpCode, f.Payload)
					}
					if ne, ok := err.(net.Error); ok && ne.Timeout() {
						t.Fatalf("oversized message did not close connection: %v", err)
					}
					if dispatched.Load() != 0 {
						t.Fatal("oversized aggregate reached handler")
					}
					return
				}
				if err != nil || f.Header.OpCode != ws.OpText || string(f.Payload) != `["EOSE","limit"]` || dispatched.Load() != 1 {
					t.Fatalf("valid aggregate reply=%q, dispatches=%d, error=%v", f.Payload, dispatched.Load(), err)
				}
			})
		}
	}
}
