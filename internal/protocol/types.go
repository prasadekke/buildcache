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

// ProtocolVersion is the version of buildcache's extensions to the
// GOCACHEPROG protocol (inline "get" bodies, StoreDir). Bump it whenever
// the framing between two buildcache processes changes incompatibly.
const ProtocolVersion = 1

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

	// Version is buildcache's own wire-protocol version (ProtocolVersion),
	// set only in the startup message. The go command ignores it; Client
	// refuses to talk to a Server announcing a different one, since
	// buildcache's extensions (see Body) change the framing and a
	// mismatched pair would silently desync rather than fail cleanly.
	Version int `json:",omitempty"`

	// StoreDir is the server's cache directory, set only in the startup
	// message and only when the server chooses to advertise it (cacheserver
	// does on its same-machine Unix socket). It lets a same-machine client
	// notice it would otherwise be layering a store on top of itself --
	// see cmd/cacheprog.
	StoreDir string `json:",omitempty"`

	// Get-specific fields.
	Miss     bool       `json:",omitempty"`
	OutputID []byte     `json:",omitempty"`
	Size     int64      `json:",omitempty"`
	Time     *time.Time `json:",omitempty"`

	// DiskPath is the absolute local path to the body for a cache hit
	// ("get") or the just-stored object ("put"). By convention the file
	// has no extension.
	//
	// This is only valid to read on the machine that received this
	// Response. On the go-command-facing leg that's always true (a
	// GOCACHEPROG subprocess is always local). It is *not* generally true
	// on the cacheprog-to-cacheserver leg once cacheserver is reachable
	// over the network rather than a same-machine Unix socket -- see Body.
	DiskPath string `json:",omitempty"`

	// BodyInline marks whether Body is present on the wire. BodySize alone
	// cannot serve as that marker because zero-byte cache objects are valid
	// and still need an inline body line when DiskPath is remote-only.
	BodyInline bool `json:",omitempty"`

	// Body and BodySize optionally carry the object's bytes inline on a
	// "get" hit, the same way a "put" request already carries its body: a
	// base64-encoded line immediately following this Response's JSON line
	// when BodyInline is true. The real go command never expects or reads
	// this (its GOCACHEPROG subprocess is always local, so DiskPath alone is
	// always sufficient) -- these fields exist for the cacheprog-to-
	// cacheserver leg only, where both ends are buildcache processes. This
	// extension is not part of the GOCACHEPROG spec: a generic decoder would
	// ignore BodyInline but would not know to consume the extra body line.
	//
	// cacheserver only populates these for connections where DiskPath
	// might not be locally readable by the caller -- see
	// internal/cas.InlineGetBodies -- never for a same-machine Unix
	// socket, where they'd just be wasted work. internal/cas/remote.Store
	// is the reader: when present, it materializes Body to a local file
	// and reports *that* path instead of trusting DiskPath verbatim.
	Body     io.Reader `json:"-"`
	BodySize int64     `json:",omitempty"`
}
