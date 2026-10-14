package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
)

// TestAgainstARealRenderer runs the whole cast against a real software renderer on the local network (gmrender-resurrect, see test/cast).
// It is skipped unless DLNA_TEST_HOST (the renderer's address) and DLNA_TEST_LOCAL (this machine's address as the renderer sees it) are set:
//
//	DLNA_TEST_HOST=192.168.50.8 DLNA_TEST_LOCAL=192.168.50.8 go test -v -run RealRenderer ./plugins/dlna
//
// It generates a 30 second clip with ffmpeg (synthetic test data), serves it with Range support from this machine and logs every request the
// renderer makes.
func TestAgainstARealRenderer(t *testing.T) {
	host, local := os.Getenv("DLNA_TEST_HOST"), os.Getenv("DLNA_TEST_LOCAL")
	if host == "" || local == "" {
		t.Skip("DLNA_TEST_HOST and DLNA_TEST_LOCAL are not set")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	clip := filepath.Join(t.TempDir(), "clip.mkv")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-f", "lavfi", "-i", "testsrc=duration=30:size=640x360:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:duration=30",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", clip).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v\n%s", err, out)
	}
	var mu sync.Mutex
	var reqs []string
	mux := http.NewServeMux()
	mux.HandleFunc("/stream.mkv", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, r.Method+" Range="+r.Header.Get("Range")+" from "+r.RemoteAddr)
		mu.Unlock()
		w.Header().Set("transferMode.dlna.org", "Streaming")
		w.Header().Set("contentFeatures.dlna.org", featuresFull)
		w.Header().Set("Content-Type", "video/x-matroska")
		f, err := os.Open(clip)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() { _ = f.Close() }()
		fi, _ := f.Stat()
		http.ServeContent(w, r, "stream.mkv", fi.ModTime(), f)
	})
	ln, err := net.Listen("tcp", local+":0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	url := "http://" + ln.Addr().String() + "/stream.mkv"

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	p := newPlugin()

	// Multicast discovery.
	disc, err := p.Discover(ctx, &pluginv1.DiscoverRequest{TimeoutMs: 4000})
	if err != nil {
		t.Fatal(err)
	}
	var dev *pluginv1.Device
	for _, d := range disc.GetDevices() {
		t.Logf("discovered: %s %q (%s %s) at %s", d.GetId(), d.GetName(), d.GetManufacturer(), d.GetModel(), d.GetAddress())
		if strings.Contains(d.GetAddress(), host) {
			dev = d
		}
	}
	if dev == nil {
		t.Errorf("the renderer at %s was not found by multicast discovery", host)
	}
	// Unicast probe (what "add by IP" does).
	pr, err := p.Probe(ctx, &pluginv1.ProbeRequest{Host: host})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	t.Logf("probed: %s %q handle %s", pr.GetDevice().GetId(), pr.GetDevice().GetName(), pr.GetDevice().GetHandle())
	if dev == nil {
		dev = pr.GetDevice()
	}

	media := &pluginv1.Media{Url: url, Container: "mkv", Title: "Test clip", DurationMs: 30_000, SizeBytes: 1, Delivery: "file", VideoCodec: "h264", AudioCodec: "aac"}
	prep, err := p.Prepare(ctx, &pluginv1.PrepareRequest{Device: dev, Media: media})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("prepare: mime %s headers %v warnings %v reasons %v", prep.GetMime(), prep.GetHeaders(), prep.GetWarnings(), prep.GetReasons())
	load, err := p.Load(ctx, &pluginv1.LoadRequest{Device: dev, Media: media, Mime: prep.GetMime()})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Logf("load: rejected %q detail %q state %s", load.GetRejectedCode(), load.GetDetail(), load.GetUpdatedState())
	if load.GetRejectedCode() != "" {
		t.Fatalf("the renderer rejected the media: %s", load.GetRejectedCode())
	}
	dev.State = load.GetUpdatedState()

	status := func(label string) *pluginv1.StatusResponse {
		s, err := p.Status(ctx, &pluginv1.StatusRequest{Device: dev})
		if err != nil {
			t.Fatalf("Status (%s): %v", label, err)
		}
		t.Logf("status %-8s state %-8s pos %5dms dur %5dms vol %d muted %v ended %v", label, s.GetState(), s.GetPositionMs(), s.GetDurationMs(), s.GetVolume(), s.GetMuted(), s.GetEnded())
		return s
	}
	ctl := func(a pluginv1.ControlAction, mod func(*pluginv1.ControlRequest)) {
		req := &pluginv1.ControlRequest{Device: dev, Action: a}
		if mod != nil {
			mod(req)
		}
		if _, err := p.Control(ctx, req); err != nil {
			t.Errorf("Control %s: %v", a, err)
		}
	}
	time.Sleep(2 * time.Second)
	first := status("playing")
	if first.GetEnded() || first.GetState() == pluginv1.PlaybackState_PLAYBACK_STATE_STOPPED && first.GetPositionMs() == 0 {
		// A renderer whose output does not wait for the clock (gmediarender with a plain fakesink) plays the whole clip at once: the load worked and
		// the end was reported, but pause and seek cannot be tried. Run the renderer with a synchronized sink to exercise them.
		t.Log("the renderer finished the clip at once (output without a clock); skipping pause, seek and volume checks")
		if first.GetDurationMs() != 30_000 {
			t.Errorf("duration of the clip: %v", first)
		}
		reportFetches(t, &mu, reqs)
		return
	}
	if first.GetState() != pluginv1.PlaybackState_PLAYBACK_STATE_PLAYING {
		t.Errorf("not playing after Load: %v", first.GetState())
	}
	ctl(pluginv1.ControlAction_CONTROL_ACTION_PAUSE, nil)
	time.Sleep(500 * time.Millisecond)
	if s := status("paused"); s.GetState() != pluginv1.PlaybackState_PLAYBACK_STATE_PAUSED {
		t.Errorf("not paused: %v", s.GetState())
	}
	ctl(pluginv1.ControlAction_CONTROL_ACTION_SEEK, func(r *pluginv1.ControlRequest) { r.PositionMs = 20_000 })
	ctl(pluginv1.ControlAction_CONTROL_ACTION_PLAY, nil)
	time.Sleep(1500 * time.Millisecond)
	status("seeked")
	ctl(pluginv1.ControlAction_CONTROL_ACTION_SET_VOLUME, func(r *pluginv1.ControlRequest) { r.Volume = 40 })
	status("volume")
	ctl(pluginv1.ControlAction_CONTROL_ACTION_STOP, nil)
	time.Sleep(500 * time.Millisecond)
	if s := status("stopped"); s.GetState() == pluginv1.PlaybackState_PLAYBACK_STATE_PLAYING {
		t.Errorf("still playing after Stop")
	}
	reportFetches(t, &mu, reqs)
}

func reportFetches(t *testing.T, mu *sync.Mutex, reqs []string) {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	for _, r := range reqs {
		t.Logf("renderer fetched: %s", r)
	}
	if len(reqs) == 0 {
		t.Error("the renderer never fetched the media")
	}
}
