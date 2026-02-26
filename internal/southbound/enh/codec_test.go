package enh

import (
	"bytes"
	"errors"
	"io"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
)

func TestENHCodecRoundTripFramedCommandData(t *testing.T) {
	encoder := ENHEncoder{}
	parser := &ENHParser{}

	expectedFrame := downstream.Frame{
		Command: byte(ENHReqStart),
		Payload: []byte{0xA5},
	}

	encodedFrame, err := encoder.Encode(expectedFrame)
	if err != nil {
		t.Fatalf("expected encode success, got %v", err)
	}

	expectedSequence := []byte{0xCA, 0xA5}
	if !reflect.DeepEqual(encodedFrame, expectedSequence) {
		t.Fatalf("expected encoded sequence %#v, got %#v", expectedSequence, encodedFrame)
	}

	actualFrame, err := parser.Parse(bytes.NewReader(encodedFrame))
	if err != nil {
		t.Fatalf("expected parse success, got %v", err)
	}

	if !reflect.DeepEqual(actualFrame, expectedFrame) {
		t.Fatalf("expected frame %#v, got %#v", expectedFrame, actualFrame)
	}
}

func TestENHParserHandlesShortDataByte(t *testing.T) {
	parser := &ENHParser{}

	frame, err := parser.Parse(bytes.NewReader([]byte{0x42}))
	if err != nil {
		t.Fatalf("expected parse success, got %v", err)
	}

	expectedFrame := downstream.Frame{
		Command: byte(ENHResReceived),
		Payload: []byte{0x42},
	}
	if !reflect.DeepEqual(frame, expectedFrame) {
		t.Fatalf("expected short-data frame %#v, got %#v", expectedFrame, frame)
	}
}

func TestENHParserKeepsStateAcrossChunkBoundaries(t *testing.T) {
	parser := &ENHParser{}

	_, err := parser.Parse(bytes.NewReader([]byte{0xC5}))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected first chunk EOF, got %v", err)
	}

	frame, err := parser.Parse(bytes.NewReader([]byte{0x81}))
	if err != nil {
		t.Fatalf("expected parse success on second chunk, got %v", err)
	}

	expectedFrame := downstream.Frame{
		Command: byte(ENHReqSend),
		Payload: []byte{0x41},
	}
	if !reflect.DeepEqual(frame, expectedFrame) {
		t.Fatalf("expected reconstructed frame %#v, got %#v", expectedFrame, frame)
	}
}

func TestENHParserRecoversAfterMalformedSecondByte(t *testing.T) {
	parser := &ENHParser{}
	stream := bytes.NewReader([]byte{
		0xC4, 0xC1, // malformed: second byte must match 10xxxxxx
		0xCA, 0xA5, // valid ENHReqStart + 0xA5
	})

	_, err := parser.Parse(stream)
	if !errors.Is(err, ErrMalformedFrame) {
		t.Fatalf("expected malformed frame error, got %v", err)
	}

	frame, err := parser.Parse(stream)
	if err != nil {
		t.Fatalf("expected parse recovery success, got %v", err)
	}

	expectedFrame := downstream.Frame{
		Command: byte(ENHReqStart),
		Payload: []byte{0xA5},
	}
	if !reflect.DeepEqual(frame, expectedFrame) {
		t.Fatalf("expected recovered frame %#v, got %#v", expectedFrame, frame)
	}
}

func TestENHParserResetsStateAfterTimeoutFragment(t *testing.T) {
	parser := &ENHParser{}
	reader, writer := net.Pipe()
	defer func() {
		_ = reader.Close()
		_ = writer.Close()
	}()

	sequence := EncodeENH(ENHReqSend, 0x41)
	writeErr := make(chan error, 1)
	go func() {
		_, writeErrErr := writer.Write([]byte{sequence[0]})
		writeErr <- writeErrErr
	}()

	if err := reader.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatalf("expected read deadline success, got %v", err)
	}

	_, err := parser.Parse(reader)
	if !isTimeoutError(err) {
		t.Fatalf("expected timeout during fragmented parse, got %v", err)
	}

	if err := <-writeErr; err != nil {
		t.Fatalf("expected first write success, got %v", err)
	}

	parser.Reset()

	if err := reader.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("expected read deadline reset success, got %v", err)
	}

	go func() {
		_, writeErrErr := writer.Write([]byte{0x42})
		writeErr <- writeErrErr
	}()

	frame, err := parser.Parse(reader)
	if err != nil {
		t.Fatalf("expected parse success after reset, got %v", err)
	}

	if err := <-writeErr; err != nil {
		t.Fatalf("expected second write success, got %v", err)
	}

	expected := downstream.Frame{
		Command: byte(ENHResReceived),
		Payload: []byte{0x42},
	}
	if !reflect.DeepEqual(frame, expected) {
		t.Fatalf("expected frame %#v, got %#v", expected, frame)
	}
}
