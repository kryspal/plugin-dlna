package main

import (
	"context"
	_ "embed"
	"strings"
	"sync"
	"time"

	pluginv1 "github.com/kryspal/plugin-sdk/pluginv1"
)

const pluginID = "dlna"

// versionFile is the single source of the plugin version (also the <version> of the bundled directory).
//
//go:embed VERSION
var versionFile string

func version() string { return strings.TrimSpace(versionFile) }

type plugin struct {
	pluginv1.UnimplementedPluginServer
	pluginv1.UnimplementedCastTargetServer

	// ssdpPort and playWait are replaceable in tests.
	ssdpPort int
	playWait time.Duration // how long Load waits for the device to start playing
	now      func() time.Time

	mu      sync.Mutex
	clients map[string]*deviceClients // by device ID
}

func newPlugin() *plugin {
	return &plugin{ssdpPort: 1900, playWait: 5 * time.Second, now: time.Now, clients: map[string]*deviceClients{}}
}

// Describe declares cast.target, the protocol "dlna" and the permission network.lan (multicast and connections to devices on the local network).
func (p *plugin) Describe(_ context.Context, req *pluginv1.DescribeRequest) (*pluginv1.Manifest, error) {
	t := messages.For(req.GetLanguage())
	return &pluginv1.Manifest{
		Id: pluginID, Name: "DLNA", Description: t.T("desc"), Version: version(), ProtocolVersion: pluginv1.ProtocolVersion,
		Capabilities: []string{pluginv1.CapabilityCastTarget}, Permissions: []string{pluginv1.PermissionLAN}, CastProtocol: "dlna",
		SuggestedConcurrency: 8,
	}, nil
}

// Configure accepts anything: the plugin has no settings (per-device options come with each call from the host).
func (p *plugin) Configure(context.Context, *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	return &pluginv1.ConfigureResponse{}, nil
}
