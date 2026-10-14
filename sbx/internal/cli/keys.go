package cli

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// X11 keysyms by the names agents use for keys, in xdotool's spelling and common aliases.
var namedKeys = map[string]uint32{
	"return": 0xff0d, "enter": 0xff0d, "tab": 0xff09, "escape": 0xff1b, "esc": 0xff1b,
	"backspace": 0xff08, "delete": 0xffff, "insert": 0xff63, "space": 0x20,
	"home": 0xff50, "end": 0xff57, "pageup": 0xff55, "page_up": 0xff55, "pagedown": 0xff56, "page_down": 0xff56,
	"left": 0xff51, "up": 0xff52, "right": 0xff53, "down": 0xff54,
	"ctrl": 0xffe3, "control": 0xffe3, "shift": 0xffe1, "alt": 0xffe9,
	"super": 0xffeb, "meta": 0xffeb, "cmd": 0xffeb, "win": 0xffeb,
	"f1": 0xffbe, "f2": 0xffbf, "f3": 0xffc0, "f4": 0xffc1, "f5": 0xffc2, "f6": 0xffc3,
	"f7": 0xffc4, "f8": 0xffc5, "f9": 0xffc6, "f10": 0xffc7, "f11": 0xffc8, "f12": 0xffc9,
}

const (
	keyShift  = 0xffe1
	keyInsert = 0xff63
)

// runeKeysym maps a character to the keysym that types it. Xvnc picks the keycode and
// the shift state for it, so capitals and symbols need no modifier from us.
func runeKeysym(r rune) uint32 {
	switch {
	case r == '\n':
		return 0xff0d
	case r == '\t':
		return 0xff09
	case r >= 0x20 && r <= 0x7e, r >= 0xa0 && r <= 0xff:
		return uint32(r)
	default:
		return 0x01000000 | uint32(r)
	}
}

// parseChord turns "ctrl+shift+t" or "Return" into keysyms, modifiers first.
func parseChord(chord string) ([]uint32, error) {
	var keys []uint32
	for _, name := range strings.Split(chord, "+") {
		if sym, ok := namedKeys[strings.ToLower(name)]; ok {
			keys = append(keys, sym)
			continue
		}
		if r, size := utf8.DecodeRuneInString(name); size > 0 && size == len(name) {
			keys = append(keys, runeKeysym(r))
			continue
		}
		return nil, fmt.Errorf("unknown key %q in %q; use names such as ctrl+l, shift+Tab, Return, Escape, F5, or a single character", name, chord)
	}
	return keys, nil
}

// pressChord holds every key of the chord down in order, then releases them in reverse.
func (s *screen) pressChord(keys []uint32) error {
	for _, k := range keys {
		if err := s.Key(k, true); err != nil {
			return err
		}
	}
	for i := len(keys) - 1; i >= 0; i-- {
		if err := s.Key(keys[i], false); err != nil {
			return err
		}
	}
	return nil
}

// typeText types ASCII as key presses and pastes runs of other characters. Xvnc binds such
// keysyms to spare keycodes just in time and reuses them, and apps keep the stale binding:
// Chromium typed 日 for 京 after the keycode moved on.
func (s *screen) typeText(ctx context.Context, text string) error {
	pasted := false
	// Leave nothing on the clipboard: the text may be a secret.
	defer func() {
		if pasted {
			s.clearClipboard()
		}
	}()
	for len(text) > 0 {
		i := strings.IndexFunc(text, func(r rune) bool { return !isTypable(r) })
		if i < 0 {
			i = len(text)
		}
		typed := s.Version()
		for _, r := range text[:i] {
			if err := s.pressChord([]uint32{runeKeysym(r)}); err != nil {
				return err
			}
		}
		text = text[i:]
		if i > 0 && len(text) > 0 {
			// paste waits for the screen to change, so the typed keys must be drawn first.
			if _, err := s.Settle(ctx, typed, 200*time.Millisecond, 100*time.Millisecond, time.Second); err != nil {
				return err
			}
		}
		j := strings.IndexFunc(text, isTypable)
		if j < 0 {
			j = len(text)
		}
		if j > 0 {
			pasted = true
			if err := s.paste(ctx, text[:j]); err != nil {
				return err
			}
		}
		text = text[j:]
	}
	return nil
}

func isTypable(r rune) bool { return r == '\n' || r == '\t' || (r >= 0x20 && r <= 0x7e) }
