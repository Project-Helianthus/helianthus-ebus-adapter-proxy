package ens

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
)

func TestENSEncoderEscapesSpecialBytes(t *testing.T) {
	encoder := ENSEncoder{}

	testCases := []struct {
		name        string
		frame       downstream.Frame
		expectedRaw []byte
	}{
		{
			name: "escape_byte",
			frame: downstream.Frame{
				Command: byte(ENSCommandData),
				Payload: []byte{ENSByteEscape},
			},
			expectedRaw: []byte{ENSByteEscape, ensEscEscape},
		},
		{
			name: "sync_byte",
			frame: downstream.Frame{
				Command: byte(ENSCommandData),
				Payload: []byte{ENSByteSync},
			},
			expectedRaw: []byte{ENSByteEscape, ensEscSync},
		},
		{
			name: "regular_byte",
			frame: downstream.Frame{
				Command: byte(ENSCommandData),
				Payload: []byte{0x42},
			},
			expectedRaw: []byte{0x42},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			encoded, err := encoder.Encode(testCase.frame)
			if err != nil {
				t.Fatalf("expected encode success, got %v", err)
			}

			if !reflect.DeepEqual(encoded, testCase.expectedRaw) {
				t.Fatalf("expected encoded bytes %#v, got %#v", testCase.expectedRaw, encoded)
			}
		})
	}
}

func TestENSParserUnescapesBytes(t *testing.T) {
	parser := &ENSParser{}

	testCases := []struct {
		name         string
		input        []byte
		expectedByte byte
	}{
		{
			name:         "escaped_escape",
			input:        []byte{ENSByteEscape, ensEscEscape},
			expectedByte: ENSByteEscape,
		},
		{
			name:         "escaped_sync",
			input:        []byte{ENSByteEscape, ensEscSync},
			expectedByte: ENSByteSync,
		},
		{
			name:         "raw_byte",
			input:        []byte{0x31},
			expectedByte: 0x31,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			frame, err := parser.Parse(bytes.NewReader(testCase.input))
			if err != nil {
				t.Fatalf("expected parse success, got %v", err)
			}

			expectedFrame := downstream.Frame{
				Command: byte(ENSCommandData),
				Payload: []byte{testCase.expectedByte},
			}
			if !reflect.DeepEqual(frame, expectedFrame) {
				t.Fatalf("expected frame %#v, got %#v", expectedFrame, frame)
			}
		})
	}
}

func TestENSParserKeepsStateAcrossChunkBoundaries(t *testing.T) {
	parser := &ENSParser{}

	_, err := parser.Parse(bytes.NewReader([]byte{ENSByteEscape}))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF for incomplete escape sequence, got %v", err)
	}

	frame, err := parser.Parse(bytes.NewReader([]byte{ensEscSync}))
	if err != nil {
		t.Fatalf("expected parse success on continuation chunk, got %v", err)
	}

	expectedFrame := downstream.Frame{
		Command: byte(ENSCommandData),
		Payload: []byte{ENSByteSync},
	}
	if !reflect.DeepEqual(frame, expectedFrame) {
		t.Fatalf("expected reconstructed frame %#v, got %#v", expectedFrame, frame)
	}
}

func TestENSParserRecoversAfterMalformedEscapeSequence(t *testing.T) {
	parser := &ENSParser{}
	stream := bytes.NewReader([]byte{
		ENSByteEscape, 0x02,
		0x41,
	})

	_, err := parser.Parse(stream)
	if !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("expected malformed frame error, got %v", err)
	}

	frame, err := parser.Parse(stream)
	if err != nil {
		t.Fatalf("expected parser recovery success, got %v", err)
	}

	expectedFrame := downstream.Frame{
		Command: byte(ENSCommandData),
		Payload: []byte{0x41},
	}
	if !reflect.DeepEqual(frame, expectedFrame) {
		t.Fatalf("expected recovered frame %#v, got %#v", expectedFrame, frame)
	}
}

func TestDecodeENSRejectsDanglingEscape(t *testing.T) {
	_, err := DecodeENS([]byte{ENSByteEscape})
	if !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("expected malformed frame error, got %v", err)
	}
}
