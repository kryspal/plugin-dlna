package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DIDL-Lite and the per-device compatibility profile.
//
// There is no way to ask a DLNA television what it can play or how it wants the metadata, so Load tries the metadata in decreasing order of
// completeness (full, lite, empty) when the device answers with a SOAP fault, and the plugin keeps the variant that worked in the device state
// (the host stores it) so the next cast goes straight to it. The administrator can force a choice with the device options text.

const (
	didlFull  = "full"
	didlLite  = "lite"
	didlEmpty = "empty"

	featuresFull = "DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000"
)

var didlOrder = []string{didlFull, didlLite, didlEmpty}

// compat is the plugin's memory of one device: the DIDL variant that worked.
type compat struct {
	DIDL string `json:"didl,omitempty"`
}

func parseCompat(state []byte) compat {
	var c compat
	_ = json.Unmarshal(state, &c)
	if c.DIDL != didlFull && c.DIDL != didlLite && c.DIDL != didlEmpty {
		c.DIDL = ""
	}
	return c
}

func (c compat) marshal() []byte {
	b, _ := json.Marshal(c)
	return b
}

// options is what the administrator wrote for the device: lines or ";"-separated key=value pairs. Unknown keys are ignored.
//
//	mime=video/mpeg       content type used instead of the one derived from the container
//	features=none         send "*" as the fourth field of protocolInfo (and no contentFeatures header)
//	didl=lite             force a DIDL variant (full, lite, empty)
//	subtitle=none         do not describe the subtitle to the device
type options struct {
	mime, features, didl, subtitle string
}

func parseOptions(text string) options {
	var o options
	for _, part := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == ';' || r == '\r' }) {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		switch k {
		case "mime":
			if strings.Contains(v, "/") && !strings.ContainsAny(v, " <>\"&") && len(v) < 100 {
				o.mime = v
			}
		case "features":
			if v == "none" || v == "full" {
				o.features = v
			}
		case "didl":
			if v == didlFull || v == didlLite || v == didlEmpty {
				o.didl = v
			}
		case "subtitle":
			if v == "none" || v == "all" {
				o.subtitle = v
			}
		}
	}
	return o
}

// mimeFor is the content type for a container (the host serves the same one).
func mimeFor(container string) string {
	switch strings.ToLower(strings.TrimPrefix(container, ".")) {
	case "mkv", "matroska", "webm":
		return "video/x-matroska"
	case "mp4", "m4v", "mov":
		return "video/mp4"
	case "ts", "m2ts", "mts":
		return "video/mpeg"
	case "avi":
		return "video/x-msvideo"
	case "wmv", "asf":
		return "video/x-ms-wmv"
	case "mpg", "mpeg", "vob":
		return "video/mpeg"
	}
	return "video/mp4"
}

func featuresFor(o options) string {
	if o.features == "none" {
		return "*"
	}
	return featuresFull
}

func esc(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// hms formats a duration as H:MM:SS (whole seconds: some televisions fail on fractions).
func hms(d time.Duration) string {
	s := int64(d / time.Second)
	if s < 0 {
		s = 0
	}
	return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
}

// parseHMS reads H:MM:SS with an optional fraction; anything else (NOT_IMPLEMENTED, empty) is zero.
func parseHMS(s string) time.Duration {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 3 || strings.HasPrefix(strings.TrimSpace(s), "-") {
		return 0
	}
	var total float64
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil || v < 0 {
			return 0
		}
		total = total*60 + v
	}
	return time.Duration(total * float64(time.Second))
}

type didlMedia struct {
	title, url, poster, mime, features string
	duration                           time.Duration
	size                               int64
	subURL                             string // empty: no subtitle
}

// buildDIDL returns the metadata for SetAVTransportURI in the given variant.
func buildDIDL(variant string, m didlMedia) string {
	if variant == didlEmpty {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/"`)
	if variant == didlFull {
		b.WriteString(` xmlns:sec="http://www.sec.co.kr/" xmlns:pv="http://www.pv.com/pvns/"`)
	}
	b.WriteString(`><item id="1" parentID="0" restricted="1"><dc:title>` + esc(m.title) + `</dc:title><upnp:class>object.item.videoItem.movie</upnp:class>`)
	if variant == didlFull && m.poster != "" {
		b.WriteString(`<upnp:albumArtURI>` + esc(m.poster) + `</upnp:albumArtURI>`)
	}
	b.WriteString(`<res protocolInfo="http-get:*:` + esc(m.mime) + `:` + esc(m.features) + `"`)
	if m.duration > 0 {
		b.WriteString(` duration="` + hms(m.duration) + `"`)
	}
	if m.size > 0 {
		b.WriteString(` size="` + strconv.FormatInt(m.size, 10) + `"`)
	}
	if variant == didlFull && m.subURL != "" {
		b.WriteString(` pv:subtitleFileType="srt" pv:subtitleFileUri="` + esc(m.subURL) + `"`)
	}
	b.WriteString(`>` + esc(m.url) + `</res>`)
	if variant == didlFull && m.subURL != "" {
		b.WriteString(`<res protocolInfo="http-get:*:text/srt:*">` + esc(m.subURL) + `</res>`)
		b.WriteString(`<sec:CaptionInfoEx sec:type="srt">` + esc(m.subURL) + `</sec:CaptionInfoEx>`)
		b.WriteString(`<sec:CaptionInfo sec:type="srt">` + esc(m.subURL) + `</sec:CaptionInfo>`)
	}
	b.WriteString(`</item></DIDL-Lite>`)
	return b.String()
}
