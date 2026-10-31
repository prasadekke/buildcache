package protocol

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
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

// TestClient_GetHitWithInlineBody covers Response.Body/BodySize -- the
// mechanism internal/cas.InlineGetBodies and internal/cas/remote rely on
// so a "get" hit is self-contained instead of trusting a DiskPath that
// might live on a different machine.
func TestClient_GetHitWithInlineBody(t *testing.T) {
	bodyBytes := []byte("inline body bytes")
	h := &fakeHandler{
		getFunc: func(req *Request, resp *Response) error {
			resp.OutputID = []byte("out")
			resp.Size = int64(len(bodyBytes))
			resp.DiskPath = "/cache/x" // still set; Body takes precedence for a remote caller
			resp.Body = bytes.NewReader(bodyBytes)
			resp.BodyInline = true
			resp.BodySize = int64(len(bodyBytes))
			return nil
		},
	}
	client, wait := newClientServerPair(t, h)
	defer func() { client.Close(); wait() }()

	resp, err := client.Get([]byte("action"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.Body == nil {
		t.Fatal("resp.Body is nil, want the inline body")
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading resp.Body: %v", err)
	}
	if string(got) != string(bodyBytes) {
		t.Errorf("resp.Body = %q, want %q", got, bodyBytes)
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
	var srvErr *ServerError
	if !errors.As(err, &srvErr) {
		t.Errorf("Get err = %T, want a *ServerError", err)
	}
	if client.Err() != nil {
		t.Errorf("client.Err() = %v, want nil (a server-reported error leaves the connection healthy)", client.Err())
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
		serverConn.Write([]byte(`{"ID":0,"KnownCommands":["get","put","close"],"Version":1}` + "\n"))
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
		serverConn.Write([]byte(`{"ID":0,"KnownCommands":["get"],"Version":1}` + "\n"))
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

	// The stream is now in an unknown state: a second call must fail fast
	// with the original error rather than trying to read from it again.
	if client.Err() == nil {
		t.Fatal("client.Err() = nil after a transport error, want the client marked broken")
	}
	done := make(chan error, 1)
	go func() {
		_, err := client.Get([]byte("y"))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "unusable") {
			t.Fatalf("second Get err = %v, want a fail-fast 'connection unusable' error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Get blocked on a broken connection, want it to fail fast")
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

func TestNewClient_RejectsVersionMismatch(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	go serverConn.Write([]byte(`{"ID":0,"KnownCommands":["get","put","close"]}` + "\n")) // no Version: a pre-versioning server

	_, err := NewClient(clientConn)
	if err == nil || !strings.Contains(err.Error(), "protocol version") {
		t.Fatalf("NewClient err = %v, want a protocol version mismatch error", err)
	}
}

func TestClient_StoreDirAdvertised(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	go func() {
		srv := NewServer(serverConn, serverConn, &fakeHandler{})
		srv.StoreDir = "/var/cache/buildcache"
		srv.Run()
	}()
	client, err := NewClient(clientConn)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()
	if got := client.StoreDir(); got != "/var/cache/buildcache" {
		t.Errorf("StoreDir() = %q, want /var/cache/buildcache", got)
	}
}

func TestClient_ReadOnlyServer(t *testing.T) {
	h := &fakeHandler{}
	serverConn, clientConn := net.Pipe()
	done := make(chan error, 1)
	go func() {
		srv := NewServer(serverConn, serverConn, h)
		srv.ReadOnly = true
		done <- srv.Run()
	}()
	client, err := NewClient(clientConn)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if client.CanPut() {
		t.Error("CanPut() = true against a read-only server")
	}
	if _, err := client.Put([]byte("a"), []byte("b"), bytes.NewReader([]byte("x")), 1); err == nil {
		t.Error("Put to a read-only server succeeded, want it rejected")
	}
	if len(h.puts) != 0 {
		t.Errorf("read-only server's handler saw %d puts, want 0", len(h.puts))
	}
	if _, err := client.Get([]byte("a")); err != nil {
		t.Errorf("Get after a rejected Put: %v (the connection should still be usable)", err)
	}
	client.Close()
	<-done
}

// TestClient_LargeBodiesStream round-trips a body much larger than any of
// the bufio buffers involved, in both directions: a streamed "put" from an
// *os.File, and a streamed inline "get" body from one.
func TestClient_LargeBodiesStream(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 300_000) // ~4.8 MB
	path := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing big file: %v", err)
	}

	h := &fakeHandler{
		getFunc: func(req *Request, resp *Response) error {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			resp.OutputID = []byte("out")
			resp.Body = f
			resp.BodyInline = true
			resp.BodySize = int64(len(data))
			return nil
		},
	}
	client, wait := newClientServerPair(t, h)
	defer func() { client.Close(); wait() }()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	if _, err := client.Put([]byte("a"), []byte("out"), f, int64(len(data))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !bytes.Equal(h.putBodies[0], data) {
		t.Errorf("server received %d bytes, want the original %d", len(h.putBodies[0]), len(data))
	}

	resp, err := client.Get([]byte("a"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, data) {
		t.Errorf("inline body = %d bytes, want the original %d", len(got), len(data))
	}
}

// TestServer_BadInlineBodyFailsOnlyThatRequest: an inline body whose size
// doesn't match what the handler declared is caught before anything hits
// the wire, so it becomes an error response for that one request instead
// of ending the session.
func TestServer_BadInlineBodyFailsOnlyThatRequest(t *testing.T) {
	h := &fakeHandler{
		getFunc: func(req *Request, resp *Response) error {
			resp.OutputID = []byte("out")
			resp.Body = bytes.NewReader([]byte("short"))
			resp.BodyInline = true
			resp.BodySize = 999
			return nil
		},
	}
	client, wait := newClientServerPair(t, h)

	if _, err := client.Get([]byte("a")); err == nil {
		t.Fatal("Get err = nil, want the server's body error")
	}
	if client.Err() != nil {
		t.Fatalf("client marked broken (%v), want the connection still healthy", client.Err())
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := wait(); err != nil {
		t.Fatalf("Server.Run: %v", err)
	}
}

func TestServer_MaxBodySize(t *testing.T) {
	h := &fakeHandler{}
	var in bytes.Buffer
	in.Write(encodeLikeGoCommand(t, Request{ID: 1, Command: CmdPut, ActionID: []byte("a"), OutputID: []byte("b"), Body: bytes.NewReader(make([]byte, 100)), BodySize: 100}))

	var out bytes.Buffer
	s := NewServer(&in, &out, h)
	s.MaxBodySize = 10
	if err := s.Run(); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("Run err = %v, want a body size limit error", err)
	}
	if len(h.puts) != 0 {
		t.Errorf("handler saw %d puts, want 0", len(h.puts))
	}
}

func TestServer_ControlLineTooLong(t *testing.T) {
	h := &fakeHandler{}
	in := strings.NewReader(strings.Repeat("x", maxControlLine+1) + "\n")
	var out bytes.Buffer
	s := NewServer(in, &out, h)
	if err := s.Run(); err == nil || !errors.Is(err, errLineTooLong) {
		t.Fatalf("Run err = %v, want errLineTooLong", err)
	}
}

func TestDecodeBodyLine_BoundedByDeclaredSize(t *testing.T) {
	// A peer declaring a 3-byte body but sending a megabyte-long line must
	// be cut off, not buffered.
	line := `"` + strings.Repeat("A", 1<<20) + `"` + "\n"
	if _, err := decodeBodyLine(bufio.NewReader(strings.NewReader(line)), 3); !errors.Is(err, errLineTooLong) {
		t.Fatalf("decodeBodyLine err = %v, want errLineTooLong", err)
	}
}
