package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
)

// fakeRenderer is a DLNA MediaRenderer for the tests: a description document, the two control endpoints with a small state machine, and the knobs
// the tests turn (which DIDL it refuses, whether it accepts POST, how it answers Seek).
type fakeRenderer struct {
	*httptest.Server
	mu         sync.Mutex
	actions    []string          // "Name arg=value ..." in order
	args       map[string]string // arguments of the last call of each action
	state      string            // the TransportState GetTransportInfo reports
	afterPlay  string            // state after Play
	pos, dur   string
	volume     int
	refuseDIDL func(didl string) bool // SetAVTransportURI answers a fault when true
	noPost     bool                   // POST gets 405, M-POST works
	relFault   bool                   // Seek REL_TIME answers a fault
	mpost      int
}

const (
	nsAVT = "urn:schemas-upnp-org:service:AVTransport:1"
	nsRC  = "urn:schemas-upnp-org:service:RenderingControl:1"
)

func newRenderer(t *testing.T) *fakeRenderer {
	t.Helper()
	r := &fakeRenderer{state: "NO_MEDIA_PRESENT", afterPlay: "PLAYING", pos: "0:00:00", dur: "0:00:00", volume: 20, args: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/desc.xml", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<?xml version="1.0"?><root xmlns="urn:schemas-upnp-org:device-1-0"><specVersion><major>1</major><minor>0</minor></specVersion><device>
<deviceType>urn:schemas-upnp-org:device:MediaRenderer:1</deviceType><friendlyName>Test TV</friendlyName><manufacturer>Acme</manufacturer><modelName>X1</modelName><UDN>uuid:1234-abcd</UDN>
<serviceList>
<service><serviceType>`+nsAVT+`</serviceType><serviceId>urn:upnp-org:serviceId:AVTransport</serviceId><controlURL>/avt/control</controlURL><eventSubURL>/avt/event</eventSubURL><SCPDURL>/avt/scpd.xml</SCPDURL></service>
<service><serviceType>`+nsRC+`</serviceType><serviceId>urn:upnp-org:serviceId:RenderingControl</serviceId><controlURL>/rc/control</controlURL><eventSubURL>/rc/event</eventSubURL><SCPDURL>/rc/scpd.xml</SCPDURL></service>
</serviceList></device></root>`)
	})
	mux.HandleFunc("/avt/control", r.soap(nsAVT))
	mux.HandleFunc("/rc/control", r.soap(nsRC))
	r.Server = httptest.NewServer(mux)
	t.Cleanup(r.Close)
	return r
}

var argRE = regexp.MustCompile(`<(\w+)>([^<]*)</\w+>`)

func (r *fakeRenderer) soap(ns string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if req.Method == "M-POST" {
			r.mpost++
		} else if r.noPost && req.Method == http.MethodPost {
			http.Error(w, "use M-POST", http.StatusMethodNotAllowed)
			return
		}
		sa := req.Header.Get("SOAPACTION")
		if sa == "" {
			sa = req.Header.Get("01-SOAPACTION")
		}
		_, name, _ := strings.Cut(strings.Trim(sa, `"`), "#")
		body, _ := io.ReadAll(req.Body)
		args := map[string]string{}
		var parts []string
		inner := string(body)
		if i := strings.Index(inner, "<u:"+name); i >= 0 {
			inner = inner[i:]
		}
		for _, m := range argRE.FindAllStringSubmatch(inner, -1) {
			if m[1] == name {
				continue
			}
			args[m[1]] = html.UnescapeString(m[2])
			parts = append(parts, m[1]+"="+args[m[1]])
		}
		r.actions = append(r.actions, name+" "+strings.Join(parts, " "))
		r.args[name] = strings.Join(parts, " ")
		for k, v := range args {
			r.args[name+"."+k] = v
		}
		reply := func(fields string) {
			w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
			_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:%sResponse xmlns:u="%s">%s</u:%sResponse></s:Body></s:Envelope>`, name, ns, fields, name)
		}
		failWith := func(code int) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>%d</errorCode><errorDescription>x</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`, code)
		}
		switch name {
		case "SetAVTransportURI":
			if r.refuseDIDL != nil && r.refuseDIDL(args["CurrentURIMetaData"]) {
				failWith(714)
				return
			}
			r.state = "STOPPED"
			reply("")
		case "Play":
			r.state = r.afterPlay
			reply("")
		case "Pause":
			r.state = "PAUSED_PLAYBACK"
			reply("")
		case "Stop":
			r.state = "STOPPED"
			reply("")
		case "Seek":
			if r.relFault && args["Unit"] == "REL_TIME" {
				failWith(710)
				return
			}
			r.pos = args["Target"]
			reply("")
		case "GetTransportInfo":
			reply(`<CurrentTransportState>` + r.state + `</CurrentTransportState><CurrentTransportStatus>OK</CurrentTransportStatus><CurrentSpeed>1</CurrentSpeed>`)
		case "GetPositionInfo":
			reply(`<Track>1</Track><TrackDuration>` + r.dur + `</TrackDuration><TrackMetaData></TrackMetaData><TrackURI></TrackURI><RelTime>` + r.pos + `</RelTime><AbsTime>` + r.pos + `</AbsTime><RelCount>0</RelCount><AbsCount>0</AbsCount>`)
		case "GetVolume":
			reply(fmt.Sprintf(`<CurrentVolume>%d</CurrentVolume>`, r.volume))
		case "SetVolume":
			fmt.Sscan(args["DesiredVolume"], &r.volume)
			reply("")
		case "GetMute":
			reply(`<CurrentMute>0</CurrentMute>`)
		case "SetMute":
			reply("")
		default:
			failWith(401)
		}
	}
}

func (r *fakeRenderer) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, a := range r.actions {
		out = append(out, strings.Fields(a)[0])
	}
	return out
}

func (r *fakeRenderer) set(f func()) {
	r.mu.Lock()
	f()
	r.mu.Unlock()
}

func (r *fakeRenderer) device() *pluginv1.Device {
	h, _ := json.Marshal(handle{Location: r.URL + "/desc.xml"})
	return &pluginv1.Device{Id: "1234-abcd", Name: "Test TV", Handle: h}
}

func newTestPlugin() *plugin {
	p := newPlugin()
	p.playWait = 400 * time.Millisecond
	return p
}

func loadReq(r *fakeRenderer, state []byte, opts string, m *pluginv1.Media) *pluginv1.LoadRequest {
	d := r.device()
	d.State = state
	if m == nil {
		m = &pluginv1.Media{Url: "http://192.0.2.1:8096/api/v1/cast/TOKEN/stream.mkv", Container: "mkv", Title: `Film & "Co"`, DurationMs: 7200_000, SizeBytes: 123}
	}
	return &pluginv1.LoadRequest{Device: d, Media: m, Options: opts, Mime: "video/x-matroska"}
}

func TestDescribe(t *testing.T) {
	m, err := newPlugin().Describe(context.Background(), &pluginv1.DescribeRequest{Language: "zh-TW"})
	if err != nil || m.GetId() != "dlna" || m.GetCastProtocol() != "dlna" || len(m.GetCapabilities()) != 1 || m.GetCapabilities()[0] != "cast.target" ||
		len(m.GetPermissions()) != 1 || m.GetPermissions()[0] != "network.lan" || m.GetVersion() == "" || !strings.Contains(m.GetDescription(), "DLNA") {
		t.Errorf("Describe = %v, %v", m, err)
	}
}

// Probe asks a host directly with a unicast M-SEARCH and reads the description it points at.
func TestProbeByUnicastSearch(t *testing.T) {
	r := newRenderer(t)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no UDP on loopback")
	}
	defer func() { _ = pc.Close() }()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if strings.HasPrefix(string(buf[:n]), "M-SEARCH") {
				_, _ = pc.WriteTo([]byte("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=1800\r\nLOCATION: "+r.URL+"/desc.xml\r\nST: urn:schemas-upnp-org:device:MediaRenderer:1\r\nUSN: uuid:1234-abcd\r\n\r\n"), from)
			}
		}
	}()
	p := newTestPlugin()
	p.ssdpPort = pc.LocalAddr().(*net.UDPAddr).Port
	resp, err := p.Probe(context.Background(), &pluginv1.ProbeRequest{Host: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	d := resp.GetDevice()
	var h handle
	_ = json.Unmarshal(d.GetHandle(), &h)
	if d.GetId() != "1234-abcd" || d.GetName() != "Test TV" || d.GetManufacturer() != "Acme" || d.GetModel() != "X1" || h.Location != r.URL+"/desc.xml" ||
		!d.GetCapabilities().GetCanSeek() || !d.GetCapabilities().GetCanSetVolume() {
		t.Errorf("device = %v (handle %+v)", d, h)
	}
	// The port the administrator typed is the renderer's HTTP port, not SSDP's: it is ignored and SSDP is asked on its own port.
	if resp, err := p.Probe(context.Background(), &pluginv1.ProbeRequest{Host: "127.0.0.1:49494"}); err != nil || resp.GetDevice().GetId() != "1234-abcd" {
		t.Errorf("Probe with an HTTP port = %v, %v", resp, err)
	}
	// Nobody answers there: NOT_FOUND. A bad host is INVALID_ARGUMENT.
	q := newTestPlugin()
	q.ssdpPort = 1 // nothing listens on loopback port 1
	if _, err := q.Probe(ctx3(), &pluginv1.ProbeRequest{Host: "127.0.0.1"}); status.Code(err) != codes.NotFound {
		t.Errorf("silent host = %v, want NOT_FOUND", err)
	}
	if _, err := p.Probe(context.Background(), &pluginv1.ProbeRequest{Host: ""}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty host = %v", err)
	}
}

func ctx3() context.Context {
	c, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	_ = cancel
	return c
}

func TestPrepare(t *testing.T) {
	p := newTestPlugin()
	prep := func(m *pluginv1.Media, opts string) *pluginv1.PrepareResponse {
		resp, err := p.Prepare(context.Background(), &pluginv1.PrepareRequest{Device: &pluginv1.Device{State: []byte(`{"didl":"lite"}`)}, Media: m, Options: opts})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	r := prep(&pluginv1.Media{Container: "mkv", VideoCodec: "h264", AudioCodec: "aac", VideoBitDepth: 8}, "")
	if r.GetMime() != "video/x-matroska" || r.GetHeaders()["transferMode.dlna.org"] != "Streaming" || r.GetHeaders()["contentFeatures.dlna.org"] != featuresFull ||
		len(r.GetWarnings()) != 0 || len(r.GetReasons()) != 0 || string(r.GetUpdatedState()) != `{"didl":"lite"}` {
		t.Errorf("plain: %v", r)
	}
	// Risky: HDR, 10-bit, DTS, AV1 are warnings, never a refusal; a subtitle warns that the style is lost.
	for _, m := range []*pluginv1.Media{{Container: "mkv", VideoRange: "hdr10"}, {Container: "mkv", VideoBitDepth: 10}, {Container: "mkv", AudioCodec: "dts"}, {Container: "mkv", VideoCodec: "av1"}} {
		if r := prep(m, ""); len(r.GetWarnings()) != 1 || r.GetWarnings()[0] != "cast_codec_risky" || len(r.GetReasons()) != 0 {
			t.Errorf("risky %v: %v", m, r)
		}
	}
	if r := prep(&pluginv1.Media{Container: "mp4", Subtitles: []*pluginv1.Subtitle{{Format: "srt"}}}, ""); r.GetMime() != "video/mp4" || len(r.GetWarnings()) != 1 || r.GetWarnings()[0] != "cast_subtitle_style_lost" {
		t.Errorf("subtitle: %v", r)
	}
	// The administrator's options: mime override, no DLNA features.
	r = prep(&pluginv1.Media{Container: "mkv"}, "mime=video/mpeg\nfeatures=none; junk; didl=bogus")
	if r.GetMime() != "video/mpeg" || r.GetHeaders()["contentFeatures.dlna.org"] != "" || r.GetHeaders()["transferMode.dlna.org"] != "Streaming" {
		t.Errorf("options: %v", r)
	}
}

func TestLoadAndPlay(t *testing.T) {
	r := newRenderer(t)
	p := newTestPlugin()
	resp, err := p.Load(context.Background(), loadReq(r, nil, "", &pluginv1.Media{Url: "http://192.0.2.1:8096/api/v1/cast/TOKEN/stream.mkv", Title: `Film & "Co"`, DurationMs: 7200_000,
		SizeBytes: 123, PosterUrl: "http://192.0.2.1:8096/poster.jpg", StartPositionMs: 90_000,
		Subtitles: []*pluginv1.Subtitle{{Url: "http://192.0.2.1:8096/api/v1/cast/TOKEN/sub/2.srt", Format: "srt"}}}))
	if err != nil || resp.GetRejectedCode() != "" {
		t.Fatalf("Load = %v, %v", resp, err)
	}
	// Order: Stop, SetAVTransportURI, (transition polling), Play, (polling), Seek.
	calls := r.calls()
	idx := func(name string) int {
		for i, c := range calls {
			if c == name {
				return i
			}
		}
		return -1
	}
	if idx("Stop") != 0 || idx("SetAVTransportURI") != 1 || idx("Play") < 2 || idx("Seek") < idx("Play") {
		t.Errorf("calls = %v", calls)
	}
	didl := r.args["SetAVTransportURI.CurrentURIMetaData"]
	for _, want := range []string{`<dc:title>Film &amp; &#34;Co&#34;</dc:title>`, `protocolInfo="http-get:*:video/x-matroska:` + featuresFull + `"`, `duration="2:00:00"`, `size="123"`,
		`>http://192.0.2.1:8096/api/v1/cast/TOKEN/stream.mkv</res>`, `<sec:CaptionInfoEx sec:type="srt">http://192.0.2.1:8096/api/v1/cast/TOKEN/sub/2.srt</sec:CaptionInfoEx>`,
		`<upnp:albumArtURI>http://192.0.2.1:8096/poster.jpg</upnp:albumArtURI>`, `pv:subtitleFileUri=`} {
		if !strings.Contains(didl, want) {
			t.Errorf("DIDL lacks %q:\n%s", want, didl)
		}
	}
	if r.args["SetAVTransportURI.CurrentURI"] != "http://192.0.2.1:8096/api/v1/cast/TOKEN/stream.mkv" || r.args["Seek.Unit"] != "REL_TIME" || r.args["Seek.Target"] != "0:01:30" || r.args["Play.Speed"] != "1" {
		t.Errorf("args = %v", r.args)
	}
	if c := parseCompat(resp.GetUpdatedState()); c.DIDL != didlFull {
		t.Errorf("state = %s", resp.GetUpdatedState())
	}
}

// A device that refuses the full metadata is asked with simpler metadata; the variant that worked is remembered, so the next cast goes straight to it.
func TestLoadFallsBackToSimplerMetadataAndRemembersIt(t *testing.T) {
	r := newRenderer(t)
	r.set(func() { r.refuseDIDL = func(d string) bool { return strings.Contains(d, "xmlns:sec") } })
	p := newTestPlugin()
	resp, err := p.Load(context.Background(), loadReq(r, nil, "", nil))
	if err != nil || resp.GetRejectedCode() != "" || parseCompat(resp.GetUpdatedState()).DIDL != didlLite {
		t.Fatalf("Load = %v, %v", resp, err)
	}
	if n := countOf(r.calls(), "SetAVTransportURI"); n != 2 {
		t.Errorf("%d SetAVTransportURI calls, want 2 (full refused, lite accepted)", n)
	}
	before := countOf(r.calls(), "SetAVTransportURI")
	if resp, err = p.Load(context.Background(), loadReq(r, resp.GetUpdatedState(), "", nil)); err != nil || resp.GetRejectedCode() != "" {
		t.Fatal(resp, err)
	}
	if got := countOf(r.calls(), "SetAVTransportURI") - before; got != 1 {
		t.Errorf("the remembered variant took %d SetAVTransportURI calls, want 1", got)
	}
	// The administrator can force a variant: empty metadata.
	if resp, err = p.Load(context.Background(), loadReq(r, nil, "didl=empty", nil)); err != nil || r.args["SetAVTransportURI.CurrentURIMetaData"] != "" || parseCompat(resp.GetUpdatedState()).DIDL != didlEmpty {
		t.Errorf("forced empty: %v, %v, metadata %q", resp, err, r.args["SetAVTransportURI.CurrentURIMetaData"])
	}
	// Everything refused: an answer (rejected), not an RPC error.
	r.set(func() { r.refuseDIDL = func(string) bool { return true } })
	if resp, err = p.Load(context.Background(), loadReq(r, nil, "", nil)); err != nil || resp.GetRejectedCode() != "renderer_rejected" || resp.GetDetail() == "" {
		t.Errorf("all refused: %v, %v", resp, err)
	}
}

func countOf(list []string, s string) int {
	n := 0
	for _, x := range list {
		if x == s {
			n++
		}
	}
	return n
}

// A device that accepts the media but never plays is reported as rejected; one that cannot be reached is an RPC error.
func TestLoadWhenTheDeviceDoesNotPlayOrIsGone(t *testing.T) {
	r := newRenderer(t)
	r.set(func() { r.afterPlay = "STOPPED" })
	p := newTestPlugin()
	if resp, err := p.Load(context.Background(), loadReq(r, nil, "", nil)); err != nil || resp.GetRejectedCode() != "renderer_stopped" {
		t.Errorf("never plays: %v, %v", resp, err)
	}
	r.set(func() { r.afterPlay = "TRANSITIONING" }) // slow, not failing
	if resp, err := p.Load(context.Background(), loadReq(r, nil, "", nil)); err != nil || resp.GetRejectedCode() != "" {
		t.Errorf("slow to start: %v, %v", resp, err)
	}
	r.Close()
	if _, err := p.Load(context.Background(), loadReq(r, nil, "", nil)); status.Code(err) != codes.Unavailable {
		t.Errorf("gone = %v, want UNAVAILABLE", err)
	}
}

func TestControlAndStatus(t *testing.T) {
	r := newRenderer(t)
	p := newTestPlugin()
	ctx := context.Background()
	d := r.device()
	ctl := func(a pluginv1.ControlAction, mod func(*pluginv1.ControlRequest)) error {
		req := &pluginv1.ControlRequest{Device: d, Action: a}
		if mod != nil {
			mod(req)
		}
		_, err := p.Control(ctx, req)
		return err
	}
	if err := ctl(pluginv1.ControlAction_CONTROL_ACTION_PLAY, nil); err != nil {
		t.Fatal(err)
	}
	st := func() *pluginv1.StatusResponse {
		resp, err := p.Status(ctx, &pluginv1.StatusRequest{Device: d})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	r.set(func() { r.dur, r.pos = "2:00:00.000", "0:10:05" })
	if s := st(); s.GetState() != pluginv1.PlaybackState_PLAYBACK_STATE_PLAYING || s.GetPositionMs() != 605_000 || s.GetDurationMs() != 7200_000 || s.GetVolume() != 20 || s.GetMuted() || s.GetEnded() {
		t.Errorf("playing: %v", s)
	}
	if err := ctl(pluginv1.ControlAction_CONTROL_ACTION_PAUSE, nil); err != nil {
		t.Fatal(err)
	}
	if s := st(); s.GetState() != pluginv1.PlaybackState_PLAYBACK_STATE_PAUSED {
		t.Errorf("paused: %v", s)
	}
	// Seek: whole seconds REL_TIME; when the device faults on REL_TIME, ABS_TIME.
	if err := ctl(pluginv1.ControlAction_CONTROL_ACTION_SEEK, func(q *pluginv1.ControlRequest) { q.PositionMs = 3_725_900 }); err != nil || r.args["Seek.Unit"] != "REL_TIME" || r.args["Seek.Target"] != "1:02:05" {
		t.Errorf("seek: %v %v", err, r.args)
	}
	r.set(func() { r.relFault = true })
	if err := ctl(pluginv1.ControlAction_CONTROL_ACTION_SEEK, func(q *pluginv1.ControlRequest) { q.PositionMs = 60_000 }); err != nil || r.args["Seek.Unit"] != "ABS_TIME" || r.args["Seek.Target"] != "0:01:00" {
		t.Errorf("seek fallback: %v %v", err, r.args)
	}
	if err := ctl(pluginv1.ControlAction_CONTROL_ACTION_SET_VOLUME, func(q *pluginv1.ControlRequest) { q.Volume = 55 }); err != nil || st().GetVolume() != 55 {
		t.Errorf("volume: %v", err)
	}
	if err := ctl(pluginv1.ControlAction_CONTROL_ACTION_SET_MUTE, func(q *pluginv1.ControlRequest) { q.Muted = true }); err != nil {
		t.Errorf("mute: %v", err)
	}
	// Stopped at the end of the media is "ended"; stopped in the middle is not; no media present is idle.
	r.set(func() { r.state, r.pos = "STOPPED", "1:59:58" })
	if s := st(); s.GetState() != pluginv1.PlaybackState_PLAYBACK_STATE_STOPPED || !s.GetEnded() {
		t.Errorf("finished: %v", s)
	}
	r.set(func() { r.pos = "0:30:00" })
	if s := st(); s.GetEnded() {
		t.Errorf("stopped in the middle: %v", s)
	}
	r.set(func() { r.state = "NO_MEDIA_PRESENT" })
	if s := st(); s.GetState() != pluginv1.PlaybackState_PLAYBACK_STATE_IDLE {
		t.Errorf("idle: %v", s)
	}
	if err := ctl(pluginv1.ControlAction_CONTROL_ACTION_STOP, nil); err != nil {
		t.Error(err)
	}
	if err := ctl(pluginv1.ControlAction_CONTROL_ACTION_UNSPECIFIED, nil); status.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown action = %v", err)
	}
	// A device with no handle cannot be reached.
	if _, err := p.Status(ctx, &pluginv1.StatusRequest{Device: &pluginv1.Device{Id: "x"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("no handle = %v", err)
	}
	// Gone: UNAUTHENTICATED is never used; the cache is dropped so the next call reads the description again.
	r.Close()
	if _, err := p.Status(ctx, &pluginv1.StatusRequest{Device: d}); status.Code(err) != codes.Unavailable {
		t.Errorf("gone = %v, want UNAVAILABLE", err)
	}
	p.mu.Lock()
	cached := len(p.clients)
	p.mu.Unlock()
	if cached != 0 {
		t.Errorf("%d cached clients after a failure", cached)
	}
}

// A device that answers 405 to POST is retried as M-POST.
func TestMPostFallback(t *testing.T) {
	r := newRenderer(t)
	r.set(func() { r.noPost = true })
	p := newTestPlugin()
	if _, err := p.Control(context.Background(), &pluginv1.ControlRequest{Device: r.device(), Action: pluginv1.ControlAction_CONTROL_ACTION_PAUSE}); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.mpost == 0 || len(r.actions) == 0 || !strings.HasPrefix(r.actions[0], "Pause") {
		t.Errorf("M-POST requests %d, actions %v", r.mpost, r.actions)
	}
}

func TestTimeFormats(t *testing.T) {
	for in, want := range map[string]time.Duration{"0:00:00": 0, "1:02:03": time.Hour + 2*time.Minute + 3*time.Second, "0:00:01.500": 1500 * time.Millisecond, "NOT_IMPLEMENTED": 0, "": 0, "1:2": 0, "-0:00:05": 0} {
		if got := parseHMS(in); got != want {
			t.Errorf("parseHMS(%q) = %v, want %v", in, got, want)
		}
	}
	if got := hms(3725900 * time.Millisecond); got != "1:02:05" {
		t.Errorf("hms = %q", got)
	}
	if o := parseOptions("mime=video/x-evil <b>\nmime=video/mpeg;features=full;didl=lite;subtitle=none;bad"); o.mime != "video/mpeg" || o.features != "full" || o.didl != didlLite || o.subtitle != "none" {
		t.Errorf("options = %+v", o)
	}
}

// A renderer that goes back to position 0 when it reaches the end is still recognized: the last position seen while playing was near the end.
func TestEndIsToldFromTheLastPositionSeenPlaying(t *testing.T) {
	r := newRenderer(t)
	p := newTestPlugin()
	ctx := context.Background()
	d := r.device()
	st := func() *pluginv1.StatusResponse {
		resp, err := p.Status(ctx, &pluginv1.StatusRequest{Device: d})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	r.set(func() { r.state, r.dur, r.pos = "PLAYING", "2:00:00", "1:59:55" })
	if s := st(); s.GetEnded() {
		t.Errorf("playing: %v", s)
	}
	r.set(func() { r.state, r.pos = "STOPPED", "0:00:00" }) // the end: the device rewinds
	if s := st(); !s.GetEnded() || s.GetPositionMs() != 7195_000 || s.GetDurationMs() != 7200_000 {
		t.Errorf("rewound at the end: %v", s)
	}
	// Stopped by a person in the middle, then rewound: not the end.
	r.set(func() { r.state, r.pos = "PLAYING", "0:30:00" })
	st()
	r.set(func() { r.state, r.pos = "STOPPED", "0:00:00" })
	if s := st(); s.GetEnded() || s.GetPositionMs() != 0 {
		t.Errorf("stopped in the middle: %v", s)
	}
}
