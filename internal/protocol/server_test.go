package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// fakeHandler is a Handler test double that records every call and lets
// each test control the response.
type fakeHandler struct {
	gets      []Request
	puts      []Request
	putBodies [][]byte

	getFunc func(*Request, *Response) error
	putFunc func(*Request, *Response) error
}

func (f *fakeHandler) Get(req *Request, resp *Response) error {
	f.gets = append(f.gets, *req)
	if f.getFunc != nil {
		return f.getFunc(req, resp)
	}
	resp.Miss = true
	return nil
}

func (f *fakeHandler) Put(req *Request, resp *Response) error {
	f.puts = append(f.puts, *req)
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return err
		}
		body = b
	}
	f.putBodies = append(f.putBodies, body)
	if f.putFunc != nil {
		return f.putFunc(req, resp)
	}
	resp.DiskPath = "/fake/disk/path"
	return nil
}

// encodeLikeGoCommand reproduces the exact byte-level framing
// cmd/go/internal/cache.(*ProgCache).writeToChild uses: a JSON request line
// via json.Encoder.Encode (which appends its own trailing "\n"), followed by
// an *unconditional extra* "\n", followed -- only when BodySize > 0 -- by a
// quoted base64 body line with a single trailing "\n" of its own.
//
// This matters because every earlier check of this package, including the
// hand-rolled clients used to smoke-test cacheprog/cacheserver by hand, used
// exactly one "\n" per message and never reproduced the real go command's
// extra blank line -- which is exactly what let the pre-close-EOF bug ship
// invisibly. See readLine's doc comment for the fix.
func encodeLikeGoCommand(t *testing.T, req Request) []byte {
	t.Helper()
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("reading request body for encoding: %v", err)
		}
		body = b
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(req); err != nil {
		t.Fatalf("encoding request: %v", err)
	}
	buf.WriteByte('\n') // the go command's extra, unconditional blank line

	if req.BodySize > 0 {
		buf.WriteByte('"')
		b64 := base64.NewEncoder(base64.StdEncoding, &buf)
		if _, err := b64.Write(body); err != nil {
			t.Fatalf("base64-encoding body: %v", err)
		}
		if err := b64.Close(); err != nil {
			t.Fatalf("closing base64 encoder: %v", err)
		}
		buf.WriteString("\"\n")
	}
	return buf.Bytes()
}

func decodeResponses(t *testing.T, r io.Reader) []Response {
	t.Helper()
	var out []Response
	dec := json.NewDecoder(r)
	for {
		var resp Response
		if err := dec.Decode(&resp); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decoding response stream: %v", err)
		}
		out = append(out, resp)
	}
	return out
}

func TestServer_StartupAnnouncement(t *testing.T) {
	h := &fakeHandler{}
	var out bytes.Buffer
	s := NewServer(strings.NewReader(""), &out, h)
	if err := s.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	resps := decodeResponses(t, &out)
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1 (startup only)", len(resps))
	}
	if resps[0].ID != 0 {
		t.Errorf("startup response ID = %d, want 0", resps[0].ID)
	}
	want := []Cmd{CmdGet, CmdPut, CmdClose}
	if len(resps[0].KnownCommands) != len(want) {
		t.Fatalf("KnownCommands = %v, want %v", resps[0].KnownCommands, want)
	}
	for i, c := range want {
		if resps[0].KnownCommands[i] != c {
			t.Errorf("KnownCommands[%d] = %q, want %q", i, resps[0].KnownCommands[i], c)
		}
	}
}

// TestServer_RealGoCommandFraming is a regression test for the bug where the
// server treated the go command's extra blank line after every request as
// end-of-stream and exited after the very first request -- silently
// breaking every real build while every hand-written protocol test passed,
// because none of them reproduced the real framing.
func TestServer_RealGoCommandFraming(t *testing.T) {
	h := &fakeHandler{}

	var in bytes.Buffer
	in.Write(encodeLikeGoCommand(t, Request{ID: 1, Command: CmdGet, ActionID: []byte("action-one")}))
	in.Write(encodeLikeGoCommand(t, Request{ID: 2, Command: CmdGet, ActionID: []byte("action-two")}))
	body := []byte("hello world")
	in.Write(encodeLikeGoCommand(t, Request{
		ID: 3, Command: CmdPut,
		ActionID: []byte("action-three"), OutputID: []byte("output-three"),
		Body: bytes.NewReader(body), BodySize: int64(len(body)),
	}))
	in.Write(encodeLikeGoCommand(t, Request{ID: 4, Command: CmdClose}))

	var out bytes.Buffer
	s := NewServer(&in, &out, h)
	if err := s.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(h.gets) != 2 {
		t.Fatalf("got %d Get calls, want 2 (server likely stopped early on the blank line)", len(h.gets))
	}
	if len(h.puts) != 1 {
		t.Fatalf("got %d Put calls, want 1", len(h.puts))
	}
	if string(h.puts[0].ActionID) != "action-three" {
		t.Errorf("put ActionID = %q, want %q", h.puts[0].ActionID, "action-three")
	}
	if got := string(h.putBodies[0]); got != "hello world" {
		t.Errorf("put body = %q, want %q", got, "hello world")
	}

	resps := decodeResponses(t, &out)
	// startup + 2 gets + 1 put + 1 close ack = 5
	if len(resps) != 5 {
		t.Fatalf("got %d responses, want 5; responses: %+v", len(resps), resps)
	}
	if resps[len(resps)-1].ID != 4 {
		t.Errorf("final response ID = %d, want 4 (close ack)", resps[len(resps)-1].ID)
	}
}

// TestServer_ZeroByteBodyPut is a regression test: a "put" with
// BodySize == 0 is a legitimate empty cache object (the go command sends no
// body line for it), but the server used to leave Request.Body nil in this
// case, which panicked downstream (disk.Store.Put -> io.Copy on a nil
// io.Reader) the moment any real build tried to cache a zero-byte action.
func TestServer_ZeroByteBodyPut(t *testing.T) {
	var gotBodyNil bool
	gotBodyLen := -1
	h := &fakeHandler{
		putFunc: func(req *Request, resp *Response) error {
			gotBodyNil = req.Body == nil
			if req.Body != nil {
				b, err := io.ReadAll(req.Body)
				if err != nil {
					return err
				}
				gotBodyLen = len(b)
			}
			resp.DiskPath = "/fake/empty"
			return nil
		},
	}

	var in bytes.Buffer
	in.Write(encodeLikeGoCommand(t, Request{ID: 1, Command: CmdPut, ActionID: []byte("empty-action"), OutputID: []byte("empty-output"), BodySize: 0}))
	in.Write(encodeLikeGoCommand(t, Request{ID: 2, Command: CmdClose}))

	var out bytes.Buffer
	s := NewServer(&in, &out, h)
	if err := s.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if gotBodyNil {
		t.Fatal("Handler.Put received a nil Body for a zero-byte put; want a valid empty io.Reader")
	}
	if gotBodyLen != 0 {
		t.Errorf("Handler.Put body length = %d, want 0", gotBodyLen)
	}
}

func TestServer_GetHitAndMiss(t *testing.T) {
	h := &fakeHandler{
		getFunc: func(req *Request, resp *Response) error {
			if string(req.ActionID) == "known" {
				resp.OutputID = []byte("out")
				resp.Size = 42
				resp.DiskPath = "/cache/known"
				return nil
			}
			resp.Miss = true
			return nil
		},
	}

	var in bytes.Buffer
	in.Write(encodeLikeGoCommand(t, Request{ID: 1, Command: CmdGet, ActionID: []byte("known")}))
	in.Write(encodeLikeGoCommand(t, Request{ID: 2, Command: CmdGet, ActionID: []byte("unknown")}))
	in.Write(encodeLikeGoCommand(t, Request{ID: 3, Command: CmdClose}))

	var out bytes.Buffer
	s := NewServer(&in, &out, h)
	if err := s.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	resps := decodeResponses(t, &out)
	if len(resps) != 4 {
		t.Fatalf("got %d responses, want 4", len(resps))
	}
	hit, miss := resps[1], resps[2]
	if hit.Miss || hit.DiskPath != "/cache/known" || hit.Size != 42 {
		t.Errorf("hit response = %+v, want DiskPath=/cache/known Size=42 Miss=false", hit)
	}
	if !miss.Miss {
		t.Errorf("miss response = %+v, want Miss=true", miss)
	}
}

// TestServer_EOFWithoutClose covers a client that hangs up mid-session
// without ever sending "close" -- an abruptly killed go command, or a
// cacheprog-to-cacheserver connection dropped mid-build. Run must return
// cleanly in that case, not treat it as a protocol violation.
func TestServer_EOFWithoutClose(t *testing.T) {
	h := &fakeHandler{}
	var in bytes.Buffer
	in.Write(encodeLikeGoCommand(t, Request{ID: 1, Command: CmdGet, ActionID: []byte("x")}))
	// no close request -- just EOF

	var out bytes.Buffer
	s := NewServer(&in, &out, h)
	if err := s.Run(); err != nil {
		t.Fatalf("Run: %v, want nil (clean EOF)", err)
	}
	if len(h.gets) != 1 {
		t.Fatalf("got %d Get calls, want 1", len(h.gets))
	}
}

func TestServer_UnsupportedCommand(t *testing.T) {
	h := &fakeHandler{}
	var in bytes.Buffer
	in.Write(encodeLikeGoCommand(t, Request{ID: 1, Command: Cmd("bogus")}))
	in.Write(encodeLikeGoCommand(t, Request{ID: 2, Command: CmdClose}))

	var out bytes.Buffer
	s := NewServer(&in, &out, h)
	if err := s.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	resps := decodeResponses(t, &out)
	if len(resps) != 3 {
		t.Fatalf("got %d responses, want 3", len(resps))
	}
	if resps[1].Err == "" {
		t.Errorf("response for unsupported command has no Err set: %+v", resps[1])
	}
}

func TestServer_HandlerErrorDoesNotStopLoop(t *testing.T) {
	callErr := errors.New("boom")
	calls := 0
	h := &fakeHandler{
		getFunc: func(req *Request, resp *Response) error {
			calls++
			if calls == 1 {
				return callErr
			}
			resp.Miss = true
			return nil
		},
	}

	var in bytes.Buffer
	in.Write(encodeLikeGoCommand(t, Request{ID: 1, Command: CmdGet, ActionID: []byte("a")}))
	in.Write(encodeLikeGoCommand(t, Request{ID: 2, Command: CmdGet, ActionID: []byte("b")}))
	in.Write(encodeLikeGoCommand(t, Request{ID: 3, Command: CmdClose}))

	var out bytes.Buffer
	s := NewServer(&in, &out, h)
	if err := s.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	resps := decodeResponses(t, &out)
	if len(resps) != 4 {
		t.Fatalf("got %d responses, want 4", len(resps))
	}
	if resps[1].Err != callErr.Error() {
		t.Errorf("response[1].Err = %q, want %q", resps[1].Err, callErr.Error())
	}
	if resps[2].Err != "" || !resps[2].Miss {
		t.Errorf("response[2] = %+v, want a clean miss (loop should continue after a handler error)", resps[2])
	}
}

func TestServer_MalformedRequestLine(t *testing.T) {
	h := &fakeHandler{}
	in := strings.NewReader("not json\n\n")
	var out bytes.Buffer
	s := NewServer(in, &out, h)
	if err := s.Run(); err == nil {
		t.Fatal("Run: got nil error, want a decode error for a malformed request line")
	}
}

func TestServer_BodySizeMismatch(t *testing.T) {
	h := &fakeHandler{}
	var in bytes.Buffer
	// Hand-craft a put whose declared BodySize doesn't match the actual body.
	req := Request{ID: 1, Command: CmdPut, ActionID: []byte("a"), OutputID: []byte("b"), BodySize: 999}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshaling request: %v", err)
	}
	in.Write(b)
	in.WriteByte('\n')
	in.WriteByte('\n')
	encoded, err := json.Marshal(base64.StdEncoding.EncodeToString([]byte("short")))
	if err != nil {
		t.Fatalf("marshaling body: %v", err)
	}
	in.Write(encoded)
	in.WriteByte('\n')

	var out bytes.Buffer
	s := NewServer(&in, &out, h)
	if err := s.Run(); err == nil {
		t.Fatal("Run: got nil error, want a body size mismatch error")
	}
}

// TestServer_ManyConsecutiveBlankLines checks readLine tolerates more than
// one blank line in a row, not just exactly the one the go command
// currently happens to send -- defensive against any future padding.
func TestServer_ManyConsecutiveBlankLines(t *testing.T) {
	h := &fakeHandler{}
	var in bytes.Buffer
	in.Write(encodeLikeGoCommand(t, Request{ID: 1, Command: CmdGet, ActionID: []byte("a")}))
	in.WriteString("\n\n\n") // extra padding beyond the one real blank line
	in.Write(encodeLikeGoCommand(t, Request{ID: 2, Command: CmdClose}))

	var out bytes.Buffer
	s := NewServer(&in, &out, h)
	if err := s.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(h.gets) != 1 {
		t.Fatalf("got %d Get calls, want 1", len(h.gets))
	}
}

// errReader wraps r and substitutes err for the io.EOF r eventually returns,
// simulating a connection that dies (broken pipe, reset, etc.) rather than
// closing cleanly.
type errReader struct {
	r   io.Reader
	err error
}

func (e *errReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		return n, e.err
	}
	return n, err
}

func TestServer_ReadRequestLineIOError(t *testing.T) {
	h := &fakeHandler{}
	wantErr := errors.New("connection reset")
	in := &errReader{r: strings.NewReader("partial line, no newline, then the connection dies"), err: wantErr}

	var out bytes.Buffer
	s := NewServer(in, &out, h)
	err := s.Run()
	if err == nil || !strings.Contains(err.Error(), wantErr.Error()) {
		t.Fatalf("Run() err = %v, want it to wrap %v", err, wantErr)
	}
}

func TestServer_ReadBodyLineIOError(t *testing.T) {
	// The request line and its mandatory blank line arrive intact, but the
	// connection dies before the body line does -- must surface as a
	// "reading body line" error, not be swallowed as a clean EOF.
	h := &fakeHandler{}
	wantErr := errors.New("connection reset")

	req := Request{ID: 1, Command: CmdPut, ActionID: []byte("a"), OutputID: []byte("b"), BodySize: 5}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshaling request: %v", err)
	}
	var prefix bytes.Buffer
	prefix.Write(b)
	prefix.WriteByte('\n')
	prefix.WriteByte('\n') // the go command's extra blank line

	in := &errReader{r: bytes.NewReader(prefix.Bytes()), err: wantErr}
	var out bytes.Buffer
	s := NewServer(in, &out, h)
	runErr := s.Run()
	if runErr == nil || !strings.Contains(runErr.Error(), "reading body line") {
		t.Fatalf("Run() err = %v, want it to mention %q", runErr, "reading body line")
	}
}

// errWriter always fails; flakyWriter fails only after a configured number
// of successful writes -- together they cover writeResponse's error path at
// both the startup announcement and a later response.
type errWriter struct{ err error }

func (w *errWriter) Write(p []byte) (int, error) { return 0, w.err }

type flakyWriter struct {
	succeedCalls int
	err          error
	buf          bytes.Buffer
}

func (w *flakyWriter) Write(p []byte) (int, error) {
	if w.succeedCalls <= 0 {
		return 0, w.err
	}
	w.succeedCalls--
	return w.buf.Write(p)
}

func TestServer_StartupWriteError(t *testing.T) {
	wantErr := errors.New("stdout closed")
	h := &fakeHandler{}
	s := NewServer(strings.NewReader(""), &errWriter{err: wantErr}, h)
	err := s.Run()
	if err == nil || !strings.Contains(err.Error(), "writing startup response") {
		t.Fatalf("Run() err = %v, want it to mention %q", err, "writing startup response")
	}
}

func TestServer_ResponseWriteError(t *testing.T) {
	wantErr := errors.New("stdout closed mid-stream")
	h := &fakeHandler{}
	var in bytes.Buffer
	in.Write(encodeLikeGoCommand(t, Request{ID: 1, Command: CmdGet, ActionID: []byte("a")}))

	out := &flakyWriter{succeedCalls: 1, err: wantErr} // startup response succeeds, the Get response doesn't
	s := NewServer(&in, out, h)
	err := s.Run()
	if err == nil || !strings.Contains(err.Error(), "writing response") {
		t.Fatalf("Run() err = %v, want it to mention %q", err, "writing response")
	}
}
