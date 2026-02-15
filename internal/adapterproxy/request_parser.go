package adapterproxy

import (
	"errors"
	"fmt"
	"io"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	southboundenh "github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

var errMalformedRequest = errors.New("enh malformed request")

// requestParser parses enhanced protocol requests flowing from a downstream client.
//
// Unlike adapter responses, raw bytes < 0x80 represent short-form SEND requests.
type requestParser struct {
	pending bool
	byte1   byte
}

func (parser *requestParser) Reset() {
	parser.pending = false
	parser.byte1 = 0
}

func (parser *requestParser) Parse(reader io.Reader) (downstream.Frame, error) {
	buffer := make([]byte, 1)

	for {
		if _, err := io.ReadFull(reader, buffer); err != nil {
			return downstream.Frame{}, err
		}

		frame, complete, err := parser.Feed(buffer[0])
		if err != nil {
			return downstream.Frame{}, err
		}

		if complete {
			return frame, nil
		}
	}
}

func (parser *requestParser) Feed(payloadByte byte) (downstream.Frame, bool, error) {
	if !parser.pending {
		if payloadByte&0x80 == 0 {
			return downstream.Frame{
				Command: byte(southboundenh.ENHReqSend),
				Payload: []byte{payloadByte},
			}, true, nil
		}

		if payloadByte&0xC0 == 0x80 {
			return downstream.Frame{}, false, fmt.Errorf(
				"%w: orphan second byte 0x%02X",
				errMalformedRequest,
				payloadByte,
			)
		}

		parser.pending = true
		parser.byte1 = payloadByte
		return downstream.Frame{}, false, nil
	}

	if payloadByte&0xC0 != 0x80 {
		parser.pending = false
		return downstream.Frame{}, false, fmt.Errorf(
			"%w: invalid second byte 0x%02X",
			errMalformedRequest,
			payloadByte,
		)
	}

	command, data, err := southboundenh.DecodeENH(parser.byte1, payloadByte)
	parser.pending = false
	if err != nil {
		return downstream.Frame{}, false, err
	}

	return downstream.Frame{
		Command: byte(command),
		Payload: []byte{data},
	}, true, nil
}
