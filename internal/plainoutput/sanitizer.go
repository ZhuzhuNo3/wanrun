package plainoutput

import "unicode/utf8"

type terminalSequence uint8

const (
	terminalText terminalSequence = iota
	terminalEscape
	terminalEscapeIntermediate
	terminalCSI
	terminalOSC
	terminalControlString
	terminalOSCEscape
	terminalStringEscape
)

type sanitizedChunk struct {
	bytes  [utf8.UTFMax * 2]byte
	length int
}

func (chunk *sanitizedChunk) append(content []byte) {
	chunk.length += copy(chunk.bytes[chunk.length:], content)
}

func (chunk *sanitizedChunk) appendReplacement() {
	chunk.append([]byte("\xef\xbf\xbd"))
}

// terminalSanitizer incrementally removes ECMA-48 controls while retaining ordinary UTF-8.
// Its unfinished state is fixed-size and spans supervisor frame boundaries.
type terminalSanitizer struct {
	sequence                   terminalSequence
	utf8Bytes                  [utf8.UTFMax]byte
	utf8Length                 int
	utf8Expected               int
	controlStringUTF8Remaining int
	controlStringC2            bool
}

func (sanitizer *terminalSanitizer) accept(value byte) sanitizedChunk {
	var visible sanitizedChunk
	if sanitizer.sequence == terminalText {
		sanitizer.consumeText(value, &visible)
	} else {
		sanitizer.consumeSequence(value)
	}
	return visible
}

func (sanitizer *terminalSanitizer) finish() sanitizedChunk {
	var visible sanitizedChunk
	if sanitizer.utf8Length != 0 {
		visible.appendReplacement()
	}
	sanitizer.resetUTF8()
	sanitizer.resetSequence()
	return visible
}

func (sanitizer *terminalSanitizer) consumeText(value byte, visible *sanitizedChunk) {
	if sanitizer.utf8Length != 0 {
		if value < 0x80 || value > 0xbf {
			visible.appendReplacement()
			sanitizer.resetUTF8()
			sanitizer.consumeText(value, visible)
			return
		}
		sanitizer.utf8Bytes[sanitizer.utf8Length] = value
		sanitizer.utf8Length++
		if sanitizer.utf8Length < sanitizer.utf8Expected {
			return
		}
		content := sanitizer.utf8Bytes[:sanitizer.utf8Length]
		if !utf8.Valid(content) {
			visible.appendReplacement()
		} else {
			character, _ := utf8.DecodeRune(content)
			if character >= 0x80 && character <= 0x9f {
				sanitizer.consumeC1(byte(character))
			} else {
				visible.append(content)
			}
		}
		sanitizer.resetUTF8()
		return
	}

	if value < utf8.RuneSelf {
		if value == 0x1b {
			sanitizer.sequence = terminalEscape
			return
		}
		visible.append([]byte{value})
		return
	}
	if value <= 0x9f {
		sanitizer.consumeC1(value)
		return
	}
	expected := utf8SequenceLength(value)
	if expected == 0 {
		visible.appendReplacement()
		return
	}
	sanitizer.utf8Bytes[0] = value
	sanitizer.utf8Length = 1
	sanitizer.utf8Expected = expected
}

func (sanitizer *terminalSanitizer) consumeC1(value byte) {
	switch value {
	case 0x90, 0x98, 0x9e, 0x9f:
		sanitizer.sequence = terminalControlString
		sanitizer.resetControlStringUTF8()
	case 0x9b:
		sanitizer.sequence = terminalCSI
	case 0x9d:
		sanitizer.sequence = terminalOSC
		sanitizer.resetControlStringUTF8()
	default:
		// Other C1 controls, including an unmatched ST, are not visible text.
	}
}

func (sanitizer *terminalSanitizer) consumeSequence(value byte) {
	switch sanitizer.sequence {
	case terminalEscape:
		sanitizer.consumeEscape(value)
	case terminalEscapeIntermediate:
		sanitizer.consumeEscapeIntermediate(value)
	case terminalCSI:
		sanitizer.consumeCSI(value)
	case terminalOSC:
		sanitizer.consumeControlString(value, true, terminalOSCEscape)
	case terminalControlString:
		sanitizer.consumeControlString(value, false, terminalStringEscape)
	case terminalOSCEscape:
		sanitizer.consumeStringEscape(value, true, terminalOSC, terminalOSCEscape)
	case terminalStringEscape:
		sanitizer.consumeStringEscape(value, false, terminalControlString, terminalStringEscape)
	}
}

func (sanitizer *terminalSanitizer) consumeEscape(value byte) {
	switch value {
	case 0x1b:
		return
	case '[':
		sanitizer.sequence = terminalCSI
	case ']':
		sanitizer.sequence = terminalOSC
		sanitizer.resetControlStringUTF8()
	case 'P', 'X', '^', '_':
		sanitizer.sequence = terminalControlString
		sanitizer.resetControlStringUTF8()
	default:
		switch {
		case value >= 0x20 && value <= 0x2f:
			sanitizer.sequence = terminalEscapeIntermediate
		case value >= 0x30 && value <= 0x7e:
			sanitizer.sequence = terminalText
		default:
			sanitizer.sequence = terminalText
			sanitizer.consumeRawC1(value)
		}
	}
}

func (sanitizer *terminalSanitizer) consumeEscapeIntermediate(value byte) {
	switch {
	case value == 0x1b:
		sanitizer.sequence = terminalEscape
	case value >= 0x20 && value <= 0x2f:
		return
	case value >= 0x30 && value <= 0x7e:
		sanitizer.sequence = terminalText
	default:
		sanitizer.sequence = terminalText
		sanitizer.consumeRawC1(value)
	}
}

func (sanitizer *terminalSanitizer) consumeCSI(value byte) {
	switch {
	case value == 0x1b:
		sanitizer.sequence = terminalEscape
	case value >= 0x20 && value <= 0x3f:
		return
	case value >= 0x40 && value <= 0x7e:
		sanitizer.sequence = terminalText
	default:
		sanitizer.sequence = terminalText
		sanitizer.consumeRawC1(value)
	}
}

func (sanitizer *terminalSanitizer) consumeRawC1(value byte) {
	if value >= 0x80 && value <= 0x9f {
		sanitizer.consumeC1(value)
	}
}

func (sanitizer *terminalSanitizer) consumeControlString(value byte, allowBell bool,
	escape terminalSequence,
) {
	if sanitizer.controlStringUTF8Remaining != 0 {
		if value >= 0x80 && value <= 0xbf {
			if sanitizer.controlStringC2 && sanitizer.controlStringUTF8Remaining == 1 && value == 0x9c {
				sanitizer.resetSequence()
				return
			}
			sanitizer.controlStringUTF8Remaining--
			if sanitizer.controlStringUTF8Remaining == 0 {
				sanitizer.controlStringC2 = false
			}
			return
		}
		sanitizer.resetControlStringUTF8()
	}
	if value == 0x1b {
		sanitizer.sequence = escape
		return
	}
	if (allowBell && value == 0x07) || value == 0x9c {
		sanitizer.resetSequence()
		return
	}
	if expected := utf8SequenceLength(value); expected != 0 {
		sanitizer.controlStringUTF8Remaining = expected - 1
		sanitizer.controlStringC2 = value == 0xc2
	}
}

func (sanitizer *terminalSanitizer) consumeStringEscape(value byte, allowBell bool,
	content, escape terminalSequence,
) {
	if value == '\\' {
		sanitizer.resetSequence()
		return
	}
	if value == 0x1b {
		return
	}
	sanitizer.sequence = content
	sanitizer.consumeControlString(value, allowBell, escape)
}

func (sanitizer *terminalSanitizer) resetUTF8() {
	sanitizer.utf8Length = 0
	sanitizer.utf8Expected = 0
}

func (sanitizer *terminalSanitizer) resetControlStringUTF8() {
	sanitizer.controlStringUTF8Remaining = 0
	sanitizer.controlStringC2 = false
}

func (sanitizer *terminalSanitizer) resetSequence() {
	sanitizer.sequence = terminalText
	sanitizer.resetControlStringUTF8()
}

func utf8SequenceLength(value byte) int {
	switch {
	case value >= 0xc2 && value <= 0xdf:
		return 2
	case value >= 0xe0 && value <= 0xef:
		return 3
	case value >= 0xf0 && value <= 0xf4:
		return 4
	default:
		return 0
	}
}
