package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Handler implements the actual cache backend logic against parsed
// requests. Implementations should be safe for concurrent use — see the
// note on Server.Run about the current single-threaded dispatch loop.
type Handler interface {
	// Get looks up req.ActionID and populates resp (Miss, OutputID,
	// DiskPath, Size, Time) accordingly, or returns an error.
	Get(req *Request, resp *Response) error

	// Put stores req.Body (req.BodySize bytes) under req.ActionID /
	// req.OutputID and populates resp.DiskPath with the local file it was
	// written to.
	Put(req *Request, resp *Response) error
}

// Server runs the GOCACHEPROG read/dispatch/write loop.
type Server struct {
	Handler Handler

	// StoreDir, if set, is advertised in the startup message (see
	// Response.StoreDir). Leave it empty unless the peer is on the same
	// machine and could make use of it.
	StoreDir string

	// ReadOnly makes the server advertise only "get" and "close", and
	// reject any "put" it receives anyway. The go command already treats a
	// server without "put" as a read-only cache; cacheserver uses this for
	// mTLS clients whose certificate only grants read access.
	ReadOnly bool

	// MaxBodySize, if > 0, is the largest "put" body the server will
	// accept. A larger declared size ends the session with an error rather
	// than buffering the body: the body line can't be skipped without
	// reading it, so there's no way to reject just that one request.
	MaxBodySize int64

	in    *bufio.Reader
	out   io.Writer
	outMu sync.Mutex
}

// NewServer wires a Server to the given transport. In production, in and
// out are os.Stdin and os.Stdout.
func NewServer(in io.Reader, out io.Writer, h Handler) *Server {
	return &Server{
		Handler: h,
		in:      bufio.NewReaderSize(in, 1<<20),
		out:     out,
	}
}

// Run announces this program's capabilities, then services requests until
// stdin closes or a "close" request is handled.
//
// This skeleton is deliberately single-threaded: requests are read,
// answered, and the next one read, strictly in order. That's a spec-correct
// implementation — Response.ID still ties each reply to its request even
// though the protocol technically allows answering out of order — but not a
// concurrent one. Once the backend is a network CAS instead of local disk,
// a slow "get" will stall an unrelated "put" behind it; dispatching each
// request onto its own goroutine (with outMu already in place to guard
// concurrent writes) is the natural next step and is left as a TODO here on
// purpose rather than guessed at prematurely.
func (s *Server) Run() error {
	known := []Cmd{CmdGet, CmdPut, CmdClose}
	if s.ReadOnly {
		known = []Cmd{CmdGet, CmdClose}
	}
	startup := &Response{ID: 0, KnownCommands: known, Version: ProtocolVersion, StoreDir: s.StoreDir}
	if err := s.writeResponse(startup); err != nil {
		return fmt.Errorf("writing startup response: %w", err)
	}

	for {
		req, err := s.readRequest()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading request: %w", err)
		}

		resp := &Response{ID: req.ID}
		switch req.Command {
		case CmdGet:
			err = s.Handler.Get(req, resp)
		case CmdPut:
			if s.ReadOnly {
				err = errors.New("put rejected: this connection is read-only")
			} else {
				err = s.Handler.Put(req, resp)
			}
		case CmdClose:
			return s.writeResponse(resp) // ack, then exit per protocol
		default:
			err = fmt.Errorf("unsupported command %q", req.Command)
		}
		if err != nil {
			resp.Err = err.Error()
		}
		if err := s.writeResponse(resp); err != nil {
			return fmt.Errorf("writing response: %w", err)
		}
	}
}

// readRequest reads one JSON request line, and for a "put" with a nonzero
// body size, the base64 body line that immediately follows it.
func (s *Server) readRequest() (*Request, error) {
	line, err := s.readLine()
	if err != nil {
		return nil, err
	}

	var req Request
	if jsonErr := json.Unmarshal(line, &req); jsonErr != nil {
		return nil, fmt.Errorf("decoding request line: %w", jsonErr)
	}

	if req.Command == CmdPut {
		if req.BodySize < 0 {
			return nil, fmt.Errorf("put request %d: negative BodySize %d", req.ID, req.BodySize)
		}
		if s.MaxBodySize > 0 && req.BodySize > s.MaxBodySize {
			return nil, fmt.Errorf("put request %d: BodySize %d exceeds the %d-byte limit", req.ID, req.BodySize, s.MaxBodySize)
		}
		if req.BodySize == 0 {
			// No body line follows for a zero-byte body (see readLine's doc
			// comment), but the body is still a legitimate empty object,
			// not a missing one -- give Store.Put a valid empty io.Reader
			// rather than leaving req.Body nil.
			req.Body = bytes.NewReader(nil)
		} else {
			body, err := decodeBodyLine(s.in, req.BodySize)
			if err != nil {
				return nil, err
			}
			req.Body = body
		}
	}

	return &req, nil
}

// readLine reads the next non-blank line from the input stream. The go
// command's ProgCache writes an extra unconditional "\n" after every
// request's JSON line, on top of the one json.Encoder.Encode already
// appends (see cmd/go/internal/cache/prog.go's writeToChild). That leaves a
// blank line between every request -- and, for a "put", between the request
// and its body line too -- which is protocol noise to skip, not a signal
// that the stream has ended. See readNonBlankLine for the actual skipping
// logic, shared with Client since Client talks to a Server too.
func (s *Server) readLine() ([]byte, error) {
	return readNonBlankLine(s.in, maxControlLine)
}

// writeResponse writes resp's JSON line and, if resp.BodyInline is true (see
// Response.Body's doc comment), the inline body line immediately after it
// -- both under the same lock, so a concurrent writeResponse from another
// goroutine can never interleave its own line in between the two. The body
// is streamed rather than encoded into memory first; if resp.Body is an
// io.Closer (cas.InlineGetBodies hands over an open file), it's closed once
// written.
//
// A body that turns out to be unusable before anything is written (wrong
// size, unreadable) is reported to the peer as an error response for that
// one request instead of ending the whole session.
func (s *Server) writeResponse(resp *Response) error {
	if c, ok := resp.Body.(io.Closer); ok {
		defer c.Close()
	}
	var body io.Reader
	if resp.BodyInline {
		b, err := checkBody(resp.Body, resp.BodySize)
		if err != nil {
			resp = &Response{ID: resp.ID, Err: fmt.Sprintf("encoding response body: %v", err)}
		} else {
			body = b
		}
	}

	s.outMu.Lock()
	defer s.outMu.Unlock()

	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if body == nil {
		_, err := s.out.Write(b)
		return err
	}

	bw := bufio.NewWriterSize(s.out, 64<<10)
	if _, err := bw.Write(b); err != nil {
		return err
	}
	if err := writeBodyLine(bw, body, resp.BodySize); err != nil {
		return fmt.Errorf("writing response body: %w", err)
	}
	return bw.Flush()
}
