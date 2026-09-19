# cpro

**cpro** — short for **C**laude Code **Pro** — is a Linux CLI that manages
multiple Claude Code accounts by email. Each account keeps its own isolated
authentication, settings, and history.

![Picking run, an account, YOLO mode, and launching claude](.images/cpro-header.svg)

## Install

Requirements: Linux, Go 1.25.8+, and `claude` on `PATH`.

```bash
go build -o bin/cpro ./src   # build
go install ./src             # install to $(go env GOPATH)/bin
```

## Quick start

```bash
cpro login you@example.com
cpro                         # interactive launcher: run / status / watch / menu
```

## Documentation

See [`docs/README.md`](docs/README.md) for every command and what it's for.

## Verification

```bash
go build -o bin/cpro ./src
go vet ./...
go test ./...
```

## License

Dual-licensed under either of [Apache License, Version 2.0](LICENSE-APACHE) or
[MIT license](LICENSE-MIT), at your option.
