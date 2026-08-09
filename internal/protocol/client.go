package protocol

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Client is a synchronous client for the wire protocol Server implements.
// It's how one buildcache process (cacheprog) talks to another
// (cacheserver) as an actual cas.Store client -- see internal/cas/remote --
// rather than blindly relaying bytes the way cacheprog's old proxy mode
// did.
//
// Client is intentionally synchronous: each Get/Put call writes its request
// and blocks for the matching response before returning, with no
// concurrent request multiplexing. That mirrors Server's own
// single-threaded dispatch (see Server.Run's doc comment) and is
// sufficient because the only caller (a tiered.Store backing one cacheprog
// invocation) never has two requests in flight on the same connection at
// once either.
type Client struct {
	conn io.ReadWriteCloser
	in   *bufio.Reader

	nextID int64
	can    map[Cmd]bool
}

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

	c.can = make(map[Cmd]bool, len(resp.KnownCommands))
	for _, cmd := range resp.KnownCommands {
		c.can[cmd] = true
	}
	return c, nil
}

// CanGet reports whether the server advertised support for "get".
func (c *Client) CanGet() bool { return c.can[CmdGet] }

// CanPut reports whether the server advertised support for "put".
func (c *Client) CanPut() bool { return c.can[CmdPut] }

// Get sends a "get" request for actionID and returns the server's
// response. A cache miss is reported via Response.Miss, not an error --
// callers should check that field, matching Handler.Get's own contract.
func (c *Client) Get(actionID []byte) (*Response, error) {
	return c.roundTrip(&Request{Command: CmdGet, ActionID: actionID})
}

// Put sends a "put" request storing body (size bytes) under actionID,
// tagged with outputID, and returns the server's response.
func (c *Client) Put(actionID, outputID []byte, body io.Reader, size int64) (*Response, error) {
	if body == nil {
		body = bytes.NewReader(nil)
	}
	return c.roundTrip(&Request{
		Command:  CmdPut,
		ActionID: actionID,
		OutputID: outputID,
		Body:     body,
		BodySize: size,
	})
}

// Close sends a "close" request (if the server advertised support for it)
// and waits for the ack, then closes the underlying connection regardless
// of whether that exchange succeeded -- a Close should never leave the
// connection open.
func (c *Client) Close() error {
	var closeErr error
	if c.can[CmdClose] {
		_, closeErr = c.roundTrip(&Request{Command: CmdClose})
	}
	if err := c.conn.Close(); err != nil && closeErr == nil {
		closeErr = err
	}
	return closeErr
}

func (c *Client) roundTrip(req *Request) (*Response, error) {
	c.nextID++
	req.ID = c.nextID

	if err := c.writeRequest(req); err != nil {
		return nil, fmt.Errorf("writing request: %w", err)
	}
	resp, err := c.readResponse()
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.ID != req.ID {
		return nil, fmt.Errorf("response ID %d does not match request ID %d", resp.ID, req.ID)
	}
	if resp.Err != "" {
		return nil, errors.New(resp.Err)
	}
	return resp, nil
}

// writeRequest prepares the full request -- including base64-encoding and
// size-checking the body, for a "put" -- before writing anything to the
// connection. That ordering matters: once the request line is on the wire,
// the server is committed to reading a body line next (see
// Server.readRequest), so a body validation failure discovered only after
// the request line was already sent would desync the connection, leaving
// the server blocked reading a body line that never arrives -- wedging
// every subsequent call on this connection, including Close.
func (c *Client) writeRequest(req *Request) error {
	var bodyLine []byte
	if req.Command == CmdPut && req.BodySize > 0 {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return fmt.Errorf("reading body to send: %w", err)
		}
		if int64(len(body)) != req.BodySize {
			return fmt.Errorf("body size mismatch: read %d bytes, want %d", len(body), req.BodySize)
		}
		encoded, err := json.Marshal(base64.StdEncoding.EncodeToString(body))
		if err != nil {
			return err
		}
		bodyLine = append(encoded, '\n')
	}

	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := c.conn.Write(b); err != nil {
		return err
	}
	if bodyLine != nil {
		if _, err := c.conn.Write(bodyLine); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) readResponse() (*Response, error) {
	line, err := readNonBlankLine(c.in)
	if err != nil {
		return nil, err
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &resp, nil
}
