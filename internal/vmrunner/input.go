// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package vmrunner

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Linux input event codes (linux/input-event-codes.h) of the keys that
// agents can press.
const (
	keyLeftCtrl  = 29
	keyLeftShift = 42
	keyLeftAlt   = 56
	keyLeftMeta  = 125
)

// keyNames maps the key names that vm_key takes to Linux key codes.
var keyNames = map[string]uint32{
	"esc": 1, "escape": 1, "backspace": 14, "tab": 15, "enter": 28, "return": 28,
	"ctrl": keyLeftCtrl, "control": keyLeftCtrl, "shift": keyLeftShift, "alt": keyLeftAlt,
	"meta": keyLeftMeta, "super": keyLeftMeta, "win": keyLeftMeta, "altgr": 100, "menu": 127,
	"space": 57, "capslock": 58, "numlock": 69, "scrolllock": 70, "printscreen": 99, "print": 99,
	"home": 102, "up": 103, "pageup": 104, "left": 105, "right": 106, "end": 107,
	"down": 108, "pagedown": 109, "insert": 110, "delete": 111, "del": 111,
	"f1": 59, "f2": 60, "f3": 61, "f4": 62, "f5": 63, "f6": 64, "f7": 65, "f8": 66,
	"f9": 67, "f10": 68, "f11": 87, "f12": 88,
	"minus": 12, "equal": 13, "comma": 51, "dot": 52, "period": 52, "slash": 53,
	"semicolon": 39, "apostrophe": 40, "grave": 41, "backslash": 43,
	"leftbrace": 26, "rightbrace": 27,
}

// unshifted and shifted map the characters on a US keyboard to their keys.
var unshifted, shifted = map[rune]uint32{}, map[rune]uint32{}

func init() {
	rows := []struct {
		plain, shift string
		first        uint32
	}{
		{"1234567890-=", "!@#$%^&*()_+", 2},
		{"qwertyuiop[]", "QWERTYUIOP{}", 16},
		{"asdfghjkl;'`", "ASDFGHJKL:\"~", 30},
		{"\\zxcvbnm,./", "|ZXCVBNM<>?", 43},
	}
	for _, r := range rows {
		shift := []rune(r.shift)
		for i, c := range r.plain {
			code := r.first + uint32(i) //nolint:gosec // G115: i < 12
			unshifted[c] = code
			shifted[shift[i]] = code
		}
	}
	unshifted[' '] = 57
	unshifted['\n'] = 28
	unshifted['\t'] = 15
	for c := 'a'; c <= 'z'; c++ {
		keyNames[string(c)] = unshifted[c]
	}
	for c := '0'; c <= '9'; c++ {
		keyNames[string(c)] = unshifted[c]
	}
}

// typeKeys returns the key presses that type text on a US keyboard layout,
// each a list of keys to press together.
func typeKeys(text string) ([][]uint32, error) {
	var out [][]uint32
	for i, c := range text {
		if code, ok := unshifted[c]; ok {
			out = append(out, []uint32{code})
		} else if code, ok := shifted[c]; ok {
			out = append(out, []uint32{keyLeftShift, code})
		} else {
			return nil, fmt.Errorf("character %q at byte %d cannot be typed: use printable ASCII, tab and newline", c, i)
		}
	}
	return out, nil
}

// parseCombo reads a key combination such as "ctrl+alt+t" or "enter".
func parseCombo(s string) ([]uint32, error) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(s)), "+")
	if len(parts) > 6 {
		return nil, fmt.Errorf("key combination %q has more than 6 keys", s)
	}
	codes := make([]uint32, 0, len(parts))
	for _, p := range parts {
		code, ok := keyNames[strings.TrimSpace(p)]
		if !ok {
			return nil, fmt.Errorf("unknown key %q in %q", p, s)
		}
		codes = append(codes, code)
	}
	return codes, nil
}

// absMax is the largest value of QEMU's absolute pointer axes
// (INPUT_EVENT_ABS_MAX); 0 to absMax spans the display.
const absMax = 0x7fff

// pointerButtons are the buttons that vm_click takes, as QMP names them.
var pointerButtons = map[string]bool{"left": true, "right": true, "middle": true, "wheel-up": true, "wheel-down": true}

type inputEvent struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

type absData struct {
	Axis  string `json:"axis"`
	Value int    `json:"value"`
}

type btnData struct {
	Down   bool   `json:"down"`
	Button string `json:"button"`
}

// qmpInput returns the QMP command that sends events to the VM's input
// devices.
func qmpInput(events ...inputEvent) string {
	b, _ := json.Marshal(map[string]any{
		"execute":   "input-send-event",
		"arguments": map[string]any{"events": events},
	})
	return string(b)
}

// moveTo returns the events that move the absolute pointer to pixel x, y
// of a w×h display.
func moveTo(x, y, w, h int) []inputEvent {
	scale := func(v, size int) int {
		if size <= 1 {
			return 0
		}
		return v * absMax / (size - 1)
	}
	return []inputEvent{
		{Type: "abs", Data: absData{Axis: "x", Value: scale(x, w)}},
		{Type: "abs", Data: absData{Axis: "y", Value: scale(y, h)}},
	}
}

func button(name string, down bool) inputEvent {
	return inputEvent{Type: "btn", Data: btnData{Down: down, Button: name}}
}

// qmpError returns the error in a QMP reply, if there is one.
func qmpError(reply string) error {
	var r struct {
		Error *struct {
			Class string `json:"class"`
			Desc  string `json:"desc"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(reply), &r); err != nil {
		return fmt.Errorf("unreadable reply from QEMU: %w", err)
	}
	if r.Error != nil {
		return fmt.Errorf("QEMU: %s: %s", r.Error.Class, r.Error.Desc)
	}
	return nil
}
