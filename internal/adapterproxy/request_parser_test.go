package adapterproxy

import (
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	southboundenh "github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

func TestRequestParserResetsStateAfterTimeoutFragment(t *testing.T) {
	parser := &requestParser{}
	reader, writer := net.Pipe()
	defer func() {
		_ = reader.Close()
		_ = writer.Close()
	}()

	sequence := southboundenh.EncodeENH(southboundenh.ENHReqSend, 0x41)

	writeErr := make(chan error, 1)
	go func() {
		_, writeErrErr := writer.Write([]byte{sequence[0]})
		writeErr <- writeErrErr
	}()

	if err := reader.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatalf("expected read deadline success, got %v", err)
	}

	_, err := parser.Parse(reader)
	if !isTimeout(err) {
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
		Command: byte(southboundenh.ENHReqSend),
		Payload: []byte{0x42},
	}
	if !reflect.DeepEqual(frame, expected) {
		t.Fatalf("expected frame %#v, got %#v", expected, frame)
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
