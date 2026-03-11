package client

import "time"

// ConnectionStatus is the runtime state exposed by the client process.
type ConnectionStatus struct {
	Connected          bool      `json:"connected"`
	Server             string    `json:"server"`
	Subdomain          string    `json:"subdomain"`
	LastError          string    `json:"last_error,omitempty"`
	ReconnectAttempt   int       `json:"reconnect_attempt"`
	LastConnectedAt    time.Time `json:"last_connected_at,omitempty"`
	LastDisconnectedAt time.Time `json:"last_disconnected_at,omitempty"`
}
