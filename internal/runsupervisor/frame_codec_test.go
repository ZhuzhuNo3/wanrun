package runsupervisor

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func TestWireFrameRoundTripAndStrictHeader(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 257)
	var encoded bytes.Buffer
	if err := writeFrame(&encoded, frameOutput, payload); err != nil {
		t.Fatal(err)
	}
	frame, err := readFrame(&oneByteReader{reader: bytes.NewReader(encoded.Bytes())})
	if err != nil {
		t.Fatal(err)
	}
	if frame.kind != frameOutput || !bytes.Equal(frame.payload, payload) {
		t.Fatalf("frame = %#v", frame)
	}

	badMagic := append([]byte(nil), encoded.Bytes()...)
	badMagic[0] ^= 0xff
	badVersion := append([]byte(nil), encoded.Bytes()...)
	badVersion[2]++
	badKind := append([]byte(nil), encoded.Bytes()...)
	badKind[3] = 0xff
	tooLarge := append([]byte(nil), encoded.Bytes()[:wireHeaderBytes]...)
	binary.BigEndian.PutUint32(tooLarge[4:], maximumWirePayload+1)
	for name, data := range map[string][]byte{
		"magic": badMagic, "version": badVersion, "kind": badKind, "size": tooLarge,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readFrame(bytes.NewReader(data)); err == nil {
				t.Fatal("invalid wire frame was accepted")
			}
		})
	}
}

type oneByteReader struct{ reader io.Reader }

func (reader *oneByteReader) Read(buffer []byte) (int, error) {
	if len(buffer) > 1 {
		buffer = buffer[:1]
	}
	return reader.reader.Read(buffer)
}
