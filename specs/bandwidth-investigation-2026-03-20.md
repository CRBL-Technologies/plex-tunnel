# Bandwidth Investigation Notes — 2026-03-20

## Scope

These notes summarize what we know from the March 20, 2026 investigation after adding the client-side send timing breakdown on `fix/bandwidth-send-breakdown`.

The relevant client fields are:

- `plex_read_ms`
- `tunnel_write_ms`
- `write_lock_wait_ms`
- `frame_encode_ms`
- `ws_write_ms`

Representative server fields from the earlier instrumentation run:

- `delivery_ms`
- `http_write_ms`

The excerpts below are trimmed for readability. Long Plex query strings are shortened with `...`.

## Short Conclusion

- Plex read time is not the bottleneck.
- Frame encoding is not the bottleneck.
- The server did not show downstream delivery pressure on the same 64 KiB stream chunks.
- The slow operation is the client-side websocket send path.
- Because all requests share one websocket guarded by `writeMu`, one slow websocket write turns into lock-wait latency for other concurrent requests.

## What The Server Logs Already Ruled Out

Representative 64 KiB server-side stream logs:

```text
2026-03-20T14:25:41Z DBG tunnel response delivery timing | component=server subdomain=antoine request_id=5f27... chunk_index=254 bytes=65536 end_stream=false delivery_ms=0
2026-03-20T14:25:41Z DBG proxied response write timing | component=server subdomain=antoine request_id=5f27... chunk_index=254 bytes=65536 end_stream=false status=206 http_write_ms=0
2026-03-20T14:25:42Z DBG tunnel response delivery timing | component=server subdomain=antoine request_id=0b93... chunk_index=419 bytes=65536 end_stream=false delivery_ms=0
2026-03-20T14:25:42Z DBG proxied response write timing | component=server subdomain=antoine request_id=5f27... chunk_index=271 bytes=65536 end_stream=false status=206 http_write_ms=0
```

Interpretation:

- `delivery_ms=0` means the server-side pending channel was not visibly backing up on these chunks.
- `http_write_ms=0` means the server-to-browser write path was not the visible bottleneck at millisecond resolution.

## What The New Client Logs Show

### 1. Plex reads are effectively free

Representative stream logs:

```text
2026-03-20T14:49:26Z DBG proxied response chunk timing | component=client request_id=c76c... path=/library/parts/.../file.mkv?... chunk_index=450 bytes=65536 plex_read_ms=0 tunnel_write_ms=0 write_lock_wait_ms=0 frame_encode_ms=0 ws_write_ms=0
2026-03-20T14:49:27Z DBG proxied response chunk timing | component=client request_id=c76c... path=/library/parts/.../file.mkv?... chunk_index=462 bytes=65536 plex_read_ms=1 tunnel_write_ms=240 write_lock_wait_ms=0 frame_encode_ms=0 ws_write_ms=239
```

Interpretation:

- `plex_read_ms` stays `0` or `1`, even when the send path is slow.
- Plex itself is not what is causing the large stalls.

### 2. Frame encoding is not the issue

Representative stream logs:

```text
2026-03-20T14:49:27Z DBG proxied response chunk timing | component=client request_id=ced1... path=/library/parts/.../file.mkv?... chunk_index=68 bytes=65536 tunnel_write_ms=199 write_lock_wait_ms=0 frame_encode_ms=0 ws_write_ms=199
2026-03-20T14:49:28Z DBG proxied response chunk timing | component=client request_id=69a3... path=/library/parts/.../file.mkv?... chunk_index=99 bytes=65536 tunnel_write_ms=1 write_lock_wait_ms=0 frame_encode_ms=1 ws_write_ms=0
```

Interpretation:

- `frame_encode_ms` is almost always `0`.
- There is no evidence that binary frame construction is the bandwidth limiter.

### 3. Slow websocket writes are real

Representative stream logs with high `ws_write_ms`:

```text
2026-03-20T14:49:27Z DBG proxied response chunk timing | component=client request_id=ced1... path=/library/parts/.../file.mkv?... chunk_index=63 bytes=65536 tunnel_write_ms=163 write_lock_wait_ms=1 frame_encode_ms=0 ws_write_ms=162
2026-03-20T14:49:27Z DBG proxied response chunk timing | component=client request_id=ced1... path=/library/parts/.../file.mkv?... chunk_index=68 bytes=65536 tunnel_write_ms=199 write_lock_wait_ms=0 frame_encode_ms=0 ws_write_ms=199
2026-03-20T14:49:27Z DBG proxied response chunk timing | component=client request_id=3ba9... path=/library/parts/.../file.mkv?... chunk_index=189 bytes=65536 tunnel_write_ms=288 write_lock_wait_ms=0 frame_encode_ms=0 ws_write_ms=288
2026-03-20T14:49:28Z DBG proxied response chunk timing | component=client request_id=69a3... path=/library/parts/.../file.mkv?... chunk_index=105 bytes=65536 tunnel_write_ms=344 write_lock_wait_ms=0 frame_encode_ms=0 ws_write_ms=344
```

Interpretation:

- The expensive part is sometimes the actual websocket write itself.
- Rough throughput implied by these examples:
  - 64 KiB / 162 ms is about 3.2 Mbps
  - 64 KiB / 288 ms is about 1.8 Mbps
  - 64 KiB / 344 ms is about 1.5 Mbps

This lines up with an observed slow-tunnel symptom.

### 4. `writeMu` head-of-line blocking is also real

Representative logs where `write_lock_wait_ms` is the dominant cost:

```text
2026-03-20T14:49:27Z DBG proxied response chunk timing | component=client request_id=3ba9... path=/library/parts/.../file.mkv?... chunk_index=180 bytes=65536 tunnel_write_ms=163 write_lock_wait_ms=162 frame_encode_ms=0 ws_write_ms=0
2026-03-20T14:49:27Z DBG proxied response chunk timing | component=client request_id=69a3... path=/library/parts/.../file.mkv?... chunk_index=86 bytes=65536 tunnel_write_ms=164 write_lock_wait_ms=163 frame_encode_ms=0 ws_write_ms=0
2026-03-20T14:49:27Z DBG proxied response chunk timing | component=client request_id=c76c... path=/library/parts/.../file.mkv?... chunk_index=452 bytes=65536 tunnel_write_ms=164 write_lock_wait_ms=164 frame_encode_ms=0 ws_write_ms=0
2026-03-20T14:49:28Z DBG proxied response chunk timing | component=client request_id=cb2e... path=/media/providers chunk_index=2 bytes=0 end_stream=true tunnel_write_ms=346 write_lock_wait_ms=346 frame_encode_ms=0 ws_write_ms=0
```

Interpretation:

- One slow write blocks unrelated in-flight requests behind `writeMu`.
- This affects both media requests and small control traffic such as `/media/providers`.
- The control path is therefore being starved by the same shared websocket connection.

## Current Best Explanation

The evidence now points to a two-part effect:

1. The primary slow operation is the client-to-server websocket write path itself (`ws_write_ms` spikes).
2. The shared websocket plus global `writeMu` turns those slow writes into head-of-line blocking for every other concurrent request (`write_lock_wait_ms` spikes).

That means:

- The user-visible slowdown is not being created by Plex read speed.
- The user-visible slowdown is not being created by frame encoding.
- The user-visible slowdown is not primarily being created after the server receives the chunk.

## What We Still Do Not Know

- Why the websocket writes are slow:
  - raw network throughput between client host and server host
  - TCP backpressure
  - container/host resource limits
  - platform-specific network path issues
- Whether larger response chunks materially improve throughput by reducing write frequency and lock contention.
- Whether the same `ws_write_ms` spikes still occur under a single isolated stream with no competing requests.

## Recommended Next Tests

1. Run a single-stream playback test with no parallel browsing or metadata traffic.
   - If `ws_write_ms` still spikes, the raw websocket path is the direct bottleneck.
   - If the spikes mostly disappear, contention is a major part of the problem.

2. Compare chunk sizes:
   - `PLEXTUNNEL_RESPONSE_CHUNK_SIZE=262144`
   - `PLEXTUNNEL_RESPONSE_CHUNK_SIZE=524288`

3. Capture client-host network and resource metrics during a reproduction.
   - interface throughput
   - packet loss/retransmits
   - CPU throttling
   - container limits

4. If raw websocket write remains the limit, consider architectural changes:
   - separate media and control traffic
   - multiple websocket connections per client session
   - a different bulk transport for large media streams

## Practical Summary

At this point, the investigation has moved past "is the issue Plex?" and "is the issue server delivery?".

The answer from the current logs is:

- no, Plex is not the limiting factor
- no, frame encoding is not the limiting factor
- no, the server write path is not the limiting factor
- yes, the client websocket send path is where the slowdown starts
- yes, `writeMu` amplifies that slowdown across concurrent requests
