// Command dlna is the official DLNA cast plugin (capability cast.target): it finds DLNA MediaRenderers (televisions,
// set-top boxes, software players) on the local network, hands them the URL of a media file and controls them (play, pause, seek, stop,
// volume). The host owns the device list, the session, the authorization behind the URL and play tracking; this plugin owns the protocol:
// SSDP discovery, UPnP descriptions, AVTransport and RenderingControl SOAP calls, DIDL-Lite and the quirks of the televisions.
package main

import "github.com/kryspal/plugin-sdk/sdk"

func main() {
	p := newPlugin()
	sdk.Main(sdk.Config{Plugin: p, CastTarget: p})
}
