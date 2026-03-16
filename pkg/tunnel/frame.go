package tunnel

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
)

const frameHeaderSize = 9

const maxFrameSectionLength = uint64(^uint32(0))

type Frame struct {
	Type     MessageType
	Metadata []byte
	Body     []byte
}

func NewFrame(msg Message) (Frame, error) {
	metadataMessage := msg
	metadataMessage.Body = nil

	metadata, err := json.Marshal(metadataMessage)
	if err != nil {
		return Frame{}, fmt.Errorf("marshal frame metadata: %w", err)
	}

	body := append([]byte(nil), msg.Body...)
	return Frame{
		Type:     msg.Type,
		Metadata: metadata,
		Body:     body,
	}, nil
}

func (f Frame) MarshalBinary() ([]byte, error) {
	if uint64(len(f.Metadata)) > maxFrameSectionLength {
		return nil, fmt.Errorf("metadata too large: %d bytes", len(f.Metadata))
	}
	if uint64(len(f.Body)) > maxFrameSectionLength {
		return nil, fmt.Errorf("body too large: %d bytes", len(f.Body))
	}

	payload := make([]byte, frameHeaderSize+len(f.Metadata)+len(f.Body))
	payload[0] = byte(f.Type)
	binary.BigEndian.PutUint32(payload[1:5], uint32(len(f.Metadata)))
	binary.BigEndian.PutUint32(payload[5:9], uint32(len(f.Body)))

	copy(payload[frameHeaderSize:], f.Metadata)
	copy(payload[frameHeaderSize+len(f.Metadata):], f.Body)

	return payload, nil
}

func UnmarshalFrame(payload []byte) (Frame, error) {
	if len(payload) < frameHeaderSize {
		return Frame{}, fmt.Errorf("frame too short")
	}

	msgType := MessageType(payload[0])
	metadataLen := uint64(binary.BigEndian.Uint32(payload[1:5]))
	bodyLen := uint64(binary.BigEndian.Uint32(payload[5:9]))
	expectedLen := uint64(frameHeaderSize) + metadataLen + bodyLen
	if expectedLen != uint64(len(payload)) {
		return Frame{}, fmt.Errorf("frame length mismatch")
	}

	metadataStart := frameHeaderSize
	metadataEnd := metadataStart + int(metadataLen)
	bodyEnd := metadataEnd + int(bodyLen)

	metadata := make([]byte, int(metadataLen))
	copy(metadata, payload[metadataStart:metadataEnd])

	body := make([]byte, int(bodyLen))
	copy(body, payload[metadataEnd:bodyEnd])

	return Frame{
		Type:     msgType,
		Metadata: metadata,
		Body:     body,
	}, nil
}

func encodeMessagePayload(msg Message) ([]byte, error) {
	if err := msg.Validate(); err != nil {
		return nil, fmt.Errorf("validate message: %w", err)
	}

	frame, err := NewFrame(msg)
	if err != nil {
		return nil, err
	}
	return frame.MarshalBinary()
}

func decodeMessagePayload(payload []byte) (Message, error) {
	frame, err := UnmarshalFrame(payload)
	if err != nil {
		return Message{}, err
	}

	var msg Message
	if len(frame.Metadata) > 0 {
		if err := json.Unmarshal(frame.Metadata, &msg); err != nil {
			return Message{}, fmt.Errorf("decode frame metadata: %w", err)
		}
	}

	if msg.Type != 0 && msg.Type != frame.Type {
		return Message{}, fmt.Errorf("frame type mismatch")
	}

	msg.Type = frame.Type
	msg.Body = frame.Body

	if err := msg.Validate(); err != nil {
		return Message{}, fmt.Errorf("validate message: %w", err)
	}

	return msg, nil
}
