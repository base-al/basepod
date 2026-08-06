// Package openapi embeds BasePod's hand-written OpenAPI 3.1 spec
// (openapi.yaml, this directory) into the basepod binary — required for
// the repo's single-binary-integrity constraint (the spec must ship
// inside the binary, not be read from disk at runtime, which would break
// once the binary is copied/deployed anywhere its source tree isn't
// present).
//
// This tiny package exists only because of where the embed directive is
// allowed to live: go:embed patterns cannot reference a parent directory
// (verified directly: a `//go:embed ../../api/openapi.yaml` directive
// inside internal/api fails the build with "invalid pattern syntax"), so
// the embed has to sit beside openapi.yaml itself. internal/api/openapi.go
// imports this package to serve the bytes at GET /api/v1/openapi.yaml —
// mirrors web/embed.go's identical reason for existing (embedding the
// built dashboard, which likewise can't be embedded from inside
// internal/server).
package openapi

import _ "embed"

// YAML is the verbatim contents of openapi.yaml.
//
//go:embed openapi.yaml
var YAML []byte
