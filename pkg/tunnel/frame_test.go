package tunnel

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	original := Message{
		Type:      MsgHTTPResponse,
		ID:        "req-1",
		Status:    200,
		Headers:   map[string][]string{"Content-Type": {"application/octet-stream"}},
		EndStream: true,
		Body:      []byte("hello binary world"),
	}

	payload, err := encodeMessagePayload(original)
	if err != nil {
		t.Fatalf("encodeMessagePayload() error = %v", err)
	}

	decoded, err := decodeMessagePayload(payload)
	if err != nil {
		t.Fatalf("decodeMessagePayload() error = %v", err)
	}

	if decoded.Type != original.Type {
		t.Fatalf("decoded.Type = %v, want %v", decoded.Type, original.Type)
	}
	if decoded.ID != original.ID {
		t.Fatalf("decoded.ID = %q, want %q", decoded.ID, original.ID)
	}
	if decoded.Status != original.Status {
		t.Fatalf("decoded.Status = %d, want %d", decoded.Status, original.Status)
	}
	if !decoded.EndStream {
		t.Fatalf("decoded.EndStream = false, want true")
	}
	if !bytes.Equal(decoded.Body, original.Body) {
		t.Fatalf("decoded.Body mismatch: got %v want %v", decoded.Body, original.Body)
	}
	if got, want := decoded.Headers["Content-Type"], original.Headers["Content-Type"]; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("decoded.Headers mismatch: got %v want %v", decoded.Headers, original.Headers)
	}
}

func TestFrameEmptyBodyRoundTrip(t *testing.T) {
	original := Message{Type: MsgPing}

	payload, err := encodeMessagePayload(original)
	if err != nil {
		t.Fatalf("encodeMessagePayload() error = %v", err)
	}

	decoded, err := decodeMessagePayload(payload)
	if err != nil {
		t.Fatalf("decodeMessagePayload() error = %v", err)
	}

	if decoded.Type != MsgPing {
		t.Fatalf("decoded.Type = %v, want %v", decoded.Type, MsgPing)
	}
	if len(decoded.Body) != 0 {
		t.Fatalf("decoded.Body length = %d, want 0", len(decoded.Body))
	}
}

func TestFrameBinaryBodyRoundTrip(t *testing.T) {
	original := Message{
		Type:   MsgHTTPResponse,
		ID:     "req-2",
		Status: 200,
		Body:   []byte{0x00, 0x7f, 0x80, 0xff},
	}

	payload, err := encodeMessagePayload(original)
	if err != nil {
		t.Fatalf("encodeMessagePayload() error = %v", err)
	}

	decoded, err := decodeMessagePayload(payload)
	if err != nil {
		t.Fatalf("decodeMessagePayload() error = %v", err)
	}

	if !bytes.Equal(decoded.Body, original.Body) {
		t.Fatalf("decoded.Body mismatch: got %v want %v", decoded.Body, original.Body)
	}
}

func TestFrameMetadataOmitsBody(t *testing.T) {
	frame, err := NewFrame(Message{
		Type:   MsgHTTPResponse,
		ID:     "req-3",
		Status: 200,
		Body:   []byte("secret-bytes"),
	})
	if err != nil {
		t.Fatalf("NewFrame() error = %v", err)
	}

	var metadata map[string]any
	if err := json.Unmarshal(frame.Metadata, &metadata); err != nil {
		t.Fatalf("json.Unmarshal(frame.Metadata) error = %v", err)
	}

	if _, ok := metadata["body"]; ok {
		t.Fatalf("metadata unexpectedly contains body field: %v", metadata)
	}
}

func TestDecodeMessagePayloadBufferIsolation(t *testing.T) {
	original := Message{
		Type:   MsgHTTPRequest,
		ID:     "req-4",
		Method: "POST",
		Path:   "/library/parts/1",
		Body:   []byte("abc123"),
	}

	payload, err := encodeMessagePayload(original)
	if err != nil {
		t.Fatalf("encodeMessagePayload() error = %v", err)
	}

	decoded, err := decodeMessagePayload(payload)
	if err != nil {
		t.Fatalf("decodeMessagePayload() error = %v", err)
	}

	wantPath := decoded.Path
	wantBody := append([]byte(nil), decoded.Body...)
	for i := range payload {
		payload[i] = 0xff
	}

	if decoded.Path != wantPath {
		t.Fatalf("decoded.Path changed after payload mutation: got %q want %q", decoded.Path, wantPath)
	}
	if !bytes.Equal(decoded.Body, wantBody) {
		t.Fatalf("decoded.Body changed after payload mutation: got %v want %v", decoded.Body, wantBody)
	}
}

func TestDecodeMessagePayloadLengthMismatch(t *testing.T) {
	payload, err := encodeMessagePayload(Message{
		Type:   MsgHTTPResponse,
		ID:     "req-5",
		Status: 200,
		Body:   []byte("payload"),
	})
	if err != nil {
		t.Fatalf("encodeMessagePayload() error = %v", err)
	}

	declaredBodyLen := binary.BigEndian.Uint32(payload[5:9])
	binary.BigEndian.PutUint32(payload[5:9], declaredBodyLen+1)

	_, err = decodeMessagePayload(payload)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "frame length mismatch") {
		t.Fatalf("expected frame length mismatch error, got %v", err)
	}
}

func TestDecodeMessagePayloadFrameTooShort(t *testing.T) {
	_, err := decodeMessagePayload([]byte{1, 2, 3})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "frame too short") {
		t.Fatalf("expected frame too short error, got %v", err)
	}
}
