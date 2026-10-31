package protocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Client is a synchronous client for the wire protocol Server implements.
// It's how one buildcache process (cacheprog) talks to another
// (cacheserver) as an actual cas.Store client -- see internal/cas/remote --
// rather than blindly relaying bytes the way cacheprog's old proxy mode
// did.
//
// Client is synchronous: each Get/Put call writes its request and blocks
// for the matching response before returning, with no request
// multiplexing. It is safe for concurrent use -- calls are serialized on
// the connection -- which internal/cas/tiered relies on, since its
// background upload worker and the go command's Gets share one Client.
//
// Any transport-level failure (a read or write error, a timeout, a
// malformed or mismatched response) leaves the connection in an unknown
// framing state, so the Client marks itself broken and every later call
// fails immediately with that original error instead of trying to read a
// desynced stream. An error the server reports for one request
// (Response.Err) is a *ServerError and leaves the connection usable.
type Client struct {
	conn io.ReadWriteCloser
	in   *bufio.Reader

	can      map[Cmd]bool
	storeDir string

	mu     sync.Mutex
	nextID int64
	broken error
}

// ServerError is an error the server reported for a single request via
// Response.Err. Unlike a transport error, it says nothing about the health
// of the connection.
type ServerError struct {
	Msg string
}

func (e *ServerError) Error() string { return e.Msg }

// NewClient performs the protocol handshake over conn (reading the
// server's startup capabilities announcement) and returns a ready-to-use
// Client. NewClient takes ownership of conn and closes it if the handshake
// fails.
func NewClient(conn io.ReadWriteCloser) (*Client, error) {
	c := &Client{
		conn: conn,
		in:   bufio.NewReaderSize(conn, 1<<20),
	}

	resp, err := c.readResponse()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading startup response: %w", err)
	}
	if resp.ID != 0 {
		conn.Close()
		return nil, fmt.Errorf("expected startup response with ID 0, got %d", resp.ID)
	}
	if len(resp.KnownCommands) == 0 {
		conn.Close()
		return nil, errors.New("server declared no supported commands")
	}
	if resp.Version != ProtocolVersion {
		conn.Close()
		return nil, fmt.Errorf("server speaks buildcache protocol version %d, want %d (upgrade cacheprog and cacheserver together)", resp.Version, ProtocolVersion)
	}

	c.can = make(map[Cmd]bool, len(resp.KnownCommands))
	for _, cmd := range resp.KnownCommands {
		c.can[cmd] = true
	}
	c.storeDir = resp.StoreDir
	return c, nil
}

// CanGet reports whether the server advertised support for "get".
func (c *Client) CanGet() bool { return c.can[CmdGet] }

// CanPut reports whether the server advertised support for "put". A
// read-only server (see Server.ReadOnly) doesn't.
func (c *Client) CanPut() bool { return c.can[CmdPut] }

// StoreDir returns the cache directory the server advertised at startup,
// or "" if it didn't advertise one (see Response.StoreDir).
func (c *Client) StoreDir() string { return c.storeDir }

// Err returns the error that made the connection unusable, or nil if it's
// still healthy.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.broken
}

// Get sends a "get" request for actionID and returns the server's
// response. A cache miss is reported via Response.Miss, not an error --
// callers should check that field, matching Handler.Get's own contract.
func (c *Client) Get(actionID []byte) (*Response, error) {
	return c.roundTrip(&Request{Command: CmdGet, ActionID: actionID})
}

// Put sends a "put" request storing body (size bytes) under actionID,
// tagged with outputID, and returns the server's response.
func (c *Client) Put(actionID, outputID []byte, body io.Reader, size int64) (*Response, error) {
	return c.roundTrip(&Request{
		Command:  CmdPut,
		ActionID: actionID,
		OutputID: outputID,
		Body:     body,
		BodySize: size,
	})
}

// Close sends a "close" request (if the server advertised support for it
// and the connection is still healthy) and waits for the ack, then closes
// the underlying connection regardless of whether that exchange succeeded
// -- a Close should never leave the connection open.
func (c *Client) Close() error {
	var closeErr error
	c.mu.Lock()
	healthy := c.broken == nil
	c.mu.Unlock()
	if healthy && c.can[CmdClose] {
		_, closeErr = c.roundTrip(&Request{Command: CmdClose})
	}
	if err := c.conn.Close(); err != nil && closeErr == nil {
		closeErr = err
	}
	return closeErr
}

func (c *Client) roundTrip(req *Request) (*Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.broken != nil {
		return nil, fmt.Errorf("connection unusable after an earlier error: %w", c.broken)
	}

	// Validate the body before anything reaches the wire -- see checkBody.
	// A failure here leaves the connection untouched and healthy.
	var body io.Reader
	if req.Command == CmdPut && req.BodySize > 0 {
		b, err := checkBody(req.Body, req.BodySize)
		if err != nil {
			return nil, fmt.Errorf("encoding request body: %w", err)
		}
		body = b
	}

	c.nextID++
	req.ID = c.nextID

	if err := c.writeRequest(req, body); err != nil {
		c.broken = fmt.Errorf("writing request: %w", err)
		return nil, c.broken
	}
	resp, err := c.readResponse()
	if err != nil {
		c.broken = fmt.Errorf("reading response: %w", err)
		return nil, c.broken
	}
	if resp.ID != req.ID {
		c.broken = fmt.Errorf("response ID %d does not match request ID %d", resp.ID, req.ID)
		return nil, c.broken
	}
	if resp.Err != "" {
		return nil, &ServerError{Msg: resp.Err}
	}
	return resp, nil
}

// writeRequest writes req's JSON line and, if body is non-nil, streams the
// body line after it. body must already have passed checkBody.
func (c *Client) writeRequest(req *Request, body io.Reader) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if body == nil {
		_, err := c.conn.Write(b)
		return err
	}

	bw := bufio.NewWriterSize(c.conn, 64<<10)
	if _, err := bw.Write(b); err != nil {
		return err
	}
	if err := writeBodyLine(bw, body, req.BodySize); err != nil {
		return err
	}
	return bw.Flush()
}

// readResponse reads one response's JSON line and, if it declares an inline
// body (see Response.Body's doc comment), the body line that immediately
// follows it.
func (c *Client) readResponse() (*Response, error) {
	line, err := readNonBlankLine(c.in, maxControlLine)
	if err != nil {
		return nil, err
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	if resp.BodyInline {
		body, err := decodeBodyLine(c.in, resp.BodySize)
		if err != nil {
			return nil, err
		}
		resp.Body = body
	}
	return &resp, nil
}
