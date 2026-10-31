package protocol

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// maxControlLine caps the length of a request or response JSON line. Real
// control lines are a few hundred bytes (IDs are 32-byte hashes); the cap
// exists so a broken or hostile peer can't make readNonBlankLine buffer an
// unbounded line in memory. Body lines are capped separately, by their
// declared size -- see decodeBodyLine.
const maxControlLine = 1 << 20

var errLineTooLong = errors.New("line exceeds maximum length")

// checkBody verifies, before anything is written to the wire, that body
// will yield exactly size bytes, and returns a reader to stream from.
//
// Checking first matters: once a request or response line declaring size
// is on the wire, the peer is committed to reading a body line of exactly
// that size, so discovering a mismatch partway through would desync the
// connection. For readers whose remaining length is knowable up front (an
// *os.File, a bytes.Reader) the check is free and body is returned as-is,
// to be streamed without buffering; anything else is read fully into
// memory first, which is the only way to know its length.
func checkBody(body io.Reader, size int64) (io.Reader, error) {
	if body == nil {
		if size != 0 {
			return nil, fmt.Errorf("nil body with nonzero size %d", size)
		}
		return bytes.NewReader(nil), nil
	}
	if n, ok := remaining(body); ok {
		if n != size {
			return nil, fmt.Errorf("body size mismatch: have %d bytes, want %d", n, size)
		}
		return body, nil
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("reading body: %w", err)
	}
	if int64(len(data)) != size {
		return nil, fmt.Errorf("body size mismatch: read %d bytes, want %d", len(data), size)
	}
	return bytes.NewReader(data), nil
}

// remaining reports how many bytes r has left, when that's knowable
// without reading it.
func remaining(r io.Reader) (int64, bool) {
	switch b := r.(type) {
	case interface{ Len() int }: // bytes.Reader, bytes.Buffer, strings.Reader
		return int64(b.Len()), true
	case *os.File:
		fi, err := b.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			return 0, false
		}
		off, err := b.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, false
		}
		return fi.Size() - off, true
	}
	return 0, false
}

// writeBodyLine streams size bytes from body to w as a quoted base64 JSON
// string followed by a newline -- the wire format used for a "put"
// request's body line, and a "get" response's optional inline body line
// (see Response.Body's doc comment). It encodes incrementally rather than
// building the whole encoded line in memory first, so a large object
// costs a buffer's worth of memory instead of several copies of itself.
//
// Callers should run body through checkBody first; a short read here
// leaves the connection desynced.
func writeBodyLine(w io.Writer, body io.Reader, size int64) error {
	if _, err := io.WriteString(w, `"`); err != nil {
		return err
	}
	enc := base64.NewEncoder(base64.StdEncoding, w)
	if n, err := io.CopyN(enc, body, size); err != nil {
		if err == io.EOF {
			return fmt.Errorf("body size mismatch: read %d bytes, want %d", n, size)
		}
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\"\n")
	return err
}

// decodeBodyLine reads the next non-blank line from r and decodes it as a
// body line of exactly size bytes. The line's length is bounded by what
// size bytes can legitimately encode to, so a peer can't push an
// arbitrarily long line by lying about (or ignoring) its declared size.
func decodeBodyLine(r *bufio.Reader, size int64) (io.Reader, error) {
	if size < 0 {
		return nil, fmt.Errorf("negative body size %d", size)
	}
	encodedLen := int64(base64.StdEncoding.EncodedLen(int(size)))
	// Generous slack over the exact `"<base64>"\n`, in case a peer's JSON
	// encoder escapes characters (e.g. "\/") that Go's doesn't.
	line, err := readNonBlankLine(r, int(2*encodedLen+64))
	if err != nil {
		return nil, fmt.Errorf("reading body line: %w", err)
	}

	var decoded []byte
	line = bytes.TrimSpace(line)
	if len(line) >= 2 && line[0] == '"' && line[len(line)-1] == '"' && bytes.IndexByte(line, '\\') < 0 {
		// The common case: a plain quoted base64 string. Decode it in
		// place instead of round-tripping through a JSON string first.
		enc := line[1 : len(line)-1]
		buf := make([]byte, base64.StdEncoding.DecodedLen(len(enc)))
		n, err := base64.StdEncoding.Decode(buf, enc)
		if err != nil {
			return nil, fmt.Errorf("base64-decoding body: %w", err)
		}
		decoded = buf[:n]
	} else {
		var encoded string
		if err := json.Unmarshal(line, &encoded); err != nil {
			return nil, fmt.Errorf("decoding body line: %w", err)
		}
		decoded, err = base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("base64-decoding body: %w", err)
		}
	}
	if int64(len(decoded)) != size {
		return nil, fmt.Errorf("body size mismatch: got %d bytes, want %d", len(decoded), size)
	}
	return bytes.NewReader(decoded), nil
}

// readNonBlankLine reads the next non-blank line from r, skipping any
// number of blank lines along the way. Returns io.EOF only once the stream
// is genuinely exhausted. A line longer than max bytes (if max > 0) is an
// error rather than something to keep buffering.
func readNonBlankLine(r *bufio.Reader, max int) ([]byte, error) {
	for {
		line, err := readLineLimit(r, max)
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

// readLineLimit is bufio.Reader.ReadBytes('\n') with a length cap.
func readLineLimit(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		frag, err := r.ReadSlice('\n')
		if max > 0 && len(line)+len(frag) > max {
			return nil, fmt.Errorf("%w (%d bytes)", errLineTooLong, max)
		}
		line = append(line, frag...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, err
	}
}
