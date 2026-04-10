package client

import "testing"

func TestClassifyRequest(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		path      string
		wantClass RouteClass
	}{
		{name: "control get eventsource notifications", method: "GET", path: "/:/eventsource/notifications", wantClass: RouteClassControl},
		{name: "control get websockets notifications", method: "GET", path: "/:/websockets/notifications", wantClass: RouteClassControl},
		{name: "control get websockets notifications prefix", method: "GET", path: "/:/websockets/notifications/foo", wantClass: RouteClassControl},
		{name: "control get identity", method: "GET", path: "/identity", wantClass: RouteClassControl},
		{name: "control get media providers", method: "GET", path: "/media/providers", wantClass: RouteClassControl},
		{name: "control get media providers child", method: "GET", path: "/media/providers/123", wantClass: RouteClassControl},
		{name: "control get library metadata id", method: "GET", path: "/library/metadata/456", wantClass: RouteClassControl},
		{name: "control get library sections", method: "GET", path: "/library/sections", wantClass: RouteClassControl},
		{name: "control get library sections nested", method: "GET", path: "/library/sections/1/all", wantClass: RouteClassControl},
		{name: "control get hubs", method: "GET", path: "/hubs", wantClass: RouteClassControl},
		{name: "control get hubs nested", method: "GET", path: "/hubs/continueWatching", wantClass: RouteClassControl},
		{name: "control get status sessions", method: "GET", path: "/status/sessions", wantClass: RouteClassControl},
		{name: "control get transcode universal decision", method: "GET", path: "/video/:/transcode/universal/decision", wantClass: RouteClassControl},
		{name: "control get transcode universal decision query", method: "GET", path: "/video/:/transcode/universal/decision?foo=bar", wantClass: RouteClassControl},
		{name: "control post timeline", method: "POST", path: "/:/timeline", wantClass: RouteClassControl},
		{name: "control post timeline nested", method: "POST", path: "/:/timeline/foo", wantClass: RouteClassControl},
		{name: "data explicit download queue media", method: "GET", path: "/downloadQueue/abc/media", wantClass: RouteClassData},
		{name: "data explicit download queue media file", method: "GET", path: "/downloadQueue/abc/media/1.mp4", wantClass: RouteClassData},
		{name: "data explicit library parts file", method: "GET", path: "/library/parts/9999/file.mkv", wantClass: RouteClassData},
		{name: "data explicit transcode universal start", method: "GET", path: "/video/:/transcode/universal/start", wantClass: RouteClassData},
		{name: "data explicit transcode universal playlist", method: "GET", path: "/video/:/transcode/universal/start.m3u8", wantClass: RouteClassData},
		{name: "data explicit photo transcode", method: "GET", path: "/photo/:/transcode/foo", wantClass: RouteClassData},
		{name: "data default root", method: "GET", path: "/", wantClass: RouteClassData},
		{name: "data default unknown path", method: "GET", path: "/some/unknown/path", wantClass: RouteClassData},
		{name: "data default wrong method metadata", method: "POST", path: "/library/metadata/456", wantClass: RouteClassData},
		{name: "data default wrong method parts", method: "DELETE", path: "/library/parts/99/file.mkv", wantClass: RouteClassData},
		{name: "data default metadata no id", method: "GET", path: "/library/metadata", wantClass: RouteClassData},
		{name: "data default parts no id", method: "GET", path: "/library/parts", wantClass: RouteClassData},
		{name: "data default similar parts prefix", method: "GET", path: "/library/partss", wantClass: RouteClassData},
		{name: "data default download queue no media", method: "GET", path: "/downloadQueue/abc", wantClass: RouteClassData},
		{name: "data default download queue metadata", method: "GET", path: "/downloadQueue/abc/metadata", wantClass: RouteClassData},
		{name: "normalization path lowercased", method: "GET", path: "/LIBRARY/METADATA/456", wantClass: RouteClassControl},
		{name: "normalization query stripped metadata", method: "GET", path: "/library/metadata/456?X-Plex-Token=secret", wantClass: RouteClassControl},
		{name: "normalization method lowercased", method: "get", path: "/hubs", wantClass: RouteClassControl},
		{name: "normalization method trimmed", method: "  GET  ", path: "/hubs", wantClass: RouteClassControl},
		{name: "normalization query stripped data", method: "GET", path: "/library/parts/9999/file.mkv?download=1", wantClass: RouteClassData},
		{name: "normalization empty path defaults", method: "GET", path: "", wantClass: RouteClassData},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ClassifyRequest(test.method, test.path)
			if got != test.wantClass {
				t.Fatalf("ClassifyRequest(%q, %q) = %v, want %v", test.method, test.path, got, test.wantClass)
			}
		})
	}
}
