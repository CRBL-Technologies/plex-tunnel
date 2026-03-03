package tunnel

import (
	"net/http"
	"testing"
)

func TestMessageValidate(t *testing.T) {
	tests := []struct {
		name    string
		msg     Message
		wantErr bool
	}{
		{
			name: "register ok",
			msg: Message{
				Type:  MsgRegister,
				Token: "abc",
			},
		},
		{
			name: "register missing token",
			msg: Message{
				Type: MsgRegister,
			},
			wantErr: true,
		},
		{
			name: "http request ok",
			msg: Message{
				Type:   MsgHTTPRequest,
				ID:     "req1",
				Method: http.MethodGet,
				Path:   "/",
			},
		},
		{
			name: "unknown type",
			msg: Message{
				Type: 100,
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.msg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil error, got %v", err)
			}
		})
	}
}

func TestCloneHeaders(t *testing.T) {
	h := http.Header{"X-Test": {"a", "b"}}
	cloned := CloneHeaders(h)
	cloned["X-Test"][0] = "changed"

	if h.Get("X-Test") != "a" {
		t.Fatalf("expected original header to stay unchanged, got %q", h.Get("X-Test"))
	}
}
