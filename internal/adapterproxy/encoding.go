package adapterproxy

import (
	"fmt"

	"github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/domain/downstream"
	southboundenh "github.com/d3vi1/helianthus-ebus-adapter-proxy/internal/southbound/enh"
)

func encodeDownstreamFrame(encoder southboundenh.ENHEncoder, frame downstream.Frame) ([]byte, error) {
	if len(frame.Payload) != 1 {
		return nil, fmt.Errorf(
			"enh frame payload must contain exactly one data byte, got %d",
			len(frame.Payload),
		)
	}

	if southboundenh.ENHCommand(frame.Command) == southboundenh.ENHResReceived && frame.Payload[0] < 0x80 {
		return []byte{frame.Payload[0]}, nil
	}

	return encoder.Encode(frame)
}

func encodeUpstreamFrame(encoder southboundenh.ENHEncoder, frame downstream.Frame) ([]byte, error) {
	if len(frame.Payload) != 1 {
		return nil, fmt.Errorf(
			"enh frame payload must contain exactly one data byte, got %d",
			len(frame.Payload),
		)
	}

	return encoder.Encode(frame)
}
