package enh

import (
	"errors"
	"fmt"
	"io"

	"github.com/Project-Helianthus/helianthus-ebus-adapter-proxy/internal/domain/downstream"
)

var ErrMalformedFrame = errors.New("enh malformed frame")

type ENHCommand byte

const (
	ENHReqInit  ENHCommand = 0x0
	ENHReqSend  ENHCommand = 0x1
	ENHReqStart ENHCommand = 0x2
	ENHReqInfo  ENHCommand = 0x3

	ENHResResetted  ENHCommand = 0x0
	ENHResReceived  ENHCommand = 0x1
	ENHResStarted   ENHCommand = 0x2
	ENHResInfo      ENHCommand = 0x3
	ENHResFailed    ENHCommand = 0xA
	ENHResErrorEBUS ENHCommand = 0xB
	ENHResErrorHost ENHCommand = 0xC
)

const (
	enhByteFlag = byte(0x80)
	enhByteMask = byte(0xC0)
	enhByte1    = byte(0xC0)
	enhByte2    = byte(0x80)
)

type ENHParser struct {
	pending bool
	byte1   byte
}

func (parser *ENHParser) Reset() {
	parser.pending = false
	parser.byte1 = 0
}

func (parser *ENHParser) Parse(reader io.Reader) (downstream.Frame, error) {
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

func (parser *ENHParser) Feed(payloadByte byte) (downstream.Frame, bool, error) {
	if !parser.pending {
		if payloadByte&enhByteFlag == 0 {
			return frameFromENH(ENHResReceived, payloadByte), true, nil
		}

		if payloadByte&enhByteMask == enhByte2 {
			return downstream.Frame{}, false, fmt.Errorf(
				"%w: orphan second byte 0x%02X",
				ErrMalformedFrame,
				payloadByte,
			)
		}

		parser.pending = true
		parser.byte1 = payloadByte
		return downstream.Frame{}, false, nil
	}

	if payloadByte&enhByteMask != enhByte2 {
		parser.pending = false
		return downstream.Frame{}, false, fmt.Errorf(
			"%w: invalid second byte 0x%02X",
			ErrMalformedFrame,
			payloadByte,
		)
	}

	command, data, err := DecodeENH(parser.byte1, payloadByte)
	parser.pending = false
	if err != nil {
		return downstream.Frame{}, false, err
	}

	return frameFromENH(command, data), true, nil
}

type ENHEncoder struct{}

func (ENHEncoder) Encode(frame downstream.Frame) ([]byte, error) {
	if len(frame.Payload) != 1 {
		return nil, fmt.Errorf(
			"enh frame payload must contain exactly one data byte, got %d",
			len(frame.Payload),
		)
	}

	command := frame.Command
	if command > 0x0F {
		return nil, fmt.Errorf("enh command out of range: 0x%02X", command)
	}

	sequence := EncodeENH(ENHCommand(command), frame.Payload[0])
	return []byte{sequence[0], sequence[1]}, nil
}

func EncodeENH(command ENHCommand, data byte) [2]byte {
	byte1 := enhByte1 | (byte(command) << 2) | ((data & 0xC0) >> 6)
	byte2 := enhByte2 | (data & 0x3F)
	return [2]byte{byte1, byte2}
}

func DecodeENH(byte1, byte2 byte) (ENHCommand, byte, error) {
	if byte1&enhByteMask != enhByte1 {
		return 0, 0, fmt.Errorf("%w: invalid first byte 0x%02X", ErrMalformedFrame, byte1)
	}

	if byte2&enhByteMask != enhByte2 {
		return 0, 0, fmt.Errorf("%w: invalid second byte 0x%02X", ErrMalformedFrame, byte2)
	}

	command := ENHCommand((byte1 >> 2) & 0x0F)
	data := byte(((byte1 & 0x03) << 6) | (byte2 & 0x3F))

	return command, data, nil
}

func frameFromENH(command ENHCommand, data byte) downstream.Frame {
	return downstream.Frame{
		Command: byte(command),
		Payload: []byte{data},
	}
}
