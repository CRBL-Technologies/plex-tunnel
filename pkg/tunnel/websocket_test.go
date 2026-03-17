package tunnel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
)

func TestWebSocketConnectionReceiveRejectsTextFrames(t *testing.T) {
	serverErrCh := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			serverErrCh <- err
			return
		}
		defer wsConn.Close(websocket.StatusNormalClosure, "")

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := wsConn.Write(ctx, websocket.MessageText, []byte(`{"type":5}`)); err != nil {
			serverErrCh <- err
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	clientConn, err := DialWebSocket(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("DialWebSocket() error = %v", err)
	}
	defer clientConn.Close()

	_, err = clientConn.Receive()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "expected binary frame") {
		t.Fatalf("expected \"expected binary frame\" error, got %v", err)
	}

	select {
	case err := <-serverErrCh:
		t.Fatalf("server error: %v", err)
	default:
	}
}
