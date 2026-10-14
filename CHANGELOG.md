# Changelog

All notable changes to this plugin are listed here. The version in `VERSION` is the single source of the version number; a published version is never changed.

## 1.0.2

- Rebuilt with Go 1.27.2 and golang.org/x/net v0.60.0 (security fixes).
- First release from the standalone repository; no functional change to the casting behavior since the last published catalog version (1.0.1), plus the documentation and source cleanup for the public repository.

## 1.0.1

- Cast target for DLNA/UPnP MediaRenderer devices: SSDP discovery, unicast probe, load with DIDL fallbacks, transport and volume control, status polling.
- Development history before this release lives in the Kryspal Media main repository; this repository starts from this version.
