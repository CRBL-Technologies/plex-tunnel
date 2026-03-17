package tunnel

import (
	"bytes"
	"encoding/json"
	"testing"
)

type legacyJSONMessage struct {
	Type   MessageType `json:"type"`
	ID     string      `json:"id,omitempty"`
	Status int         `json:"status,omitempty"`
	Body   []byte      `json:"body,omitempty"`
}

func BenchmarkBinaryFrameVsLegacyJSON(b *testing.B) {
	body := bytes.Repeat([]byte("a"), 256*1024)
	msg := Message{
		Type:   MsgHTTPResponse,
		ID:     "bench-request",
		Status: 200,
		Body:   body,
	}

	b.Run("binary", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			payload, err := encodeMessagePayload(msg)
			if err != nil {
				b.Fatalf("encodeMessagePayload() error = %v", err)
			}
			if _, err := decodeMessagePayload(payload); err != nil {
				b.Fatalf("decodeMessagePayload() error = %v", err)
			}
		}
	})

	b.Run("legacy-json-base64", func(b *testing.B) {
		b.ReportAllocs()
		legacy := legacyJSONMessage{
			Type:   msg.Type,
			ID:     msg.ID,
			Status: msg.Status,
			Body:   body,
		}
		for i := 0; i < b.N; i++ {
			payload, err := json.Marshal(legacy)
			if err != nil {
				b.Fatalf("json.Marshal() error = %v", err)
			}

			var decoded legacyJSONMessage
			if err := json.Unmarshal(payload, &decoded); err != nil {
				b.Fatalf("json.Unmarshal() error = %v", err)
			}
		}
	})
}
