package ens

import (
	"errors"
	"fmt"
	"io"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
)

var ErrMalformedFrame = errors.New("ens malformed frame")

type ENSCommand byte

const (
	ENSCommandData ENSCommand = 0x1
)

const (
	ENSByteEscape = byte(0xA9)
	ENSByteSync   = byte(0xAA)
	ensEscEscape  = byte(0x00)
	ensEscSync    = byte(0x01)
)

type ENSParser struct {
	escapePending bool
}

func (parser *ENSParser) Reset() {
	parser.escapePending = false
}

func (parser *ENSParser) Finish() error {
	if !parser.escapePending {
		return nil
	}

	parser.escapePending = false
	return fmt.Errorf("%w: dangling escape byte", ErrMalformedFrame)
}

func (parser *ENSParser) Parse(reader io.Reader) (downstream.Frame, error) {
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

func (parser *ENSParser) Feed(payloadByte byte) (downstream.Frame, bool, error) {
	if parser.escapePending {
		parser.escapePending = false

		switch payloadByte {
		case ensEscEscape:
			return frameFromENS(ENSByteEscape), true, nil
		case ensEscSync:
			return frameFromENS(ENSByteSync), true, nil
		default:
			return downstream.Frame{}, false, fmt.Errorf(
				"%w: invalid escaped byte 0x%02X",
				ErrMalformedFrame,
				payloadByte,
			)
		}
	}

	if payloadByte == ENSByteEscape {
		parser.escapePending = true
		return downstream.Frame{}, false, nil
	}

	if payloadByte == ENSByteSync {
		return downstream.Frame{}, false, fmt.Errorf(
			"%w: unexpected sync byte 0x%02X",
			ErrMalformedFrame,
			payloadByte,
		)
	}

	return frameFromENS(payloadByte), true, nil
}

type ENSEncoder struct{}

func (ENSEncoder) Encode(frame downstream.Frame) ([]byte, error) {
	if len(frame.Payload) != 1 {
		return nil, fmt.Errorf(
			"ens frame payload must contain exactly one data byte, got %d",
			len(frame.Payload),
		)
	}

	if frame.Command != 0 && frame.Command != byte(ENSCommandData) {
		return nil, fmt.Errorf("ens command is not supported: 0x%02X", frame.Command)
	}

	return EncodeENS(frame.Payload), nil
}

func EncodeENS(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}

	encoded := make([]byte, 0, len(data))
	for _, payloadByte := range data {
		switch payloadByte {
		case ENSByteEscape:
			encoded = append(encoded, ENSByteEscape, ensEscEscape)
		case ENSByteSync:
			encoded = append(encoded, ENSByteEscape, ensEscSync)
		default:
			encoded = append(encoded, payloadByte)
		}
	}

	return encoded
}

func DecodeENS(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}

	parser := &ENSParser{}
	decoded := make([]byte, 0, len(data))

	for _, payloadByte := range data {
		frame, complete, err := parser.Feed(payloadByte)
		if err != nil {
			return nil, err
		}

		if complete {
			decoded = append(decoded, frame.Payload[0])
		}
	}

	if err := parser.Finish(); err != nil {
		return nil, err
	}

	return decoded, nil
}

func frameFromENS(data byte) downstream.Frame {
	return downstream.Frame{
		Command: byte(ENSCommandData),
		Payload: []byte{data},
	}
}
