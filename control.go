package main

import (
	"context"
	"strings"
	"time"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
	"github.com/kryspal/plugin-sdk/sdk"
)

// Prepare decides what the host must put on the media responses: the content type and the DLNA headers (they must match the protocolInfo that
// Load sends in the DIDL, because some televisions compare them), and the warnings. A DLNA renderer cannot be asked what it decodes, so the
// plugin never refuses a video on its own: it warns about what televisions commonly fail on (the answer comes with Load, see rejected_code).
func (p *plugin) Prepare(_ context.Context, req *pluginv1.PrepareRequest) (*pluginv1.PrepareResponse, error) {
	m, o := req.GetMedia(), parseOptions(req.GetOptions())
	mime := o.mime
	if mime == "" {
		mime = mimeFor(m.GetContainer())
	}
	headers := map[string]string{"transferMode.dlna.org": "Streaming"}
	if f := featuresFor(o); f != "*" {
		headers["contentFeatures.dlna.org"] = f
	}
	resp := &pluginv1.PrepareResponse{Mime: mime, Headers: headers, UpdatedState: req.GetDevice().GetState()}
	if risky(m) {
		resp.Warnings = append(resp.Warnings, "cast_codec_risky")
	}
	if len(m.GetSubtitles()) > 0 {
		resp.Warnings = append(resp.Warnings, "cast_subtitle_style_lost") // the host converts to plain srt
	}
	return resp, nil
}

// risky reports whether the media uses something televisions commonly cannot play over DLNA: Dolby Vision or HDR video, 10-bit video,
// HEVC in a container other than mkv/mp4/ts, VP9 and AV1, DTS and TrueHD audio.
func risky(m *pluginv1.Media) bool {
	vc, ac := strings.ToLower(m.GetVideoCodec()), strings.ToLower(m.GetAudioCodec())
	switch {
	case vc == "av1" || vc == "vp9":
		return true
	case m.GetVideoRange() == "dolby-vision" || m.GetVideoRange() == "hdr10" || m.GetVideoRange() == "hlg" || m.GetVideoBitDepth() > 8:
		return true
	case strings.HasPrefix(ac, "dts") || ac == "truehd" || ac == "mlp":
		return true
	}
	return false
}

// Load pushes the media: Stop (some televisions answer 701 to SetAVTransportURI while playing), SetAVTransportURI with the DIDL variant that
// works, wait for the transition, Play, and Seek to the start position. A device that refuses the metadata is asked again with simpler
// metadata; a device that does not start playing is reported as rejected (the answer is not an RPC error: the administrator can change the
// options and cast again).
func (p *plugin) Load(ctx context.Context, req *pluginv1.LoadRequest) (*pluginv1.LoadResponse, error) {
	c, err := p.clientsFor(ctx, req.GetDevice())
	if err != nil {
		return nil, err
	}
	m, o := req.GetMedia(), parseOptions(req.GetOptions())
	mime := req.GetMime()
	if mime == "" {
		mime = mimeFor(m.GetContainer())
	}
	dm := didlMedia{title: m.GetTitle(), url: m.GetUrl(), poster: m.GetPosterUrl(), mime: mime, features: featuresFor(o),
		duration: time.Duration(m.GetDurationMs()) * time.Millisecond, size: m.GetSizeBytes()}
	if len(m.GetSubtitles()) > 0 && o.subtitle != "none" {
		dm.subURL = m.GetSubtitles()[0].GetUrl()
	}

	cm := parseCompat(req.GetDevice().GetState())
	variants := didlOrder
	switch {
	case o.didl != "":
		variants = []string{o.didl}
	case cm.DIDL != "":
		variants = append([]string{cm.DIDL}, without(didlOrder, cm.DIDL)...)
	}

	_ = c.avt.StopCtx(ctx, 0) // the answer does not matter: nothing may be playing
	var lastFault string
	accepted := ""
	for _, v := range variants {
		err := c.avt.SetAVTransportURICtx(ctx, 0, m.GetUrl(), buildDIDL(v, dm))
		if err == nil {
			accepted = v
			break
		}
		if _, isFault := fault(err); !isFault {
			p.forget(req.GetDevice().GetId())
			return nil, sdk.Unavailable("dlna: " + err.Error())
		}
		lastFault = err.Error()
	}
	if accepted == "" {
		return &pluginv1.LoadResponse{RejectedCode: "renderer_rejected", Detail: lastFault}, nil
	}

	waitTransition(ctx, c, 6*time.Second)
	if err := c.avt.PlayCtx(ctx, 0, "1"); err != nil {
		time.Sleep(500 * time.Millisecond)
		if err = c.avt.PlayCtx(ctx, 0, "1"); err != nil {
			if _, isFault := fault(err); isFault {
				return &pluginv1.LoadResponse{RejectedCode: "renderer_rejected", Detail: err.Error()}, nil
			}
			p.forget(req.GetDevice().GetId())
			return nil, sdk.Unavailable("dlna: " + err.Error())
		}
	}
	state, ok := waitPlaying(ctx, c, p.playWait)
	if !ok {
		return &pluginv1.LoadResponse{RejectedCode: "renderer_stopped", Detail: "the device did not start playing: " + state}, nil
	}
	detail := ""
	if start := time.Duration(m.GetStartPositionMs()) * time.Millisecond; start > time.Second {
		if err := seek(ctx, c, start); err != nil {
			detail = "could not seek to the start position: " + err.Error()
		}
	}
	cm.DIDL = accepted
	return &pluginv1.LoadResponse{Detail: detail, UpdatedState: cm.marshal()}, nil
}

func without(list []string, drop string) []string {
	var out []string
	for _, s := range list {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// waitTransition polls until the device leaves TRANSITIONING (no fixed delay: nothing says how long a television needs).
func waitTransition(ctx context.Context, c *deviceClients, limit time.Duration) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		st, _, _, err := c.avt.GetTransportInfoCtx(ctx, 0)
		if err != nil || st != "TRANSITIONING" {
			return
		}
		sleep(ctx, 250*time.Millisecond)
	}
}

// waitPlaying polls until the device plays, is paused or is still loading; a device that stays stopped is not playing. It returns the last
// state and whether the cast is on its way.
func waitPlaying(ctx context.Context, c *deviceClients, limit time.Duration) (string, bool) {
	deadline := time.Now().Add(limit)
	last := ""
	for time.Now().Before(deadline) && ctx.Err() == nil {
		st, _, _, err := c.avt.GetTransportInfoCtx(ctx, 0)
		if err == nil {
			last = st
			switch st {
			case "PLAYING", "PAUSED_PLAYBACK":
				return st, true
			}
		}
		sleep(ctx, 300*time.Millisecond)
	}
	// A television that is still TRANSITIONING at the end is slow, not failing; one that never left STOPPED or NO_MEDIA_PRESENT is.
	return last, last == "TRANSITIONING" || last == ""
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// seek moves to a position: REL_TIME first, ABS_TIME when the device does not implement it (whole seconds).
func seek(ctx context.Context, c *deviceClients, pos time.Duration) error {
	err := c.avt.SeekCtx(ctx, 0, "REL_TIME", hms(pos))
	if err == nil {
		return nil
	}
	if _, isFault := fault(err); !isFault {
		return err
	}
	return c.avt.SeekCtx(ctx, 0, "ABS_TIME", hms(pos))
}

// Control sends a command to the device.
func (p *plugin) Control(ctx context.Context, req *pluginv1.ControlRequest) (*pluginv1.ControlResponse, error) {
	c, err := p.clientsFor(ctx, req.GetDevice())
	if err != nil {
		return nil, err
	}
	switch req.GetAction() {
	case pluginv1.ControlAction_CONTROL_ACTION_PLAY:
		err = c.avt.PlayCtx(ctx, 0, "1")
	case pluginv1.ControlAction_CONTROL_ACTION_PAUSE:
		err = c.avt.PauseCtx(ctx, 0)
	case pluginv1.ControlAction_CONTROL_ACTION_STOP:
		err = c.avt.StopCtx(ctx, 0)
	case pluginv1.ControlAction_CONTROL_ACTION_SEEK:
		err = seek(ctx, c, time.Duration(req.GetPositionMs())*time.Millisecond)
	case pluginv1.ControlAction_CONTROL_ACTION_SET_VOLUME:
		if c.rc == nil {
			return nil, sdk.InvalidArgument("dlna: the device has no volume control")
		}
		err = c.rc.SetVolumeCtx(ctx, 0, "Master", uint16(min(max(req.GetVolume(), 0), 100)))
	case pluginv1.ControlAction_CONTROL_ACTION_SET_MUTE:
		if c.rc == nil {
			return nil, sdk.InvalidArgument("dlna: the device has no volume control")
		}
		err = c.rc.SetMuteCtx(ctx, 0, "Master", req.GetMuted())
	default:
		return nil, sdk.InvalidArgument("dlna: unknown control action")
	}
	if err != nil {
		p.forget(req.GetDevice().GetId())
		return nil, sdk.Unavailable("dlna: " + err.Error())
	}
	return &pluginv1.ControlResponse{}, nil
}

// Status reads the transport state and the position (and the volume when the device has RenderingControl).
func (p *plugin) Status(ctx context.Context, req *pluginv1.StatusRequest) (*pluginv1.StatusResponse, error) {
	c, err := p.clientsFor(ctx, req.GetDevice())
	if err != nil {
		return nil, err
	}
	ts, _, _, err := c.avt.GetTransportInfoCtx(ctx, 0)
	if err != nil {
		p.forget(req.GetDevice().GetId())
		return nil, sdk.Unavailable("dlna: " + err.Error())
	}
	resp := &pluginv1.StatusResponse{Volume: -1}
	switch ts {
	case "PLAYING":
		resp.State = pluginv1.PlaybackState_PLAYBACK_STATE_PLAYING
	case "PAUSED_PLAYBACK", "PAUSED_RECORDING":
		resp.State = pluginv1.PlaybackState_PLAYBACK_STATE_PAUSED
	case "TRANSITIONING":
		resp.State = pluginv1.PlaybackState_PLAYBACK_STATE_LOADING
	case "STOPPED":
		resp.State = pluginv1.PlaybackState_PLAYBACK_STATE_STOPPED
	case "NO_MEDIA_PRESENT":
		resp.State = pluginv1.PlaybackState_PLAYBACK_STATE_IDLE
	}
	if _, dur, _, _, rel, _, _, _, perr := c.avt.GetPositionInfoCtx(ctx, 0); perr == nil {
		resp.PositionMs, resp.DurationMs = parseHMS(rel).Milliseconds(), parseHMS(dur).Milliseconds()
	}
	resp.Ended = c.observe(resp)
	if c.rc != nil {
		if v, verr := c.rc.GetVolumeCtx(ctx, 0, "Master"); verr == nil && v <= 100 {
			resp.Volume = int32(v)
		}
		if mu, merr := c.rc.GetMuteCtx(ctx, 0, "Master"); merr == nil {
			resp.Muted = mu
		}
	}
	return resp, nil
}

// endedWithin is how close to the end the last position seen while playing must be for a stop to count as the end of the media (polls are
// about two seconds apart, and a renderer may stop a little before the nominal duration): 3% of the duration, between 2 and 10 seconds.
func endedWithin(dur time.Duration) time.Duration {
	return min(max(dur/33, 2*time.Second), 10*time.Second)
}

// observe remembers the position while the device plays and reports whether a stop is the end of the media: stopped (or no media) right after
// playing within endedWithin of the end, or stopped at the end according to the device's own position. When the device went back to position 0
// on stopping, the last position seen playing is reported instead so the host records where the viewing ended.
func (c *deviceClients) observe(r *pluginv1.StatusResponse) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	pos, dur := time.Duration(r.GetPositionMs())*time.Millisecond, time.Duration(r.GetDurationMs())*time.Millisecond
	switch r.GetState() {
	case pluginv1.PlaybackState_PLAYBACK_STATE_PLAYING, pluginv1.PlaybackState_PLAYBACK_STATE_PAUSED:
		c.wasPlaying, c.lastPos = true, pos
		if dur > 0 {
			c.lastDur = dur
		}
		return false
	case pluginv1.PlaybackState_PLAYBACK_STATE_STOPPED, pluginv1.PlaybackState_PLAYBACK_STATE_IDLE:
		if dur <= 0 {
			dur = c.lastDur
		}
		ended := dur > 0 && (pos >= dur-2*time.Second || c.wasPlaying && c.lastPos >= dur-endedWithin(dur))
		if ended && pos < c.lastPos && c.wasPlaying {
			r.PositionMs = c.lastPos.Milliseconds()
		}
		if dur > 0 && r.GetDurationMs() == 0 {
			r.DurationMs = dur.Milliseconds()
		}
		c.wasPlaying = false
		return ended
	}
	return false
}
