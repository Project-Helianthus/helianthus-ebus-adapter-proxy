package enh

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
)

func TestLengthPrefixedCodecRoundTrip(t *testing.T) {
	encoder := LengthPrefixedEncoder{}
	parser := LengthPrefixedParser{}

	expectedFrame := downstream.Frame{
		Address: 0x21,
		Command: 0x09,
		Payload: []byte{0x10, 0x11, 0x12},
	}

	encodedFrame, err := encoder.Encode(expectedFrame)
	if err != nil {
		t.Fatalf("expected encode success, got %v", err)
	}

	actualFrame, err := parser.Parse(bytes.NewReader(encodedFrame))
	if err != nil {
		t.Fatalf("expected parse success, got %v", err)
	}

	if !reflect.DeepEqual(actualFrame, expectedFrame) {
		t.Fatalf("expected frame %#v, got %#v", expectedFrame, actualFrame)
	}
}

func TestLengthPrefixedParserRejectsMalformedFrame(t *testing.T) {
	parser := LengthPrefixedParser{}

	_, err := parser.Parse(bytes.NewReader([]byte{0x01}))
	if !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("expected malformed frame error, got %v", err)
	}
}

func TestLengthPrefixedEncoderRejectsOversizePayload(t *testing.T) {
	encoder := LengthPrefixedEncoder{}
	oversizePayload := make([]byte, 254)

	_, err := encoder.Encode(downstream.Frame{
		Address: 0x21,
		Command: 0x05,
		Payload: oversizePayload,
	})
	if err == nil {
		t.Fatalf("expected oversize payload error")
	}
}
