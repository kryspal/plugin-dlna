# dlna: DLNA cast plugin

An official Kryspal Media plugin that casts to DLNA/UPnP MediaRenderer devices. It implements the protocol v1 `CastTarget` service (capability `cast.target`, permission `network.lan`, `cast_protocol` `dlna`) with [`github.com/kryspal/plugin-sdk`](https://github.com/kryspal/plugin-sdk). It is **disabled by default**, because an enabled plugin sends multicast on the local network. `VERSION` is the single source of the version (`go:embed`).

The host owns the device list, the cast session, the authorization behind the media URL and play tracking; the plugin owns the protocol.

## What it does

- **Discover**: SSDP `M-SEARCH` from every up, multicast-capable IPv4 interface (so a host on two networks reaches both), for `MediaRenderer:1` and `ssdp:all` (some devices answer only one), plus a **unicast** `M-SEARCH` to each host the administrator added by hand (works through a container's NAT and across subnets, where multicast does not). Each distinct `LOCATION` is read (`goupnp.DeviceByURL`, 10 at a time, 3 s each) and kept when the device has an `AVTransport:1` service. `Device.id` is the UDN, `handle` is `{"loc": <description URL>}`.
- **Probe**: the unicast search for one host (`host` or `host:port`).
- **Prepare**: the content type (from the container, or the `mime=` option) and the headers the host must put on the media responses: `transferMode.dlna.org: Streaming` and `contentFeatures.dlna.org` (`DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=0170…`, the same as the `protocolInfo` of the DIDL; no `DLNA.ORG_PN`, no `OP=1x` because time seek is not implemented). DLNA cannot ask a television what it decodes, so Prepare never refuses; it warns (`cast_codec_risky`: HDR, Dolby Vision, 10-bit, AV1, VP9, DTS, TrueHD; `cast_subtitle_style_lost`).
- **Load**: `Stop` (some televisions answer 701 to `SetAVTransportURI` while playing) → `SetAVTransportURI` with a DIDL-Lite variant → poll `GetTransportInfo` until it leaves `TRANSITIONING` (no fixed delay) → `Play` (one retry) → wait for `PLAYING` → `Seek` to the start position (`REL_TIME`, whole seconds). A device that answers a SOAP fault to the metadata is asked again with simpler metadata: **full** (poster, Samsung `sec:CaptionInfoEx`/`CaptionInfo`, a `text/srt` `<res>`, `pv:` subtitle attributes) → **lite** (title, class and `<res>` only) → **empty**. The variant that worked is kept in `Device.state` (`{"didl":"lite"}`, stored by the host), so the next cast goes straight to it. A device that refuses everything, or never leaves `STOPPED`, is answered with `rejected_code` (`renderer_rejected`, `renderer_stopped`), not an RPC error; an unreachable device is `UNAVAILABLE`.
- **Control**: play, pause, stop, seek (`REL_TIME`, falling back to `ABS_TIME`), volume and mute (`RenderingControl`, only when the device has it).
- **Status**: `GetTransportInfo` → state (`PLAYING`, `PAUSED_PLAYBACK`, `TRANSITIONING`, `STOPPED`, `NO_MEDIA_PRESENT`), `GetPositionInfo` → position and duration, volume and mute when available; `ended` when it stops at the end: by the device's own position, or (a renderer that rewinds to 0 when it stops) by the last position seen while playing being within 3% of the duration (2 to 10 s) of the end; the last position seen is then reported as the position.

## SOAP quirks handled

`SOAPAction` with the quoted URN and `Content-Type: text/xml; charset="utf-8"` (goupnp); no redirects (control URLs are used on the host of the description); no keep-alive (some devices close the connection after each answer); a 405 to `POST` is retried as `M-POST` with the `MAN` header and `01-SOAPACTION`; only `<`, `>`, `&` are escaped in the embedded DIDL (goupnp), which televisions that reject escaped quotes accept; the DIDL always has the full `<DIDL-Lite>` root.

## Per-device options (the administrator's text, passed with every call)

Lines or `;`-separated `key=value`: `mime=video/mpeg` (content type), `features=none` (send `*` as the fourth `protocolInfo` field and no `contentFeatures` header), `didl=full|lite|empty` (force a variant), `subtitle=none` (do not describe the subtitle). Unknown keys are ignored.

## Not done (first version)

GENA event subscriptions (state is polled), `CaptionInfo.sec` response header (the subtitle URL does not exist yet when the host calls Prepare; the DIDL carries it), per-brand preset profiles beyond the remembered variant, AVTransport:2 and RenderingControl:2 only devices, multi-subtitle.

## Building and releasing

Releases are built by the CI workflows in `.github/workflows/`: a `v*` tag builds linux/amd64 and linux/arm64 packages and creates a GitHub Release. Any change that alters the binary needs a new `VERSION`; a published version is never changed. `CATALOG.json` holds the display data for the plugin catalog.

## Dependencies

`github.com/huin/goupnp` v1.3.0 (BSD-2-Clause): SSDP search, device descriptions, the AVTransport/RenderingControl SOAP clients. The copyright notice is in `NOTICE`.

## Tests

`go test -race ./...`: a fake MediaRenderer (description document, the two control endpoints with a state machine, switches for refused DIDL, 405 on POST, a Seek fault) covers Load order and DIDL content, the fallback and its memory, refused and never-playing devices, an unreachable device, control and status mapping, the `M-POST` retry, and unicast probing against a fake SSDP responder. `integration_test.go` runs the whole cast against a real software renderer (gmrender-resurrect): `DLNA_TEST_HOST=<renderer ip> DLNA_TEST_LOCAL=<this machine's ip> go test -v -run RealRenderer .` (skipped without those variables). It checks multicast discovery, the unicast probe, Prepare, Load (the renderer fetches the clip from a local Range-capable server), and, when the renderer's output waits for the clock, pause, seek, volume and stop. Verified against gmrender-resurrect: discovery, probe and load work against gmrender-resurrect (full DIDL accepted, the renderer fetched the media); a fakesink without a clock plays the clip at once, so pause and seek need a synchronized sink.

## License

Apache License 2.0, see `LICENSE` and `NOTICE`.
