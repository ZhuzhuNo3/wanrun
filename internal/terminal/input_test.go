package terminal

import (
	"reflect"
	"testing"

	xterm "github.com/gitpod-io/xterm-go"
)

func TestDecodeMousePreservesButtonActionModifiersAndCellCoordinates(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want xterm.CoreMouseEvent
	}{
		{name: "press", raw: "\x1b[<0;12;7M", want: xterm.CoreMouseEvent{
			Col: 12, Row: 7, Button: xterm.MouseButtonLeft, Action: xterm.MouseActionDown}},
		{name: "release", raw: "\x1b[<2;13;8m", want: xterm.CoreMouseEvent{
			Col: 13, Row: 8, Button: xterm.MouseButtonRight, Action: xterm.MouseActionUp}},
		{name: "modified-drag", raw: "\x1b[<60;14;9M", want: xterm.CoreMouseEvent{
			Col: 14, Row: 9, Button: xterm.MouseButtonLeft, Action: xterm.MouseActionMove,
			Shift: true, Alt: true, Ctrl: true}},
		{name: "unbuttoned-move", raw: "\x1b[<35;15;10M", want: xterm.CoreMouseEvent{
			Col: 15, Row: 10, Button: xterm.MouseButtonNone, Action: xterm.MouseActionMove}},
		{name: "modified-wheel-down", raw: "\x1b[<93;16;11M", want: xterm.CoreMouseEvent{
			Col: 16, Row: 11, Button: xterm.MouseButtonWheel, Action: xterm.MouseActionDown,
			Shift: true, Alt: true, Ctrl: true}},
		{name: "horizontal-wheel-left", raw: "\x1b[<66;18;13M", want: xterm.CoreMouseEvent{
			Col: 18, Row: 13, Button: xterm.MouseButtonWheel, Action: xterm.MouseActionLeft}},
		{name: "horizontal-wheel-right", raw: "\x1b[<67;19;14M", want: xterm.CoreMouseEvent{
			Col: 19, Row: 14, Button: xterm.MouseButtonWheel, Action: xterm.MouseActionRight}},
		{name: "auxiliary", raw: "\x1b[<128;17;12M", want: xterm.CoreMouseEvent{
			Col: 17, Row: 12, Button: xterm.MouseButtonAux1, Action: xterm.MouseActionDown}},
		{name: "auxiliary-five", raw: "\x1b[<192;20;15M", want: xterm.CoreMouseEvent{
			Col: 20, Row: 15, Button: xterm.MouseButtonAux5, Action: xterm.MouseActionDown}},
		{name: "auxiliary-eight", raw: "\x1b[<195;21;16M", want: xterm.CoreMouseEvent{
			Col: 21, Row: 16, Button: xterm.MouseButtonAux8, Action: xterm.MouseActionDown}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := decodeMouse([]byte(test.raw))
			if !ok || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("decodeMouse(%q)=(%#v,%t), want (%#v,true)", test.raw, got, ok, test.want)
			}
		})
	}
}

func TestDecodeMouseRejectsInvalidCoordinatesAndCodes(t *testing.T) {
	for _, raw := range []string{"\x1b[<0;0;1M", "\x1b[<0;1;0M", "\x1b[<96;1;1M",
		"\x1b[<64;1;1m", "\x1b[<256;1;1M", "\x1b[<320;1;1M", "\x1b[<0;1M"} {
		if event, ok := decodeMouse([]byte(raw)); ok {
			t.Fatalf("decodeMouse(%q) accepted %#v", raw, event)
		}
	}
}

func TestCompleteSGRMouseSequencesNeverBecomeKeyboardBytes(t *testing.T) {
	for _, raw := range []string{"\x1b[<96;12;7M", "\x1b[<64;12;7m", "\x1b[<256;12;7M",
		"\x1b[<320;12;7M", "\x1b[<0;;7M", "\x1b[<0;0;7M", "\x1b[<0;12M"} {
		decoded, consumed, complete := decodeOneInput([]byte(raw))
		if !complete || consumed != len(raw) || decoded.kind != decodedMouse || decoded.mouseParsed {
			t.Fatalf("invalid SGR mouse %q decoded as kind=%d parsed=%t consumed=%d complete=%t",
				raw, decoded.kind, decoded.mouseParsed, consumed, complete)
		}
	}
}
