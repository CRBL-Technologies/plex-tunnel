package tunnel

import (
	"errors"
	"fmt"
	"net/http"
)

type MessageType uint8

const (
	ProtocolVersion uint16 = 1
)

const (
	MsgRegister MessageType = iota + 1
	MsgRegisterAck
	MsgHTTPRequest
	MsgHTTPResponse
	MsgPing
	MsgPong
	MsgError
	MsgWSOpen
	MsgWSFrame
	MsgWSClose
	MsgKeyExchange
)

type Message struct {
	Type            MessageType         `json:"type"`
	ID              string              `json:"id,omitempty"`
	Token           string              `json:"token,omitempty"`
	Subdomain       string              `json:"subdomain,omitempty"`
	ProtocolVersion uint16              `json:"protocol_version,omitempty"`
	Method          string              `json:"method,omitempty"`
	Path            string              `json:"path,omitempty"`
	Headers         map[string][]string `json:"headers,omitempty"`
	Body            []byte              `json:"-"`
	Status          int                 `json:"status,omitempty"`
	EndStream       bool                `json:"end_stream,omitempty"`
	Error           string              `json:"error,omitempty"`
}

func (m Message) Validate() error {
	switch m.Type {
	case MsgRegister:
		if m.Token == "" {
			return errors.New("register message missing token")
		}
		if m.ProtocolVersion == 0 {
			return errors.New("register message missing protocol_version")
		}
	case MsgRegisterAck:
		if m.Subdomain == "" {
			return errors.New("register ack missing subdomain")
		}
		if m.ProtocolVersion == 0 {
			return errors.New("register ack missing protocol_version")
		}
	case MsgHTTPRequest:
		if m.ID == "" {
			return errors.New("http request message missing id")
		}
		if m.Method == "" {
			return errors.New("http request message missing method")
		}
		if m.Path == "" {
			return errors.New("http request message missing path")
		}
	case MsgHTTPResponse:
		if m.ID == "" {
			return errors.New("http response message missing id")
		}
		if m.Status < 0 {
			return fmt.Errorf("invalid http response status: %d", m.Status)
		}
	case MsgPing, MsgPong:
		return nil
	case MsgError:
		if m.Error == "" {
			return errors.New("error message missing body")
		}
	case MsgWSOpen, MsgWSFrame, MsgWSClose:
		if m.ID == "" {
			return errors.New("websocket message missing id")
		}
	case MsgKeyExchange:
		return nil
	default:
		return fmt.Errorf("unknown message type: %d", m.Type)
	}

	return nil
}

func CloneHeaders(headers http.Header) map[string][]string {
	if len(headers) == 0 {
		return nil
	}

	out := make(map[string][]string, len(headers))
	for k, v := range headers {
		vals := make([]string, len(v))
		copy(vals, v)
		out[k] = vals
	}
	return out
}
