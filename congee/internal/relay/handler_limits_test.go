package relay

import (
	"bytes"
	"compress/flate"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsflate"
	"github.com/gobwas/ws/wsutil"
)

type countedDecompressor struct {
	io.ReadCloser
	read *int
}

func (r *countedDecompressor) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	*r.read += n
	return n, err
}

func TestReadOneFlateTextBoundsExpansion(t *testing.T) {
	const limit = 4096
	for _, size := range []int{limit - 1, limit, 1 << 20} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			payload := bytes.Repeat([]byte("a"), size)
			var compressed bytes.Buffer
			compressor, err := flate.NewWriter(&compressed, flate.DefaultCompression)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := compressor.Write(payload); err != nil {
				t.Fatal(err)
			}
			if err := compressor.Flush(); err != nil {
				t.Fatal(err)
			}
			// RFC 7692 removes the sync-flush tail, before closing the stream.
			encoded := bytes.Clone(compressed.Bytes())
			if err := compressor.Close(); err != nil {
				t.Fatal(err)
			}
			if len(encoded) < 4 || !bytes.Equal(encoded[len(encoded)-4:], []byte{0, 0, 255, 255}) {
				t.Fatal("fixture lacks the per-message DEFLATE sync-flush tail")
			}
			frame := ws.NewTextFrame(encoded[:len(encoded)-4])
			frame.Header, err = wsflate.SetBit(frame.Header)
			if err != nil {
				t.Fatal(err)
			}
			if len(frame.Payload) >= limit {
				t.Fatal("fixture must fit compressed frame limit")
			}
			client, server := net.Pipe()
			t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
			if err := server.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			go func() { _ = ws.WriteFrame(client, ws.MaskFrame(frame)); _ = client.Close() }()
			read := 0
			fr := wsflate.NewReader(nil, func(r io.Reader) wsflate.Decompressor {
				return &countedDecompressor{ReadCloser: flate.NewReader(r), read: &read}
			})
			t.Cleanup(func() { _ = fr.Close() })
			var msg wsflate.MessageState
			rd := wsutil.Reader{Source: server, State: ws.StateServerSide | ws.StateExtended, MaxFrameSize: limit, Extensions: []wsutil.RecvExtension{&msg}}
			got, err := readOneFlateText(server, &rd, fr, &msg, limit)
			if size > limit {
				if !errors.Is(err, wsutil.ErrFrameTooLarge) {
					t.Fatalf("oversized expansion error = %v", err)
				}
				if read != limit+1 {
					t.Fatalf("decompressed %d bytes, want bounded probe of %d", read, limit+1)
				}
				return
			}
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("valid payload size %d: got %d bytes, error %v", size, len(got), err)
			}
		})
	}
}

type countedFrameConn struct {
	net.Conn
	reader *bytes.Reader
	read   int
}

func (c *countedFrameConn) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	c.read += n
	return n, err
}

func TestReadNextTextMessageBoundsFragmentedPayload(t *testing.T) {
	var wire bytes.Buffer
	for i := 0; i < 3; i++ {
		op := ws.OpContinuation
		if i == 0 {
			op = ws.OpText
		}
		frame := ws.MaskFrame(ws.NewFrame(op, i == 2, bytes.Repeat([]byte("a"), 600)))
		if err := ws.WriteFrame(&wire, frame); err != nil {
			t.Fatal(err)
		}
	}
	c := &countedFrameConn{reader: bytes.NewReader(wire.Bytes())}
	if _, err := readNextTextMessage(c, 1024); !errors.Is(err, wsutil.ErrFrameTooLarge) {
		t.Fatalf("oversized fragmented message error = %v", err)
	}
	if c.read >= wire.Len() {
		t.Fatalf("read all %d bytes before rejecting message", c.read)
	}
}

type terminalWriteFailureConn struct {
	net.Conn
	deadlineFails bool
	readStarted   chan struct{}
	readOnce      sync.Once
}

func (c *terminalWriteFailureConn) Read(p []byte) (int, error) {
	c.readOnce.Do(func() { close(c.readStarted) })
	return c.Conn.Read(p)
}

func (c *terminalWriteFailureConn) SetWriteDeadline(t time.Time) error {
	if c.deadlineFails {
		return errors.New("test write deadline failure")
	}
	return c.Conn.SetWriteDeadline(t)
}

func (c *terminalWriteFailureConn) Write([]byte) (int, error) {
	return 0, errors.New("test terminal write failure")
}

func TestWriteLoopFailureShutsDownReadLoop(t *testing.T) {
	for _, tt := range []struct {
		name          string
		deadlineFails bool
		compressed    bool
	}{
		{name: "plain/write"},
		{name: "plain/deadline", deadlineFails: true},
		{name: "flate/write", compressed: true},
		{name: "flate/deadline", compressed: true, deadlineFails: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, peer, cleanup := testConnWithPipe(t)
			t.Cleanup(cleanup)
			if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			nc := &terminalWriteFailureConn{Conn: c.nc, deadlineFails: tt.deadlineFails, readStarted: make(chan struct{})}
			c.nc = nc
			readDone := make(chan struct{})
			go func() {
				if tt.compressed {
					c.readLoopFlate()
				} else {
					c.readLoopPlain()
				}
				close(readDone)
			}()
			select {
			case <-nc.readStarted:
			case <-time.After(2 * time.Second):
				t.Fatal("read loop did not start")
			}
			go c.writeLoop()
			if err := c.enqueue([]byte("reply")); err != nil {
				t.Fatal(err)
			}
			if !c.waitWriterDone(2 * time.Second) {
				t.Fatal("writer did not exit after terminal failure")
			}
			select {
			case <-c.ctx.Done():
			default:
				t.Fatal("terminal writer failure did not cancel connection")
			}
			select {
			case <-readDone:
			case <-time.After(2 * time.Second):
				t.Fatal("terminal writer failure left reader blocked")
			}
			if err := c.enqueue([]byte("late reply")); !errors.Is(err, ErrSlowConsumer) {
				t.Fatalf("outbound channel remains open: %v", err)
			}
			var b [1]byte
			if _, err := peer.Read(b[:]); !errors.Is(err, io.EOF) {
				t.Fatalf("terminal writer failure did not close socket: %v", err)
			}
		})
	}
}
