package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

const (
	defaultReadTimeout  = 70 * time.Second
	defaultWriteTimeout = 60 * time.Second
	defaultReadLimit    = int64(8 * 1024 * 1024)
)

type WebSocketTransport struct {
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

func NewWebSocketTransport() *WebSocketTransport {
	return &WebSocketTransport{
		ReadTimeout:  defaultReadTimeout,
		WriteTimeout: defaultWriteTimeout,
	}
}

func (t *WebSocketTransport) Dial(ctx context.Context, rawURL string) (Connection, error) {
	return dialWebSocket(ctx, rawURL, nil, t.readTimeout(), t.writeTimeout())
}

func (t *WebSocketTransport) Listen(addr string) (Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen websocket transport: %w", err)
	}

	l := &websocketListener{
		ln:    ln,
		conns: make(chan Connection, 64),
		errCh: make(chan error, 1),
		done:  make(chan struct{}),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := acceptWebSocket(w, r, t.readTimeout(), t.writeTimeout())
		if err != nil {
			return
		}

		select {
		case l.conns <- conn:
		case <-l.done:
			_ = conn.Close()
		}
	})

	l.server = &http.Server{Handler: mux}
	go func() {
		err := l.server.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case l.errCh <- err:
			default:
			}
		}
		close(l.conns)
	}()

	return l, nil
}

func DialWebSocket(ctx context.Context, rawURL string, headers http.Header) (*WebSocketConnection, error) {
	return dialWebSocket(ctx, rawURL, headers, defaultReadTimeout, defaultWriteTimeout)
}

func AcceptWebSocket(w http.ResponseWriter, r *http.Request) (*WebSocketConnection, error) {
	return acceptWebSocket(w, r, defaultReadTimeout, defaultWriteTimeout)
}

func dialWebSocket(
	ctx context.Context,
	rawURL string,
	headers http.Header,
	readTimeout time.Duration,
	writeTimeout time.Duration,
) (*WebSocketConnection, error) {
	dialOpts := &websocket.DialOptions{HTTPHeader: headers}
	wsConn, resp, err := websocket.Dial(ctx, rawURL, dialOpts)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("dial websocket failed with status %d: %w", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("dial websocket: %w", err)
	}

	remoteAddr := rawURL
	if parsed, parseErr := url.Parse(rawURL); parseErr == nil {
		remoteAddr = parsed.Host
	}

	return newWebSocketConnection(wsConn, remoteAddr, readTimeout, writeTimeout), nil
}

func acceptWebSocket(
	w http.ResponseWriter,
	r *http.Request,
	readTimeout time.Duration,
	writeTimeout time.Duration,
) (*WebSocketConnection, error) {
	wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return nil, fmt.Errorf("accept websocket: %w", err)
	}
	return newWebSocketConnection(wsConn, r.RemoteAddr, readTimeout, writeTimeout), nil
}

type WebSocketConnection struct {
	conn         *websocket.Conn
	remoteAddr   string
	readTimeout  time.Duration
	writeTimeout time.Duration
	writeMu      sync.Mutex
}

func newWebSocketConnection(conn *websocket.Conn, remoteAddr string, readTimeout, writeTimeout time.Duration) *WebSocketConnection {
	// nhooyr websocket defaults to a low read limit (~32KiB), which is too small
	// for proxied HTTP chunks and causes read-limited disconnects.
	conn.SetReadLimit(defaultReadLimit)

	return &WebSocketConnection{
		conn:         conn,
		remoteAddr:   remoteAddr,
		readTimeout:  readTimeout,
		writeTimeout: writeTimeout,
	}
}

func (c *WebSocketConnection) Send(msg Message) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), c.writeTimeout)
	defer cancel()

	payload, err := encodeMessagePayload(msg)
	if err != nil {
		return fmt.Errorf("encode websocket message: %w", err)
	}

	if err := c.conn.Write(ctx, websocket.MessageBinary, payload); err != nil {
		return fmt.Errorf("send websocket message: %w", err)
	}
	return nil
}

func (c *WebSocketConnection) Receive() (Message, error) {
	ctx := context.Background()
	cancel := func() {}
	if c.readTimeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), c.readTimeout)
	}
	defer cancel()

	msgType, payload, err := c.conn.Read(ctx)
	if err != nil {
		return Message{}, fmt.Errorf("receive websocket message: %w", err)
	}

	if msgType != websocket.MessageBinary {
		return Message{}, fmt.Errorf("expected binary frame")
	}

	msg, err := decodeMessagePayload(payload)
	if err != nil {
		return Message{}, fmt.Errorf("decode websocket message: %w", err)
	}

	return msg, nil
}

func (c *WebSocketConnection) Close() error {
	return c.conn.Close(websocket.StatusNormalClosure, "")
}

func (c *WebSocketConnection) RemoteAddr() string {
	return c.remoteAddr
}

type websocketListener struct {
	ln     net.Listener
	server *http.Server
	conns  chan Connection
	errCh  chan error
	done   chan struct{}
	once   sync.Once
}

func (l *websocketListener) Accept() (Connection, error) {
	select {
	case conn, ok := <-l.conns:
		if !ok {
			select {
			case err := <-l.errCh:
				if err != nil {
					return nil, err
				}
			default:
			}
			return nil, net.ErrClosed
		}
		return conn, nil
	case err := <-l.errCh:
		if err != nil {
			return nil, err
		}
		return nil, net.ErrClosed
	}
}

func (l *websocketListener) Close() error {
	var closeErr error
	l.once.Do(func() {
		close(l.done)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		closeErr = l.server.Shutdown(ctx)
		if err := l.ln.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
	})
	return closeErr
}

func (t *WebSocketTransport) readTimeout() time.Duration {
	if t.ReadTimeout <= 0 {
		return defaultReadTimeout
	}
	return t.ReadTimeout
}

func (t *WebSocketTransport) writeTimeout() time.Duration {
	if t.WriteTimeout <= 0 {
		return defaultWriteTimeout
	}
	return t.WriteTimeout
}
