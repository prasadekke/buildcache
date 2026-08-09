package protocol

import (
	"bytes"
	"errors"
	"net"
	"testing"
	"time"
)

// newClientServerPair starts h being served over one end of an in-memory
// full-duplex pipe and returns a ready Client wired to the other end, plus
// a function to wait for Server.Run to finish and report its error.
func newClientServerPair(t *testing.T, h Handler) (*Client, func() error) {
	t.Helper()
	serverConn, clientConn := net.Pipe()

	done := make(chan error, 1)
	go func() {
		srv := NewServer(serverConn, serverConn, h)
		done <- srv.Run()
	}()

	client, err := NewClient(clientConn)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client, func() error {
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("Server.Run did not return in time")
			return nil
		}
	}
}

func TestClient_Handshake(t *testing.T) {
	h := &fakeHandler{}
	client, wait := newClientServerPair(t, h)

	if !client.CanGet() || !client.CanPut() {
		t.Errorf("CanGet()=%v CanPut()=%v, want both true", client.CanGet(), client.CanPut())
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := wait(); err != nil {
		t.Fatalf("Server.Run: %v", err)
	}
}

func TestClient_GetHit(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	h := &fakeHandler{
		getFunc: func(req *Request, resp *Response) error {
			resp.OutputID = []byte("out")
			resp.Size = 7
			resp.DiskPath = "/cache/x"
			resp.Time = &now
			return nil
		},
	}
	client, wait := newClientServerPair(t, h)
	defer func() { client.Close(); wait() }()

	resp, err := client.Get([]byte("action"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.Miss || string(resp.OutputID) != "out" || resp.Size != 7 || resp.DiskPath != "/cache/x" {
		t.Errorf("resp = %+v, want a hit with OutputID=out Size=7 DiskPath=/cache/x", resp)
	}
}

func TestClient_GetMiss(t *testing.T) {
	h := &fakeHandler{} // default getFunc sets Miss=true
	client, wait := newClientServerPair(t, h)
	defer func() { client.Close(); wait() }()

	resp, err := client.Get([]byte("nope"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !resp.Miss {
		t.Errorf("resp.Miss = false, want true")
	}
}

func TestClient_Put(t *testing.T) {
	h := &fakeHandler{
		putFunc: func(req *Request, resp *Response) error {
			resp.DiskPath = "/cache/new"
			return nil
		},
	}
	client, wait := newClientServerPair(t, h)
	defer func() { client.Close(); wait() }()

	body := []byte("hello world")
	resp, err := client.Put([]byte("action"), []byte("output"), bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if resp.DiskPath != "/cache/new" {
		t.Errorf("resp.DiskPath = %q, want /cache/new", resp.DiskPath)
	}
	if len(h.puts) != 1 || string(h.putBodies[0]) != "hello world" {
		t.Errorf("server received puts=%+v bodies=%q, want one put with body %q", h.puts, h.putBodies, "hello world")
	}
}

func TestClient_PutZeroByteBody(t *testing.T) {
	var gotNil bool
	h := &fakeHandler{
		putFunc: func(req *Request, resp *Response) error {
			gotNil = req.Body == nil
			resp.DiskPath = "/cache/empty"
			return nil
		},
	}
	client, wait := newClientServerPair(t, h)
	defer func() { client.Close(); wait() }()

	if _, err := client.Put([]byte("a"), []byte("b"), nil, 0); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if gotNil {
		t.Error("server's Handler.Put saw a nil Body for a zero-byte put")
	}
}

func TestClient_HandlerErrorSurfacesAsGoError(t *testing.T) {
	wantErr := errors.New("disk exploded")
	h := &fakeHandler{getFunc: func(req *Request, resp *Response) error { return wantErr }}
	client, wait := newClientServerPair(t, h)
	defer func() { client.Close(); wait() }()

	_, err := client.Get([]byte("x"))
	if err == nil || err.Error() != wantErr.Error() {
		t.Fatalf("Get err = %v, want %v", err, wantErr)
	}
}

// TestClient_PutBodySizeMismatch is a regression test: a size mismatch
// must be caught before anything is written to the connection. Catching it
// only after the request line was already sent would leave the server
// blocked reading a body line that never arrives, wedging every
// subsequent call on the connection -- including the Close in this test's
// own cleanup, which is exactly what this test would deadlock on if that
// regressed.
func TestClient_PutBodySizeMismatch(t *testing.T) {
	h := &fakeHandler{}
	client, wait := newClientServerPair(t, h)

	_, err := client.Put([]byte("a"), []byte("b"), bytes.NewReader([]byte("short")), 999)
	if err == nil {
		t.Fatal("Put err = nil, want a body size mismatch error")
	}

	// The connection must still be usable: a clean Get right after the
	// failed Put, then a clean Close, prove nothing was left desynced.
	resp, err := client.Get([]byte("x"))
	if err != nil {
		t.Fatalf("Get after a failed Put: %v", err)
	}
	if !resp.Miss {
		t.Errorf("Get after a failed Put: resp = %+v, want a clean miss", resp)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close after a failed Put: %v", err)
	}
	if err := wait(); err != nil {
		t.Fatalf("Server.Run: %v", err)
	}
}

func TestNewClient_BadStartupID(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	go serverConn.Write([]byte(`{"ID":5,"KnownCommands":["get"]}` + "\n"))

	if _, err := NewClient(clientConn); err == nil {
		t.Fatal("NewClient err = nil, want an error for a startup response with a non-zero ID")
	}
}

func TestNewClient_NoKnownCommands(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	go serverConn.Write([]byte(`{"ID":0}` + "\n"))

	if _, err := NewClient(clientConn); err == nil {
		t.Fatal("NewClient err = nil, want an error when the server declares no commands")
	}
}

func TestNewClient_ReadError(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	serverConn.Close() // already broken before the client ever reads

	if _, err := NewClient(clientConn); err == nil {
		t.Fatal("NewClient err = nil, want an error when the connection is already closed")
	}
}

func TestClient_MismatchedResponseID(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	go func() {
		serverConn.Write([]byte(`{"ID":0,"KnownCommands":["get","put","close"]}` + "\n"))
		// net.Pipe is fully synchronous with no buffering: drain the
		// client's incoming Get request before writing back, otherwise
		// both ends end up blocked in Write with nobody reading.
		buf := make([]byte, 4096)
		serverConn.Read(buf)
		serverConn.Write([]byte(`{"ID":999,"Miss":true}` + "\n")) // wrong ID for the Get above
	}()

	client, err := NewClient(clientConn)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.Get([]byte("x")); err == nil {
		t.Fatal("Get err = nil, want an error for a mismatched response ID")
	}
}

func TestClient_MalformedResponse(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	go func() {
		serverConn.Write([]byte(`{"ID":0,"KnownCommands":["get"]}` + "\n"))
		buf := make([]byte, 4096)
		serverConn.Read(buf) // drain the client's Get request; see TestClient_MismatchedResponseID
		serverConn.Write([]byte("not json\n"))
	}()

	client, err := NewClient(clientConn)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.Get([]byte("x")); err == nil {
		t.Fatal("Get err = nil, want a decode error")
	}
}

// closeErrConn wraps a net.Conn to make Close report a caller-chosen error
// after still actually closing the underlying connection.
type closeErrConn struct {
	net.Conn
	closeErr error
}

func (c *closeErrConn) Close() error {
	c.Conn.Close()
	return c.closeErr
}

func TestClient_CloseConnError(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	go func() {
		srv := NewServer(serverConn, serverConn, &fakeHandler{})
		srv.Run()
	}()

	wrapped := &closeErrConn{Conn: clientConn, closeErr: errors.New("close failed")}
	client, err := NewClient(wrapped)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.Close(); err == nil {
		t.Fatal("Close err = nil, want the wrapped conn's close error surfaced")
	}
}
