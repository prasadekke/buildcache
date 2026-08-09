package protocol

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
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
	if err := s.writeResponse(&Response{ID: 0, KnownCommands: []Cmd{CmdGet, CmdPut, CmdClose}}); err != nil {
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
			err = s.Handler.Put(req, resp)
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
		if req.BodySize == 0 {
			// No body line follows for a zero-byte body (see readLine's doc
			// comment), but the body is still a legitimate empty object,
			// not a missing one -- give Store.Put a valid empty io.Reader
			// rather than leaving req.Body nil.
			req.Body = bytes.NewReader(nil)
		} else {
			bodyLine, bErr := s.readLine()
			if bErr != nil {
				return nil, fmt.Errorf("reading body line: %w", bErr)
			}
			var encoded string
			if jsonErr := json.Unmarshal(bodyLine, &encoded); jsonErr != nil {
				return nil, fmt.Errorf("decoding body line: %w", jsonErr)
			}
			decoded, decErr := base64.StdEncoding.DecodeString(encoded)
			if decErr != nil {
				return nil, fmt.Errorf("base64-decoding body: %w", decErr)
			}
			if int64(len(decoded)) != req.BodySize {
				return nil, fmt.Errorf("body size mismatch: got %d bytes, want %d", len(decoded), req.BodySize)
			}
			req.Body = bytes.NewReader(decoded)
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
	return readNonBlankLine(s.in)
}

// readNonBlankLine reads the next non-blank line from r, skipping any
// number of blank lines along the way. Returns io.EOF only once the stream
// is genuinely exhausted.
func readNonBlankLine(r *bufio.Reader) ([]byte, error) {
	for {
		line, err := r.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			return nil, err // typically io.EOF
		}
		if err != nil && err != io.EOF {
			return nil, err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			if err == io.EOF {
				return nil, io.EOF
			}
			continue
		}
		return line, nil
	}
}

func (s *Server) writeResponse(resp *Response) error {
	s.outMu.Lock()
	defer s.outMu.Unlock()

	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = s.out.Write(b)
	return err
}
