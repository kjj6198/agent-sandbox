package proto

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// ExecRequest is the JSON body of POST /exec. Script always runs through `bash -lc`
// (`sh -lc` if the image has no bash), so PATH and profile scripts match a login shell.
//
// Without Stdin or TTY the response is 200 with a frame stream. With either, the server
// answers 101 Switching Protocols and the connection carries frames both ways: FrameStdin,
// FrameStdinEOF, and FrameResize from the client, output frames from the server.
type ExecRequest struct {
	Script  string            `json:"script"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	User    string            `json:"user,omitempty"`
	Timeout Duration          `json:"timeout,omitempty"`
	Stdin   bool              `json:"stdin,omitempty"`
	TTY     bool              `json:"tty,omitempty"`
	Rows    uint16            `json:"rows,omitempty"`
	Cols    uint16            `json:"cols,omitempty"`
	// Detach starts the command in the background, logging to Log, and returns at once.
	Detach bool `json:"detach,omitempty"`
}

func (r ExecRequest) Interactive() bool { return r.Stdin || r.TTY }

// ExecResult is the payload of the final FrameExit.
type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
	PID      int    `json:"pid,omitempty"`
	Log      string `json:"log,omitempty"`
}

// ExecUpgrade is the Upgrade token for interactive exec connections.
const ExecUpgrade = "sbx-exec"

const (
	FrameStdout   byte = 1
	FrameStderr   byte = 2
	FrameExit     byte = 3
	FrameStdin    byte = 4
	FrameStdinEOF byte = 5
	// FrameResize carries rows and cols as two big-endian uint16.
	FrameResize byte = 6
)

const maxFrame = 1 << 20

// A frame is a 1-byte type, a 4-byte big-endian length, and the payload.
func WriteFrame(w io.Writer, typ byte, p []byte) error {
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(p)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(p) == 0 {
		return nil
	}
	_, err := w.Write(p)
	return err
}

func ReadFrame(r io.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("frame of %d bytes exceeds limit", n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	return hdr[0], p, nil
}

// FrameWriter turns writes into frames of one type. Several FrameWriters can share one
// underlying writer; Mu serializes them and Flush, if set, runs after each frame.
type FrameWriter struct {
	W     io.Writer
	Type  byte
	Mu    *sync.Mutex
	Flush func()
}

func (f *FrameWriter) Write(p []byte) (int, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	for off := 0; off < len(p); off += maxFrame {
		end := min(off+maxFrame, len(p))
		if err := WriteFrame(f.W, f.Type, p[off:end]); err != nil {
			return off, err
		}
	}
	if f.Flush != nil {
		f.Flush()
	}
	return len(p), nil
}
