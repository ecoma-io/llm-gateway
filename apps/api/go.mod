// The Ecoma LLM Gateway API service.
//
// The module path is spelled in full because Go's import path IS the name;
// living under apps/api of the ecoma-io/llm-gateway monorepo is the accepted
// cost of the monorepo decision — the same arrangement every module in this
// organisation carries.
module github.com/ecoma-io/llm-gateway/apps/api

go 1.26

// valkey-go is the sole RESP client. deploy/redis/README.md records why it is
// the smallest reasonable dependency and why Valkey is selected; the Go
// standard library has no Redis-compatible client.
require github.com/valkey-io/valkey-go v1.0.78

require golang.org/x/sys v0.47.0 // indirect
