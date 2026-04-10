package client

import (
	"strings"
)

// RouteClass distinguishes traffic that runs on the singleton control
// WebSocket from traffic that leases a data WebSocket exclusively.
type RouteClass int

const (
	RouteClassData    RouteClass = iota // leases a data tunnel exclusively
	RouteClassControl                   // multiplexes on the control tunnel
)

func (c RouteClass) String() string {
	switch c {
	case RouteClassControl:
		return "control"
	default:
		return "data"
	}
}

// ClassifyRequest returns the route class for an inbound proxied request.
// This function MUST stay byte-for-byte in sync with the server classifier
// at plex-tunnel-server/pkg/server/classifier.go - any divergence is a bug
// and will cause request starvation or cross-lane contamination.
//
// Unmatched paths default to data by design: never risk surprise large
// bodies on the control semaphore. Widen the control allowlist only from
// observed traffic captures AND update the server classifier in the same
// cycle.
func ClassifyRequest(method, path string) RouteClass {
	class, _ := classifyRequest(method, path)

	return class
}

func classifyRequest(method, path string) (RouteClass, bool) {
	method = strings.ToUpper(strings.TrimSpace(method))

	path = normalizeClassifyPath(path)

	if method == "GET" {
		switch {
		case strings.HasPrefix(path, "/:/eventsource/"):
			return RouteClassControl, true

		case strings.HasPrefix(path, "/:/websockets/notifications"):
			return RouteClassControl, true

		case path == "/identity":
			return RouteClassControl, true

		case pathHasLiteralPrefix(path, "/media/providers"):
			return RouteClassControl, true

		case pathHasIDPrefix(path, "/library/metadata/"):
			return RouteClassControl, true

		case pathHasLiteralPrefix(path, "/library/sections"):
			return RouteClassControl, true

		case pathHasLiteralPrefix(path, "/hubs"):
			return RouteClassControl, true

		case pathHasLiteralPrefix(path, "/status/sessions"):
			return RouteClassControl, true

		case isDownloadQueueMediaPath(path):
			return RouteClassData, true

		case pathHasIDPrefix(path, "/library/parts/"):
			return RouteClassData, true

		case strings.HasPrefix(path, "/video/:/transcode/universal/decision"):
			return RouteClassControl, true

		case strings.HasPrefix(path, "/video/:/transcode/"):
			return RouteClassData, true

		case strings.HasPrefix(path, "/photo/:/transcode/"):
			return RouteClassData, true
		}
	}

	if method == "POST" && pathHasLiteralPrefix(path, "/:/timeline") {
		return RouteClassControl, true
	}

	return RouteClassData, false
}

func normalizeClassifyPath(path string) string {
	path = strings.TrimSpace(path)

	if path == "" {
		return "/"
	}

	if idx := strings.IndexByte(path, '?'); idx >= 0 {
		path = path[:idx]
	}

	return strings.ToLower(path)
}

func pathHasLiteralPrefix(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func pathHasIDPrefix(path, prefix string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}

	return strings.TrimPrefix(path, prefix) != ""
}

func isDownloadQueueMediaPath(path string) bool {
	if !strings.HasPrefix(path, "/downloadqueue/") {
		return false
	}

	rest := strings.TrimPrefix(path, "/downloadqueue/")
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return false
	}

	return strings.HasPrefix(rest[slash:], "/media")
}
