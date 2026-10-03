# auto-dev-cycle

An unattended software-development cycle that turns requirements into merged, gated and reviewed changes on any git
repository, harvesting model capacity from wherever it can find it. Working name; see `docs/plan/` for the first
release plan and `docs/design/m0/` for the M0 contracts.

Status: **M0 (contracts and foundations)**, under construction. Nothing here runs a cycle yet.

## Build

Requirements: Linux (the controller platform; plan §2.14), `curl`, `jq`, `tar`, `sha256sum`, `git`, GNU make. No system Go is needed.

```sh
make toolchain   # installs the Go toolchain pinned in go.mod into ~/.local/go (checksum-verified)
make build       # bin/adcd, bin/adc, bin/adc-agent
make ci          # everything CI runs
```

`make help` lists every target.
