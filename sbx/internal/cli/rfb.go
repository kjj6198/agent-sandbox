package cli

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"io"
	"sync"
	"time"
)

// screen is a VNC (RFB 3.8) client for a sandbox's desktop. It keeps a copy of the
// framebuffer current by always having an incremental update request outstanding, so the
// latest pixels are on hand without asking, and it knows when the screen last changed.
type screen struct {
	conn io.ReadWriteCloser
	r    *bufio.Reader

	writeMu sync.Mutex
	// actMu serializes actions, so each one's keys and settled screen are its own.
	actMu sync.Mutex

	mu        sync.Mutex
	fb        *image.RGBA
	version   uint64
	changedAt time.Time
	changed   chan struct{} // closed and replaced on every change
	err       error         // why the reader stopped

	clipboard string
	provided  chan struct{} // closed when an app has read clipboard
}

// Mouse button bits of an RFB pointer event.
const (
	buttonLeft = 1 << iota
	buttonMiddle
	buttonRight
	wheelUp
	wheelDown
	wheelLeft
	wheelRight
)

const (
	encodingRaw         = 0
	encodingCopyRect    = 1
	encodingDesktopSize = -223
	// UTF-8 clipboard, the only way to enter text the keyboard map lacks.
	encodingExtendedClipboard = -1063131698
)

// Flags of an extended clipboard message.
const (
	clipText    = 1 << 0
	clipCaps    = 1 << 24
	clipRequest = 1 << 25
	clipNotify  = 1 << 27
	clipProvide = 1 << 28
)

func dialScreen(ctx context.Context, c *client, id string) (*screen, error) {
	path := fmt.Sprintf("/v1/sandboxes/%s/tcp?port=%d&passive=1", id, screenPort)
	conn, err := c.upgrade(ctx, "GET", path, "tcp", nil)
	if err != nil {
		return nil, err
	}
	s := &screen{conn: conn, r: bufio.NewReaderSize(conn, 1<<16), changed: make(chan struct{})}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	err = s.handshake()
	if !stop() || err != nil {
		conn.Close()
		return nil, fmt.Errorf("screen of %s: %w", id, cmp.Or(ctx.Err(), err))
	}
	go s.readLoop()
	// The framebuffer stays blank until the update the handshake requested arrives, and a
	// screenshot taken before it would be empty.
	if ok, err := s.Settle(ctx, 0, 5*time.Second, 0, 5*time.Second); !ok {
		s.Close()
		return nil, fmt.Errorf("screen of %s: %w", id, cmp.Or(err, errors.New("the VNC server sent no picture within 5 s")))
	}
	return s, nil
}

func (s *screen) Close() error { return s.conn.Close() }

func checkScreenSize(w, h int) error {
	if w < 1 || h < 1 || w > 8192 || h > 8192 {
		return fmt.Errorf("VNC server reports a %dx%d screen", w, h)
	}
	return nil
}

func (s *screen) handshake() error {
	version := make([]byte, 12)
	if _, err := io.ReadFull(s.r, version); err != nil {
		return err
	}
	if _, err := s.conn.Write([]byte("RFB 003.008\n")); err != nil {
		return err
	}
	var n uint8
	if err := binary.Read(s.r, binary.BigEndian, &n); err != nil {
		return err
	}
	if n == 0 {
		return s.readReason()
	}
	types := make([]byte, n)
	if _, err := io.ReadFull(s.r, types); err != nil {
		return err
	}
	const securityNone = 1
	found := false
	for _, t := range types {
		found = found || t == securityNone
	}
	if !found {
		return fmt.Errorf("VNC server wants authentication (types %v); sbx-desktop serves it without", types)
	}
	if _, err := s.conn.Write([]byte{securityNone}); err != nil {
		return err
	}
	var result uint32
	if err := binary.Read(s.r, binary.BigEndian, &result); err != nil {
		return err
	}
	if result != 0 {
		return s.readReason()
	}
	const shared = 1
	if _, err := s.conn.Write([]byte{shared}); err != nil {
		return err
	}
	var init struct {
		Width, Height uint16
		PixelFormat   [16]byte
		NameLen       uint32
	}
	if err := binary.Read(s.r, binary.BigEndian, &init); err != nil {
		return err
	}
	if _, err := s.r.Discard(int(init.NameLen)); err != nil {
		return err
	}
	if err := checkScreenSize(int(init.Width), int(init.Height)); err != nil {
		return err
	}
	s.fb = image.NewRGBA(image.Rect(0, 0, int(init.Width), int(init.Height)))

	// 32 bits per pixel, little endian, so each pixel arrives as B, G, R, padding.
	pixelFormat := []byte{0, 0, 0, 0, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0}
	encodings := []int32{encodingCopyRect, encodingRaw, encodingDesktopSize, encodingExtendedClipboard}
	msg := binary.BigEndian.AppendUint16([]byte{2, 0}, uint16(len(encodings)))
	for _, e := range encodings {
		msg = binary.BigEndian.AppendUint32(msg, uint32(e))
	}
	if err := s.write(append(pixelFormat, msg...)); err != nil {
		return err
	}
	return s.requestUpdate(false)
}

func (s *screen) readReason() error {
	var n uint32
	if err := binary.Read(s.r, binary.BigEndian, &n); err != nil {
		return err
	}
	reason := make([]byte, min(n, 1024))
	io.ReadFull(s.r, reason)
	return fmt.Errorf("VNC server refused: %s", reason)
}

func (s *screen) write(b []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.conn.Write(b)
	return err
}

func (s *screen) requestUpdate(incremental bool) error {
	s.mu.Lock()
	w, h := s.fb.Rect.Dx(), s.fb.Rect.Dy()
	s.mu.Unlock()
	inc := byte(0)
	if incremental {
		inc = 1
	}
	msg := []byte{3, inc, 0, 0, 0, 0}
	msg = binary.BigEndian.AppendUint16(msg, uint16(w))
	msg = binary.BigEndian.AppendUint16(msg, uint16(h))
	return s.write(msg)
}

func (s *screen) readLoop() {
	err := s.serve()
	s.mu.Lock()
	s.err = fmt.Errorf("screen connection lost: %w", err)
	close(s.changed)
	s.mu.Unlock()
}

func (s *screen) serve() error {
	for {
		typ, err := s.r.ReadByte()
		if err != nil {
			return err
		}
		switch typ {
		case 0:
			if err := s.readUpdate(); err != nil {
				return err
			}
			if err := s.requestUpdate(true); err != nil {
				return err
			}
		case 1: // colour map entries, unused with true colour
			var h struct {
				Pad          byte
				First, Count uint16
			}
			if err := binary.Read(s.r, binary.BigEndian, &h); err != nil {
				return err
			}
			if _, err := s.r.Discard(int(h.Count) * 6); err != nil {
				return err
			}
		case 2: // bell
		case 3:
			if err := s.readClipboard(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown VNC message type %d", typ)
		}
	}
}

func (s *screen) readUpdate() error {
	var h struct {
		Pad   byte
		Count uint16
	}
	if err := binary.Read(s.r, binary.BigEndian, &h); err != nil {
		return err
	}
	for range h.Count {
		var rect struct {
			X, Y, W, H uint16
			Encoding   int32
		}
		if err := binary.Read(s.r, binary.BigEndian, &rect); err != nil {
			return err
		}
		r := image.Rect(int(rect.X), int(rect.Y), int(rect.X)+int(rect.W), int(rect.Y)+int(rect.H))
		// Code in the sandbox can replace Xvnc, so nothing it sends is trusted.
		s.mu.Lock()
		inside := r.In(s.fb.Rect)
		s.mu.Unlock()
		if !inside && rect.Encoding != encodingDesktopSize {
			return fmt.Errorf("VNC server sent a rectangle %v outside the screen", r)
		}
		switch rect.Encoding {
		case encodingRaw:
			row := make([]byte, r.Dx()*4)
			for y := r.Min.Y; y < r.Max.Y; y++ {
				if _, err := io.ReadFull(s.r, row); err != nil {
					return err
				}
				s.mu.Lock()
				dst := s.fb.Pix[s.fb.PixOffset(r.Min.X, y):]
				for i := 0; i < len(row); i += 4 {
					dst[i], dst[i+1], dst[i+2], dst[i+3] = row[i+2], row[i+1], row[i], 255
				}
				s.mu.Unlock()
			}
		case encodingCopyRect:
			var src struct{ X, Y uint16 }
			if err := binary.Read(s.r, binary.BigEndian, &src); err != nil {
				return err
			}
			s.mu.Lock()
			draw.Draw(s.fb, r, s.fb, image.Pt(int(src.X), int(src.Y)), draw.Src)
			s.mu.Unlock()
		case encodingDesktopSize:
			if err := checkScreenSize(r.Dx(), r.Dy()); err != nil {
				return err
			}
			s.mu.Lock()
			s.fb = image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
			s.mu.Unlock()
		default:
			return fmt.Errorf("VNC server sent encoding %d, which was not requested", rect.Encoding)
		}
	}
	if h.Count > 0 {
		s.mu.Lock()
		s.version++
		s.changedAt = time.Now()
		close(s.changed)
		s.changed = make(chan struct{})
		s.mu.Unlock()
	}
	return nil
}

// Version counts framebuffer changes. Pass it to Settle to wait for the effect of an action.
func (s *screen) Version() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

// Image returns a copy of the current screen.
func (s *screen) Image() (*image.RGBA, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	img := image.NewRGBA(s.fb.Rect)
	copy(img.Pix, s.fb.Pix)
	return img, nil
}

// Settle waits for the screen to react to an action taken at version since. It returns
// false if nothing changed within react. Otherwise it waits until the screen has been
// still for quiet, but never longer than limit, so a blinking caret cannot hold it.
func (s *screen) Settle(ctx context.Context, since uint64, react, quiet, limit time.Duration) (bool, error) {
	start := time.Now()
	for {
		s.mu.Lock()
		version, changedAt, changed, err := s.version, s.changedAt, s.changed, s.err
		s.mu.Unlock()
		if err != nil {
			return false, err
		}
		wait := react - time.Since(start)
		if version != since {
			wait = min(quiet-time.Since(changedAt), limit-time.Since(start))
		}
		if wait <= 0 {
			return version != since, nil
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return false, ctx.Err()
		case <-changed:
			t.Stop()
		case <-t.C:
		}
	}
}

// readClipboard answers the server's requests for our clipboard and skips everything else.
func (s *screen) readClipboard() error {
	var h struct {
		Pad [3]byte
		Len int32
	}
	if err := binary.Read(s.r, binary.BigEndian, &h); err != nil {
		return err
	}
	if h.Len >= 0 {
		_, err := s.r.Discard(int(h.Len))
		return err
	}
	if h.Len < -(1 << 20) {
		return fmt.Errorf("VNC server sent a clipboard message of %d bytes", -int64(h.Len))
	}
	payload := make([]byte, -h.Len)
	if _, err := io.ReadFull(s.r, payload); err != nil {
		return err
	}
	if len(payload) < 4 {
		return nil
	}
	flags := binary.BigEndian.Uint32(payload)
	switch {
	case flags&clipCaps != 0:
		return s.sendClipboard(clipCaps|clipText|clipRequest|clipNotify|clipProvide, binary.BigEndian.AppendUint32(nil, 1<<20))
	case flags&clipRequest != 0 && flags&clipText != 0:
		s.mu.Lock()
		text, provided := s.clipboard, s.provided
		s.mu.Unlock()
		var data bytes.Buffer
		zw := zlib.NewWriter(&data)
		binary.Write(zw, binary.BigEndian, uint32(len(text)+1))
		zw.Write(append([]byte(text), 0))
		zw.Close()
		if err := s.sendClipboard(clipProvide|clipText, data.Bytes()); err != nil {
			return err
		}
		if provided != nil {
			close(provided)
			s.mu.Lock()
			if s.provided == provided {
				s.provided = nil
			}
			s.mu.Unlock()
		}
	}
	return nil
}

func (s *screen) sendClipboard(flags uint32, data []byte) error {
	msg := []byte{6, 0, 0, 0}
	msg = binary.BigEndian.AppendUint32(msg, uint32(-int32(4+len(data))))
	msg = binary.BigEndian.AppendUint32(msg, flags)
	return s.write(append(msg, data...))
}

// paste puts text on the sandbox's clipboard and pastes it with shift+Insert, which pastes
// in Chromium, GTK apps, and xterm alike. Xvnc fetches the text only when the app asks, and
// apps fetch in several steps, so a clipboard change before the text shows up on screen
// cancels the paste. paste returns only after the screen changed.
func (s *screen) paste(ctx context.Context, text string) error {
	provided := make(chan struct{})
	s.mu.Lock()
	s.clipboard, s.provided = text, provided
	s.mu.Unlock()
	if err := s.sendClipboard(clipNotify|clipText, nil); err != nil {
		return err
	}
	before := s.Version()
	if err := s.pressChord([]uint32{keyShift, keyInsert}); err != nil {
		return err
	}
	t := time.NewTimer(2 * time.Second)
	defer t.Stop()
	select {
	case <-provided:
	case <-t.C:
		return errors.New("the focused program did not accept a paste; non-ASCII text is typed by pasting")
	case <-ctx.Done():
		return ctx.Err()
	}
	_, err := s.Settle(ctx, before, time.Second, 100*time.Millisecond, 2*time.Second)
	return err
}

// clearClipboard announces an empty clipboard, which makes Xvnc give up the selection.
func (s *screen) clearClipboard() error {
	s.mu.Lock()
	s.clipboard, s.provided = "", nil
	s.mu.Unlock()
	return s.sendClipboard(clipNotify, nil)
}

func (s *screen) Size() image.Point {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fb.Rect.Size()
}

func (s *screen) Pointer(x, y int, buttons byte) error {
	size := s.Size()
	if x < 0 || y < 0 || x >= size.X || y >= size.Y {
		return fmt.Errorf("(%d, %d) is outside the %dx%d screen", x, y, size.X, size.Y)
	}
	msg := []byte{5, buttons}
	msg = binary.BigEndian.AppendUint16(msg, uint16(x))
	msg = binary.BigEndian.AppendUint16(msg, uint16(y))
	return s.write(msg)
}

func (s *screen) Key(keysym uint32, down bool) error {
	d := byte(0)
	if down {
		d = 1
	}
	return s.write(binary.BigEndian.AppendUint32([]byte{4, d, 0, 0}, keysym))
}
