package adapter

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// eventStreamMessage is one decoded AWS event-stream frame (the
// application/vnd.amazon.eventstream wire format Bedrock's converse-stream
// answers in). Only string-valued headers are retained; those are the ones
// that matter (:event-type, :message-type, :exception-type).
type eventStreamMessage struct {
	Headers map[string]string
	Payload []byte
}

// eventStreamDecoder reassembles frames from arbitrarily chunked input. The
// proxy relays upstream bodies split at '\n', which is meaningless for a
// binary framing, so the decoder buffers whatever it is fed and yields every
// complete frame each time.
//
// Frame layout (all integers big-endian):
//
//	total_length u32 | headers_length u32 | prelude_crc u32
//	headers…  (headers_length bytes)
//	payload…  (total_length - headers_length - 16 bytes)
//	message_crc u32
type eventStreamDecoder struct {
	buf []byte
}

const eventStreamPreludeLen = 12
const eventStreamOverhead = eventStreamPreludeLen + 4 // prelude + message CRC

// Feed appends chunk and returns every frame that is now complete. A CRC or
// structural violation is unrecoverable — the stream is out of sync — and is
// reported as an error.
func (d *eventStreamDecoder) Feed(chunk []byte) ([]eventStreamMessage, error) {
	d.buf = append(d.buf, chunk...)
	var out []eventStreamMessage
	for {
		if len(d.buf) < eventStreamPreludeLen {
			return out, nil
		}
		total := binary.BigEndian.Uint32(d.buf[0:4])
		headersLen := binary.BigEndian.Uint32(d.buf[4:8])
		preludeCRC := binary.BigEndian.Uint32(d.buf[8:12])
		if crc32.ChecksumIEEE(d.buf[0:8]) != preludeCRC {
			return out, fmt.Errorf("event-stream prelude checksum mismatch")
		}
		if total < eventStreamOverhead || headersLen > total-eventStreamOverhead {
			return out, fmt.Errorf("event-stream frame declares impossible lengths (total=%d headers=%d)", total, headersLen)
		}
		if uint32(len(d.buf)) < total {
			return out, nil // wait for the rest of this frame
		}
		frame := d.buf[:total]
		messageCRC := binary.BigEndian.Uint32(frame[total-4:])
		if crc32.ChecksumIEEE(frame[:total-4]) != messageCRC {
			return out, fmt.Errorf("event-stream message checksum mismatch")
		}
		headers, err := parseEventStreamHeaders(frame[eventStreamPreludeLen : eventStreamPreludeLen+headersLen])
		if err != nil {
			return out, err
		}
		payload := make([]byte, total-eventStreamOverhead-headersLen)
		copy(payload, frame[eventStreamPreludeLen+headersLen:total-4])
		out = append(out, eventStreamMessage{Headers: headers, Payload: payload})
		d.buf = d.buf[total:]
	}
}

// parseEventStreamHeaders decodes the header block. Header value types:
// 0/1 bool, 2 byte, 3 short, 4 int, 5 long, 6 byte-buffer, 7 string,
// 8 timestamp, 9 uuid.
func parseEventStreamHeaders(b []byte) (map[string]string, error) {
	headers := map[string]string{}
	for len(b) > 0 {
		nameLen := int(b[0])
		if len(b) < 1+nameLen+1 {
			return nil, fmt.Errorf("event-stream header truncated")
		}
		name := string(b[1 : 1+nameLen])
		valueType := b[1+nameLen]
		b = b[2+nameLen:]
		var size int
		switch valueType {
		case 0, 1:
			size = 0
		case 2:
			size = 1
		case 3:
			size = 2
		case 4:
			size = 4
		case 5, 8:
			size = 8
		case 9:
			size = 16
		case 6, 7:
			if len(b) < 2 {
				return nil, fmt.Errorf("event-stream header truncated")
			}
			size = int(binary.BigEndian.Uint16(b[0:2]))
			b = b[2:]
		default:
			return nil, fmt.Errorf("event-stream header %q has unknown value type %d", name, valueType)
		}
		if len(b) < size {
			return nil, fmt.Errorf("event-stream header truncated")
		}
		if valueType == 7 {
			headers[name] = string(b[:size])
		}
		b = b[size:]
	}
	return headers, nil
}

// encodeEventStreamMessage builds one frame with string headers. It exists
// for tests (and any future encoder use); the gateway never speaks
// event-stream upstream itself.
func encodeEventStreamMessage(headers map[string]string, payload []byte) []byte {
	var hdr []byte
	for name, value := range headers {
		hdr = append(hdr, byte(len(name)))
		hdr = append(hdr, name...)
		hdr = append(hdr, 7)
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(len(value)))
		hdr = append(hdr, value...)
	}
	total := uint32(eventStreamOverhead + len(hdr) + len(payload))
	frame := make([]byte, 0, total)
	frame = binary.BigEndian.AppendUint32(frame, total)
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(hdr)))
	frame = binary.BigEndian.AppendUint32(frame, crc32.ChecksumIEEE(frame[:8]))
	frame = append(frame, hdr...)
	frame = append(frame, payload...)
	frame = binary.BigEndian.AppendUint32(frame, crc32.ChecksumIEEE(frame))
	return frame
}
