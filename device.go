package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/huin/goupnp"
	"github.com/huin/goupnp/dcps/av1"
	"github.com/huin/goupnp/soap"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
	"github.com/kryspal/plugin-sdk/sdk"
)

const (
	urnAVTransport      = "urn:schemas-upnp-org:service:AVTransport:1"
	urnRenderingControl = "urn:schemas-upnp-org:service:RenderingControl:1"
	// clientTTL is how long the control clients built from a device description are reused before the description is read again
	// (a LOCATION can go stale when the device restarts on another port).
	clientTTL = 10 * time.Minute
	// httpTimeout bounds one request to a device (the host's per-call timeouts are the outer limit).
	httpTimeout = 8 * time.Second
)

// handle is the opaque Device.handle: where the device description is.
type handle struct {
	Location string `json:"loc"`
}

type deviceClients struct {
	loc     string
	avt     *av1.AVTransport1
	rc      *av1.RenderingControl1 // nil when the device has no RenderingControl
	builtAt time.Time

	// What the last poll saw while the device was playing: a renderer that reaches the end of the media often goes back to position 0 when it
	// stops, so the end is told from the last position seen playing (Status).
	mu               sync.Mutex
	lastPos, lastDur time.Duration
	wasPlaying       bool
}

// newHTTPClient is the HTTP client used for the SOAP calls to a device: no redirects (control URLs are used on the host of the description
// only), no keep-alive (some devices close the connection after each answer and fail on a reused one), and M-POST when a device answers
// 405 to POST.
func newHTTPClient() http.Client {
	return http.Client{
		Timeout:       httpTimeout,
		Transport:     &mpostTransport{base: &http.Transport{DisableKeepAlives: true, ResponseHeaderTimeout: httpTimeout}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// mpostTransport retries a SOAP POST as an M-POST (RFC 2774 mandatory extension) when the device answers 405 Method Not Allowed.
type mpostTransport struct{ base http.RoundTripper }

func (t *mpostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || req.Body == nil {
		return t.base.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}
	first := req.Clone(req.Context())
	first.Body, first.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	resp, err := t.base.RoundTrip(first)
	if err != nil || resp.StatusCode != http.StatusMethodNotAllowed {
		return resp, err
	}
	_ = resp.Body.Close()
	retry := req.Clone(req.Context())
	retry.Method = "M-POST"
	retry.Header.Set("MAN", `"http://schemas.xmlsoap.org/soap/envelope/"; ns=01`)
	if sa := retry.Header.Get("SOAPACTION"); sa != "" {
		retry.Header.Set("01-SOAPACTION", sa)
		retry.Header.Del("SOAPACTION")
	}
	retry.Body, retry.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	return t.base.RoundTrip(retry)
}

// clientsFor returns the control clients of a device, building them from its description when there are none or they are old.
func (p *plugin) clientsFor(ctx context.Context, d *pluginv1.Device) (*deviceClients, error) {
	var h handle
	if err := json.Unmarshal(d.GetHandle(), &h); err != nil || h.Location == "" {
		return nil, sdk.InvalidArgument("dlna: the device has no usable handle")
	}
	p.mu.Lock()
	c := p.clients[d.GetId()]
	if c != nil && c.loc == h.Location && p.now().Sub(c.builtAt) < clientTTL {
		p.mu.Unlock()
		return c, nil
	}
	p.mu.Unlock()
	c, err := buildClients(ctx, h.Location)
	if err != nil {
		return nil, err
	}
	c.builtAt = p.now()
	p.mu.Lock()
	p.clients[d.GetId()] = c
	p.mu.Unlock()
	return c, nil
}

func (p *plugin) forget(id string) {
	p.mu.Lock()
	delete(p.clients, id)
	p.mu.Unlock()
}

func buildClients(ctx context.Context, location string) (*deviceClients, error) {
	loc, err := url.Parse(location)
	if err != nil {
		return nil, sdk.InvalidArgument("dlna: bad device location")
	}
	root, err := goupnp.DeviceByURLCtx(ctx, loc)
	if err != nil {
		return nil, sdk.Unavailable("dlna: cannot read the device description: " + err.Error())
	}
	avts, err := av1.NewAVTransport1ClientsFromRootDevice(root, loc)
	if err != nil || len(avts) == 0 {
		return nil, sdk.Unavailable("dlna: the device has no AVTransport service")
	}
	c := &deviceClients{loc: location, avt: avts[0]}
	c.avt.SOAPClient.HTTPClient = newHTTPClient()
	if rcs, err := av1.NewRenderingControl1ClientsFromRootDevice(root, loc); err == nil && len(rcs) > 0 {
		c.rc = rcs[0]
		c.rc.SOAPClient.HTTPClient = newHTTPClient()
	}
	return c, nil
}

// fault returns the UPnP error code of a SOAP fault answer, or 0 and false for any other error (network, timeout).
func fault(err error) (code int, ok bool) {
	var sf *soap.SOAPFaultError
	if errors.As(err, &sf) {
		return sf.Detail.UPnPError.Errorcode, true
	}
	return 0, false
}
