# Dev and Test Tasks for golang-jirib-utils

These tasks are orchestrated with [mise](https://mise.jdx.dev/):

- `mise run test` — fast, hermetic build, vet, and unit tests with race detection across all library packages.
- `mise run test-integration` — runs dedicated integration tests under `tests/integration/` exercising cross-package flows (IPC control socket, sandboxed execution pipeline, HTTP API with auth and metrics).
- `mise run clean` — cleans Go test cache (`go clean -testcache`) and removes temporary artifacts or coverage profiles.

Run `mise tasks` to list all available tasks.
