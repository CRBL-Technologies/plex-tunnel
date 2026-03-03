package tunnel

import "context"

type Transport interface {
	Dial(ctx context.Context, url string) (Connection, error)
	Listen(addr string) (Listener, error)
}

type Connection interface {
	Send(msg Message) error
	Receive() (Message, error)
	Close() error
	RemoteAddr() string
}

type Listener interface {
	Accept() (Connection, error)
	Close() error
}
