package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func screenshot(id string) (png []byte, w, h int, err error) {
	dir := vmDir(id)
	tmp := filepath.Join(runDir(dir), fmt.Sprintf("screen-%d.png", time.Now().UnixNano()))
	defer os.Remove(tmp)
	if _, err := qmp(dir, "screendump", map[string]any{"filename": tmp, "format": "png"}, 30*time.Second); err != nil {
		return nil, 0, 0, err
	}
	png, err = readQEMUFile(tmp, 64<<20)
	if err != nil {
		return nil, 0, 0, err
	}
	if len(png) < 24 {
		return nil, 0, 0, fmt.Errorf("bad screenshot")
	}
	return png, int(binary.BigEndian.Uint32(png[16:20])), int(binary.BigEndian.Uint32(png[20:24])), nil
}

// Pointer events use absolute coordinates scaled to 0..32767.
func absEvents(x, y, w, h int) []map[string]any {
	scale := func(v, size int) int {
		if size <= 1 {
			return 0
		}
		v = max(0, min(v, size-1))
		return v * 0x7fff / (size - 1)
	}
	return []map[string]any{
		{"type": "abs", "data": map[string]any{"axis": "x", "value": scale(x, w)}},
		{"type": "abs", "data": map[string]any{"axis": "y", "value": scale(y, h)}},
	}
}

func btnEvent(button string, down bool) []map[string]any {
	return []map[string]any{{"type": "btn", "data": map[string]any{"button": button, "down": down}}}
}

func keyEvent(qcode string, down bool) map[string]any {
	return map[string]any{"type": "key", "data": map[string]any{"down": down,
		"key": map[string]any{"type": "qcode", "data": qcode}}}
}

func (q *qmpConn) send(events []map[string]any) error {
	_, err := q.do("input-send-event", map[string]any{"events": events}, 30*time.Second)
	return err
}

type pointer struct {
	id   string
	q    *qmpConn
	w, h int
}

func newPointer(m *Meta) (*pointer, error) {
	if err := needDesktop(m); err != nil {
		return nil, err
	}
	_, w, h, err := screenshot(m.ID)
	if err != nil {
		return nil, err
	}
	q, err := dialQMP(vmDir(m.ID), 10*time.Second)
	if err != nil {
		return nil, err
	}
	touch(m.ID)
	return &pointer{id: m.ID, q: q, w: w, h: h}, nil
}

func (p *pointer) move(x, y int) error { return p.q.send(absEvents(x, y, p.w, p.h)) }

func (p *pointer) click(x, y int, button string, count int) error {
	if err := p.move(x, y); err != nil {
		return err
	}
	time.Sleep(30 * time.Millisecond)
	for i := 0; i < count; i++ {
		if err := p.q.send(btnEvent(button, true)); err != nil {
			return err
		}
		time.Sleep(40 * time.Millisecond)
		if err := p.q.send(btnEvent(button, false)); err != nil {
			return err
		}
		time.Sleep(60 * time.Millisecond)
	}
	return nil
}

func (p *pointer) drag(x1, y1, x2, y2 int) error {
	if err := p.move(x1, y1); err != nil {
		return err
	}
	time.Sleep(30 * time.Millisecond)
	if err := p.q.send(btnEvent("left", true)); err != nil {
		return err
	}
	const steps = 12
	for i := 1; i <= steps; i++ {
		time.Sleep(20 * time.Millisecond)
		if err := p.move(x1+(x2-x1)*i/steps, y1+(y2-y1)*i/steps); err != nil {
			return err
		}
	}
	time.Sleep(40 * time.Millisecond)
	return p.q.send(btnEvent("left", false))
}

func (p *pointer) scroll(dir string, n int) error {
	btn := "wheel-down"
	switch dir {
	case "up":
		btn = "wheel-up"
	case "left":
		btn = "wheel-left"
	case "right":
		btn = "wheel-right"
	}
	for i := 0; i < n; i++ {
		if err := p.q.send(append(btnEvent(btn, true), btnEvent(btn, false)...)); err != nil {
			return err
		}
		time.Sleep(30 * time.Millisecond)
	}
	return nil
}

// Keyboard: QEMU qcodes for a US layout.
var keyAliases = map[string]string{
	"enter": "ret", "return": "ret", "escape": "esc", "space": "spc", "del": "delete",
	"control": "ctrl", "ctl": "ctrl", "cmd": "meta_l", "win": "meta_l", "super": "meta_l", "meta": "meta_l",
	"windows": "meta_l", "option": "alt", "pageup": "pgup", "pagedown": "pgdn", "page_up": "pgup",
	"page_down": "pgdn", "ins": "insert", "bksp": "backspace", "ctrl_r": "ctrl_r", "altgr": "alt_r",
	"-": "minus", "=": "equal", "[": "bracket_left", "]": "bracket_right", `\`: "backslash", ";": "semicolon",
	"'": "apostrophe", "`": "grave_accent", ",": "comma", ".": "dot", "/": "slash", "plus": "equal",
}

var unshifted = map[rune]string{' ': "spc", '\n': "ret", '\t': "tab", '-': "minus", '=': "equal",
	'[': "bracket_left", ']': "bracket_right", '\\': "backslash", ';': "semicolon", '\'': "apostrophe",
	'`': "grave_accent", ',': "comma", '.': "dot", '/': "slash"}

var shifted = map[rune]string{'!': "1", '@': "2", '#': "3", '$': "4", '%': "5", '^': "6", '&': "7",
	'*': "8", '(': "9", ')': "0", '_': "minus", '+': "equal", '{': "bracket_left", '}': "bracket_right",
	'|': "backslash", ':': "semicolon", '"': "apostrophe", '~': "grave_accent", '<': "comma", '>': "dot", '?': "slash"}

func charKey(r rune) (qcode string, shift bool, ok bool) {
	switch {
	case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		return string(r), false, true
	case r >= 'A' && r <= 'Z':
		return strings.ToLower(string(r)), true, true
	}
	if k, ok := unshifted[r]; ok {
		return k, false, true
	}
	if k, ok := shifted[r]; ok {
		return k, true, true
	}
	return "", false, false
}

func typeText(m *Meta, text string) error {
	for _, r := range text {
		if _, _, ok := charKey(r); !ok {
			return fmt.Errorf("cannot type %q with the virtual keyboard (US layout, ASCII only); put the text on the clipboard or in a file with `veems exec` instead", r)
		}
	}
	q, err := dialQMP(vmDir(m.ID), 10*time.Second)
	if err != nil {
		return err
	}
	defer q.Close()
	touch(m.ID)
	for _, r := range text {
		k, shift, _ := charKey(r)
		down := []map[string]any{keyEvent(k, true)}
		up := []map[string]any{keyEvent(k, false)}
		if shift {
			down = append([]map[string]any{keyEvent("shift", true)}, down...)
			up = append(up, keyEvent("shift", false))
		}
		if err := q.send(down); err != nil {
			return err
		}
		time.Sleep(12 * time.Millisecond)
		if err := q.send(up); err != nil {
			return err
		}
		time.Sleep(12 * time.Millisecond)
	}
	return nil
}

func pressKeys(m *Meta, combos []string) error {
	q, err := dialQMP(vmDir(m.ID), 10*time.Second)
	if err != nil {
		return err
	}
	defer q.Close()
	touch(m.ID)
	for _, combo := range combos {
		var keys []string
		parts := strings.Split(strings.ToLower(combo), "+")
		if combo == "+" {
			parts = []string{"plus"}
		}
		for _, k := range parts {
			k = strings.TrimSpace(k)
			if a, ok := keyAliases[k]; ok {
				k = a
			}
			if k == "" {
				return fmt.Errorf("bad key combo %q", combo)
			}
			keys = append(keys, k)
		}
		// Press in order, release in reverse, and finish before returning so
		// the next veems command never sees a modifier still held.
		for _, k := range keys {
			if err := q.send([]map[string]any{keyEvent(k, true)}); err != nil {
				return fmt.Errorf("key %q: %w", combo, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(40 * time.Millisecond)
		for i := len(keys) - 1; i >= 0; i-- {
			if err := q.send([]map[string]any{keyEvent(keys[i], false)}); err != nil {
				return fmt.Errorf("key %q: %w", combo, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

func launchGUI(m *Meta, o ExecOpts) error {
	if err := needDesktop(m); err != nil {
		return err
	}
	_, err := mustExec(m, m.guest().gui(o))
	return err
}

func needDesktop(m *Meta) error {
	if !m.Desktop {
		return fmt.Errorf("%s has no desktop; start one with `veems new %s --desktop`", m.ID, m.OS)
	}
	return nil
}

func atoiArgs(ss []string) ([]int, error) {
	out := make([]int, len(ss))
	for i, s := range ss {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, fmt.Errorf("%q is not a pixel coordinate", s)
		}
		out[i] = n
	}
	return out, nil
}
