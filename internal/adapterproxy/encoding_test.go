package adapterproxy

import (
	"testing"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	southboundenh "github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

func TestEncodeDownstreamFrame_UsesShortFormForReceivedBelow80(t *testing.T) {
	encoder := southboundenh.ENHEncoder{}
	frame := downstream.Frame{
		Command: byte(southboundenh.ENHResReceived),
		Payload: []byte{0x7F},
	}

	encoded, err := encodeDownstreamFrame(encoder, frame)
	if err != nil {
		t.Fatalf("encodeDownstreamFrame returned error: %v", err)
	}

	if len(encoded) != 1 || encoded[0] != 0x7F {
		t.Fatalf("expected short form [0x7F], got %v", encoded)
	}
}

func TestEncodeDownstreamFrame_EncodesReceivedAtOrAbove80(t *testing.T) {
	encoder := southboundenh.ENHEncoder{}
	frame := downstream.Frame{
		Command: byte(southboundenh.ENHResReceived),
		Payload: []byte{0x80},
	}

	encoded, err := encodeDownstreamFrame(encoder, frame)
	if err != nil {
		t.Fatalf("encodeDownstreamFrame returned error: %v", err)
	}

	if len(encoded) != 2 {
		t.Fatalf("expected 2-byte ENH encoding, got %v", encoded)
	}
}

func TestEncodeDownstreamFrame_DoesNotShortFormNonReceived(t *testing.T) {
	encoder := southboundenh.ENHEncoder{}
	frame := downstream.Frame{
		Command: byte(southboundenh.ENHResResetted),
		Payload: []byte{0x00},
	}

	encoded, err := encodeDownstreamFrame(encoder, frame)
	if err != nil {
		t.Fatalf("encodeDownstreamFrame returned error: %v", err)
	}

	if len(encoded) != 2 {
		t.Fatalf("expected 2-byte ENH encoding, got %v", encoded)
	}
}

func TestEncodeUpstreamFrame_UsesShortFormForSendBelow80(t *testing.T) {
	encoder := southboundenh.ENHEncoder{}
	frame := downstream.Frame{
		Command: byte(southboundenh.ENHReqSend),
		Payload: []byte{0x01},
	}

	encoded, err := encodeUpstreamFrame(encoder, frame)
	if err != nil {
		t.Fatalf("encodeUpstreamFrame returned error: %v", err)
	}

	if len(encoded) != 1 || encoded[0] != 0x01 {
		t.Fatalf("expected short form [0x01], got %v", encoded)
	}
}

func TestEncodeUpstreamFrame_EncodesSendAtOrAbove80(t *testing.T) {
	encoder := southboundenh.ENHEncoder{}
	frame := downstream.Frame{
		Command: byte(southboundenh.ENHReqSend),
		Payload: []byte{0x80},
	}

	encoded, err := encodeUpstreamFrame(encoder, frame)
	if err != nil {
		t.Fatalf("encodeUpstreamFrame returned error: %v", err)
	}

	if len(encoded) != 2 {
		t.Fatalf("expected 2-byte ENH encoding, got %v", encoded)
	}
}
