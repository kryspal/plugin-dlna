# plugin-dlna

Play your videos on a television or other DLNA receiver on your home network. The receiver fetches the video straight from the server, so nothing is transcoded. An official [Kryspal Media](https://github.com/kryspal) plugin, built on [plugin-sdk](https://github.com/kryspal/plugin-sdk).

```sh
go build -o dlna .
go vet ./... && go test ./...
```

Implementation notes: [docs/design.md](docs/design.md).

Licensed under the Apache License 2.0 (see `LICENSE` and `NOTICE`).

Official builds are signed and published through the Kryspal plugin catalog; builds from source are treated as third-party plugins.
