# Reliability fork maintenance

## Source and branches

- Upstream: [foxy1402/cline-proxy](https://github.com/foxy1402/cline-proxy).
- Fork: [weihu353-wq/cline-proxy](https://github.com/weihu353-wq/cline-proxy).
- Upstream baseline: `5a11408848cd558efe928d6c38abec44ba413de4`.
- `main` retains the upstream baseline and history.
- `codex/cline-reliability-r3` contains the reviewed r3 changes and this build documentation.

The six changed Go files match the r3 patch deployed on 2026-10-02:

```text
internal/app/cline_stream.go
internal/app/cline_stream_test.go
internal/app/proxy.go
internal/app/responses.go
internal/app/cline_reliability.go
internal/app/cline_reliability_test.go
```

The source patch's SHA-256 is
`60742eafe4e4b5cbba78aa8332ae91a98b1d70c1da245fa6a93a19e91977c91e`.
This branch already includes it; do not apply the patch a second time.

## Changes in r3

1. Preserve upstream HTTP 401, 403, 429 and 5xx status codes, return sanitized errors, and forward a valid `Retry-After`.
2. Retry once on another active account only for a recognized account quota 429, before any response output. Shared team/region TPM limits and unknown 429 responses do not trigger account rotation.
3. When nonstream-to-stream fallback fails, return the latest error so current failure and recovery information is retained.
4. Inspect the first complete single-line SSE event before committing HTTP 200, with a 64 KiB inspection limit. Reject initial errors, premature completion, EOF, invalid JSON, usage-only events and invalid choices as HTTP 502.
5. If an error arrives after streaming starts, terminate with a sanitized error. Responses and Anthropic converters must not append a successful completion. Requests that already emitted output are not replayed.

The regression tests cover quota classification, bounded retries, preserved error metadata, malformed/oversized initial SSE events, first-event replay, and error termination in the Responses and Anthropic converters.

These changes make failures explicit; upstream quota, shared limits and service availability can still cause requests to fail.

## Validate and build

Use a Go toolchain compatible with `go.mod` and its dependencies. The deployed build used the Go 1.26 Alpine image.

```sh
go test ./...
go vet ./...
git diff --check
docker build --platform linux/amd64 -f Dockerfile.reliability -t cline-proxy:reliability-r3 .
```

`Dockerfile.reliability` builds the binary from this checkout and copies it into the pinned upstream runtime image. It preserves the runtime filesystem, non-root user, health check and entrypoint. It targets Linux amd64; use the upstream `Dockerfile` if maintaining a separate multi-architecture build.

The runtime image is pinned by digest. The builder uses the version tag `golang:1.26-alpine`; byte-identical builds require pinning the builder digest as well.

The same six Go source files passed `go test ./...` and `go vet ./...` in the deployment environment on 2026-10-02. Canary and production checks exercised Chat and Responses, streaming and nonstreaming, and a function-call/tool-result round trip. Publication of this fork did not redeploy the service or rerun those live checks.

## Keep runtime data separate

The repository contains source, synthetic regression tests and build instructions. Account pools, API keys, credentials, sessions, production configuration and runtime logs belong in restricted deployment storage. Preserve the existing `.gitignore` and `.dockerignore` exclusions when changing the build.

## Updating from upstream

Fetch upstream changes, inspect the diff from the recorded baseline, and review overlaps with these six Go files. Integrate updates on a new branch, rerun tests and vet, and validate an isolated deployment before changing production. Preserve the previous image and configuration as rollback points.

The inherited image-publishing workflow is configured for `main`, version tags and manual dispatch. Pushing this reliability branch does not match its `push` trigger. Enabling Actions or publishing images is a separate maintenance operation.
