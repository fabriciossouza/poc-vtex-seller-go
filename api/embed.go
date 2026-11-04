// Package api embeds the OpenAPI specification served at /docs/openapi.yaml.
package api

import _ "embed"

//go:embed openapi.yaml
var Spec []byte
