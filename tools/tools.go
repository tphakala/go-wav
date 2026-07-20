//go:build tools

// Package tools anchors build-time-only dependencies so `go mod tidy` retains
// them. The ruleguard DSL backs the custom gocritic ruleguard matchers in
// rules/*.go (those files carry the `ruleguard` build tag and are read by
// golangci-lint, never by the normal toolchain). This file is never compiled
// into the module.
package tools

import _ "github.com/quasilyte/go-ruleguard/dsl"
