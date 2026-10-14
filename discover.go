package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/httpu"
	"github.com/huin/goupnp/ssdp"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
	"github.com/kryspal/plugin-sdk/sdk"
)

const (
	stMediaRenderer = "urn:schemas-upnp-org:device:MediaRenderer:1"
	// describeTimeout bounds the fetch of one device description during a scan.
	describeTimeout = 3 * time.Second
	// describeParallel is how many descriptions are fetched at the same time.
	describeParallel = 10
)

// Discover scans every IPv4 network interface with SSDP (MediaRenderer and ssdp:all: some devices answer only one of them), asks the
// administrator's known hosts directly (unicast, which works where multicast does not), reads the description of each distinct LOCATION and
// keeps the devices that have an AVTransport service. A host that does not answer is left out.
func (p *plugin) Discover(ctx context.Context, req *pluginv1.DiscoverRequest) (*pluginv1.DiscoverResponse, error) {
	wait := time.Duration(req.GetTimeoutMs()) * time.Millisecond
	if wait < 2*time.Second {
		wait = 2 * time.Second
	}
	if wait > 10*time.Second {
		wait = 10 * time.Second
	}
	locs := map[string]*url.URL{}
	var mu sync.Mutex
	add := func(l *url.URL) {
		if l != nil && (l.Scheme == "http" || l.Scheme == "https") {
			mu.Lock()
			locs[l.String()] = l
			mu.Unlock()
		}
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, l := range multicastSearch(ctx, wait, req.GetInterfaces()) {
			add(l)
		}
	}()
	for _, h := range req.GetKnownHosts() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ls, _ := p.unicastSearch(ctx, h, wait)
			for _, l := range ls {
				add(l)
			}
		}()
	}
	wg.Wait()

	var out []*pluginv1.Device
	var omu sync.Mutex
	sem := make(chan struct{}, describeParallel)
	var dwg sync.WaitGroup
	for _, l := range locs {
		dwg.Add(1)
		sem <- struct{}{}
		go func() {
			defer dwg.Done()
			defer func() { <-sem }()
			dctx, cancel := context.WithTimeout(ctx, describeTimeout)
			defer cancel()
			if d, ok := describe(dctx, l); ok {
				omu.Lock()
				out = append(out, d)
				omu.Unlock()
			}
		}()
	}
	dwg.Wait()
	return &pluginv1.DiscoverResponse{Devices: out}, nil
}

// Probe asks one host directly (host or host:port) and returns the first renderer it knows of; NOT_FOUND when there is none.
func (p *plugin) Probe(ctx context.Context, req *pluginv1.ProbeRequest) (*pluginv1.ProbeResponse, error) {
	host := strings.TrimSpace(req.GetHost())
	if host == "" {
		return nil, sdk.InvalidArgument("dlna: a host is required")
	}
	locs, err := p.unicastSearch(ctx, host, 3*time.Second)
	if err != nil {
		return nil, sdk.InvalidArgument("dlna: " + err.Error())
	}
	for _, l := range locs {
		dctx, cancel := context.WithTimeout(ctx, describeTimeout)
		d, ok := describe(dctx, l)
		cancel()
		if ok {
			return &pluginv1.ProbeResponse{Device: d}, nil
		}
	}
	return nil, sdk.NotFound("dlna: no renderer answered at " + host)
}

// describe reads a device description and returns the device when it has an AVTransport service.
func describe(ctx context.Context, loc *url.URL) (*pluginv1.Device, bool) {
	root, err := goupnp.DeviceByURLCtx(ctx, loc)
	if err != nil {
		return nil, false
	}
	// The device itself or one of its embedded devices has the services.
	var avt, rc bool
	avt = len(root.Device.FindService(urnAVTransport)) > 0
	rc = len(root.Device.FindService(urnRenderingControl)) > 0
	if !avt || root.Device.UDN == "" {
		return nil, false
	}
	h, _ := json.Marshal(handle{Location: loc.String()})
	return &pluginv1.Device{Id: strings.TrimPrefix(root.Device.UDN, "uuid:"), Name: root.Device.FriendlyName, Manufacturer: root.Device.Manufacturer,
		Model: root.Device.ModelName, Address: loc.Host, Handle: h,
		Capabilities: &pluginv1.DeviceCapabilities{CanSeek: true, CanSetVolume: rc, CanMute: rc, CanShowSubtitles: true}}, true
}

// multicastSearch sends M-SEARCH from every non-loopback IPv4 interface address (so a host on two networks reaches both) and returns the
// distinct LOCATIONs. A device that answers only one of the two search targets is found by the other.
func multicastSearch(ctx context.Context, wait time.Duration, only []string) []*url.URL {
	addrs := localIPv4(only)
	if len(addrs) == 0 {
		return nil
	}
	var clients []httpu.ClientInterfaceCtx
	for _, a := range addrs {
		if c, err := httpu.NewHTTPUClientAddr(a); err == nil {
			clients = append(clients, c)
			defer func() { _ = c.Close() }()
		}
	}
	if len(clients) == 0 {
		return nil
	}
	multi := httpu.NewMultiClientCtx(clients)
	sctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	seen := map[string]*url.URL{}
	for _, st := range []string{stMediaRenderer, ssdp.SSDPAll} {
		resps, err := ssdp.RawSearch(sctx, multi, st, 2) //nolint:bodyclose // every response is closed in the loop below
		if err != nil {
			continue
		}
		for _, r := range resps {
			if l, err := r.Location(); err == nil {
				seen[l.String()] = l
			}
			_ = r.Body.Close()
		}
	}
	out := make([]*url.URL, 0, len(seen))
	for _, l := range seen {
		out = append(out, l)
	}
	return out
}

// localIPv4 lists the IPv4 addresses of the up, multicast-capable, non-loopback interfaces (only the named ones when only is not empty).
func localIPv4(only []string) []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagMulticast == 0 {
			continue
		}
		if len(only) > 0 && !containsFold(only, ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, ipn.IP.String())
			}
		}
	}
	return out
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// unicastSearch sends M-SEARCH straight to one host and returns the LOCATIONs in its answers. This works through a container's NAT and across
// subnets, where multicast does not. SSDP is always asked on port 1900: an administrator who types host:port usually copied the renderer's
// HTTP port (the one in its description URL), which is not where SSDP listens, so any port in host is ignored.
func (p *plugin) unicastSearch(ctx context.Context, host string, wait time.Duration) ([]*url.URL, error) {
	ip := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		ip = h
	}
	hostport := net.JoinHostPort(strings.Trim(ip, "[]"), strconv.Itoa(p.ssdpPort))
	raddr, err := net.ResolveUDPAddr("udp", hostport)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenPacket("udp", ":0")
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	deadline := time.Now().Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	for _, st := range []string{stMediaRenderer, ssdp.SSDPAll} {
		msg := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\nST: " + st + "\r\n\r\n"
		for i := 0; i < 2; i++ {
			_, _ = conn.WriteTo([]byte(msg), raddr)
		}
	}
	seen := map[string]*url.URL{}
	buf := make([]byte, 4096)
	for ctx.Err() == nil {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			break
		}
		resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf[:n])), nil)
		if err != nil {
			continue
		}
		if l, err := resp.Location(); err == nil {
			seen[l.String()] = l
		}
		_ = resp.Body.Close()
	}
	out := make([]*url.URL, 0, len(seen))
	for _, l := range seen {
		out = append(out, l)
	}
	return out, nil
}
