// Package protocol implements the GOCACHEPROG wire protocol that the go
// command speaks to an external build-cache subprocess.
//
// Reference: cmd/go/internal/cacheprog (Go 1.24+), which is the source of
// truth this file mirrors. The go command starts this program as a child
// process and exchanges newline-delimited JSON over stdin/stdout:
//
//  1. On startup, this program writes one Response{ID: 0, KnownCommands: ...}
//     announcing which commands it supports.
//  2. The go command then sends a stream of Request messages on stdin. Each
//     is a single JSON object on its own line. A "put" request with
//     BodySize > 0 is immediately followed by a second line containing the
//     body as a base64-encoded JSON string.
//  3. This program replies to each Request with a Response on stdout.
//     Responses may be written out of order; the ID field ties them back
//     to the originating Request. This skeleton replies in order, which is
//     a valid (if not maximally concurrent) implementation — see server.go.
package protocol

import (
	"io"
	"time"
)

// Cmd identifies the kind of request the go command is issuing.
type Cmd string

const (
	// CmdPut stores an object in the cache under ActionID, tagged with
	// OutputID, with Body as its content. The subprocess must persist Body
	// to a local file and return that file's path in Response.DiskPath;
	// the file must exist at least until a "close" request.
	CmdPut = Cmd("put")

	// CmdGet retrieves an object by ActionID. On a cache miss the
	// subprocess sets Response.Miss = true. On a hit it must populate
	// Response.OutputID and Response.DiskPath (a local file containing the
	// bytes originally stored via CmdPut).
	CmdGet = Cmd("get")

	// CmdClose asks the subprocess to reply and then exit gracefully.
	CmdClose = Cmd("close")
)

// Request is a single JSON-encoded message sent from the go command to this
// subprocess over stdin.
type Request struct {
	// ID is unique per process across all requests and must be echoed back
	// in the corresponding Response.
	ID int64

	// Command is the requested operation. The go command will only send
	// commands this program advertised in its startup KnownCommands.
	Command Cmd

	// ActionID is the cache key for "put" and "get".
	ActionID []byte `json:",omitempty"`

	// OutputID is stored alongside the body for "put" requests.
	OutputID []byte `json:",omitempty"`

	// Body is the object content for "put" requests. It is transmitted as
	// a separate base64-encoded line following the Request's JSON line
	// (see ReadRequest), not as a struct field in the JSON itself — that's
	// what lets large bodies stream without inflating the control message.
	Body io.Reader `json:"-"`

	// BodySize is the number of bytes in Body. Zero means no body line
	// follows.
	BodySize int64 `json:",omitempty"`
}

// Response is the JSON message this subprocess sends back to the go
// command, either as the startup capabilities announcement (ID == 0) or as
// a reply to a specific Request (ID matches Request.ID).
type Response struct {
	ID  int64
	Err string `json:",omitempty"`

	// KnownCommands is set only in the startup message (ID == 0) and lists
	// every Cmd this program is willing to handle.
	KnownCommands []Cmd `json:",omitempty"`

	// Get-specific fields.
	Miss     bool       `json:",omitempty"`
	OutputID []byte     `json:",omitempty"`
	Size     int64      `json:",omitempty"`
	Time     *time.Time `json:",omitempty"`

	// DiskPath is the absolute local path to the body for a cache hit
	// ("get") or the just-stored object ("put"). By convention the file
	// has no extension.
	DiskPath string `json:",omitempty"`
}
