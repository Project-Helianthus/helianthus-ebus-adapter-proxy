package enh

import (
	"errors"
	"fmt"
	"io"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
)

var ErrMalformedFrame = errors.New("enh malformed frame")

type LengthPrefixedParser struct{}

func (LengthPrefixedParser) Parse(reader io.Reader) (downstream.Frame, error) {
	header := make([]byte, 1)
	if _, err := io.ReadFull(reader, header); err != nil {
		return downstream.Frame{}, err
	}

	frameLength := int(header[0])
	if frameLength < 2 {
		return downstream.Frame{}, fmt.Errorf(
			"%w: frame length %d is below minimum",
			ErrMalformedFrame,
			frameLength,
		)
	}

	body := make([]byte, frameLength)
	if _, err := io.ReadFull(reader, body); err != nil {
		return downstream.Frame{}, err
	}

	return downstream.Frame{
		Address: body[0],
		Command: body[1],
		Payload: append([]byte(nil), body[2:]...),
	}, nil
}

type LengthPrefixedEncoder struct{}

func (LengthPrefixedEncoder) Encode(frame downstream.Frame) ([]byte, error) {
	frameLength := len(frame.Payload) + 2
	if frameLength > 255 {
		return nil, fmt.Errorf("payload too large: got %d bytes", len(frame.Payload))
	}

	payload := make([]byte, frameLength+1)
	payload[0] = byte(frameLength)
	payload[1] = frame.Address
	payload[2] = frame.Command
	copy(payload[3:], frame.Payload)

	return payload, nil
}
