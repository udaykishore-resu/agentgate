package provider

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// EventStreamMessage is one decoded frame of the AWS event-stream encoding,
// which Bedrock uses for streaming responses instead of Server-Sent Events.
type EventStreamMessage struct {
	Headers map[string]string
	Payload []byte
}

// EventStreamDecoder decodes the AWS event-stream binary framing.
//
// Frame layout:
//
//	 0..3   total length      uint32 big-endian
//	 4..7   headers length    uint32 big-endian
//	 8..11  prelude CRC32     uint32
//	12..    headers           name-len(1) name value-type(1) value
//	 ...    payload           total - headers - 16 bytes
//	last 4  message CRC32     uint32
//
// The CRCs are not verified. The transport is TLS over a single connection, so
// a corrupted frame means a broken connection rather than a bit flip, and a
// malformed frame is already rejected by the length arithmetic below. This is
// a deliberate, documented simplification.
type EventStreamDecoder struct {
	r io.Reader
}

// NewEventStreamDecoder wraps a reader.
func NewEventStreamDecoder(r io.Reader) *EventStreamDecoder { return &EventStreamDecoder{r: r} }

// Header value types defined by the encoding. Only the ones Bedrock emits are
// decoded into strings; the rest are skipped by length so an unexpected type
// does not desynchronise the stream.
const (
	esTypeBoolTrue  = 0
	esTypeBoolFalse = 1
	esTypeByte      = 2
	esTypeShort     = 3
	esTypeInteger   = 4
	esTypeLong      = 5
	esTypeByteArray = 6
	esTypeString    = 7
	esTypeTimestamp = 8
	esTypeUUID      = 9
)

// maxFrame bounds a single frame so a malformed length cannot be turned into
// an allocation large enough to end the process.
const maxFrame = 16 << 20

// Next reads the next frame, returning io.EOF at the end of the stream.
func (d *EventStreamDecoder) Next() (*EventStreamMessage, error) {
	var prelude [12]byte
	if _, err := io.ReadFull(d.r, prelude[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, io.EOF
		}
		return nil, err
	}
	total := binary.BigEndian.Uint32(prelude[0:4])
	headerLen := binary.BigEndian.Uint32(prelude[4:8])
	if total < 16 || total > maxFrame || uint64(headerLen)+16 > uint64(total) {
		return nil, fmt.Errorf("eventstream: implausible frame lengths total=%d headers=%d", total, headerLen)
	}
	rest := make([]byte, total-12)
	if _, err := io.ReadFull(d.r, rest); err != nil {
		return nil, err
	}
	headers := rest[:headerLen]
	payload := rest[headerLen : len(rest)-4]

	msg := &EventStreamMessage{Headers: map[string]string{}, Payload: payload}
	for i := 0; i < len(headers); {
		if i+1 > len(headers) {
			break
		}
		nameLen := int(headers[i])
		i++
		if i+nameLen+1 > len(headers) {
			break
		}
		name := string(headers[i : i+nameLen])
		i += nameLen
		valueType := headers[i]
		i++
		switch valueType {
		case esTypeBoolTrue:
			msg.Headers[name] = "true"
		case esTypeBoolFalse:
			msg.Headers[name] = "false"
		case esTypeByte:
			i++
		case esTypeShort:
			i += 2
		case esTypeInteger:
			i += 4
		case esTypeLong, esTypeTimestamp:
			i += 8
		case esTypeUUID:
			i += 16
		case esTypeString, esTypeByteArray:
			if i+2 > len(headers) {
				i = len(headers)
				break
			}
			l := int(binary.BigEndian.Uint16(headers[i : i+2]))
			i += 2
			if i+l > len(headers) {
				i = len(headers)
				break
			}
			if valueType == esTypeString {
				msg.Headers[name] = string(headers[i : i+l])
			}
			i += l
		default:
			// Unknown type: the remainder cannot be parsed safely.
			i = len(headers)
		}
	}
	return msg, nil
}
