# difit-go

A Go port of [difit](https://github.com/yoshiko-pg/difit): a CLI that serves a
GitHub-like "Files changed" view of Git diffs in the browser, shipped as a
single standalone `difit` binary with the web UI embedded.

It is a one-time fork of difit v5.0.12. The CLI flags, HTTP API, and UI match
that release (see [Differences from difit](#differences-from-difit)); there is
no npm package or VS Code extension.

## Install

Download an archive for your platform (Linux, macOS, Windows; amd64 and
arm64) from the releases page and put `difit` on your `PATH`. At runtime it
needs only `git` (and the GitHub CLI `gh` for `--pr`).

Or build from source (see [Building](#building)).

## Usage

```sh
difit                      # the HEAD commit
difit HEAD~3               # a specific commit
difit main feature         # compare two revisions
difit main --merge-base    # changes since the merge base with main
difit working              # unstaged changes
difit staged               # staged changes
difit .                    # all uncommitted changes
git diff | difit           # any unified diff on stdin
difit --pr https://github.com/owner/repo/pull/123
```

The diff opens in your browser. Closing the tab stops the server and prints
your review comments to the terminal, ready to paste into an AI coding agent.
Use `--keep-alive` to keep serving, or `--background` to detach and print the
server's URL as JSON.

Other flags: `--port`, `--host`, `--no-open`, `--context <lines>`,
`--include-untracked`, `--clean`, and `--comment <json>` (repeatable) to seed
comments. Run `difit --help` for details.

Comments on a running server can be managed from the command line:

```sh
echo '{"type":"thread","filePath":"src/a.ts","position":{"side":"new","line":3},"body":"Why?"}' \
  | difit comment add
difit comment get
difit comment resolve <thread-id>
```

User settings are stored in `~/.difit/config.json` (override the directory
with `DIFIT_CONFIG_DIR`).

## Differences from difit

- `Ctrl+C` prints the review comments in stdin mode too.
- Malformed JSON request bodies get a JSON `400` response instead of an HTML
  error page; request bodies are limited to 10 MB.
- CORS preflight (`OPTIONS`) requests return `204`.
- `--background` hands the server info from the detached process to the
  parent over a pipe rather than Node IPC; the printed JSON is the same.
- `--help` output uses cobra's layout.

## Building

Requires Go and, to build the UI, Node.js 22+ with pnpm.

```sh
make all          # build web/ into internal/web/assets/dist, then bin/difit
make test         # Go tests + web (vitest) tests
make check        # go vet, gofmt, oxlint, oxfmt
make e2e          # Playwright smoke test against bin/difit (needs playwright)
make conformance  # compare the API with upstream difit (needs DIFIT_REFERENCE)
make release-snapshot  # goreleaser build of all platforms into dist/
```

`go build ./cmd/difit` works without Node, but the binary then serves a
placeholder page instead of the UI.

`make conformance` runs upstream difit's Node server and the Go binary on the
same fixture repository and compares every API response. It needs a built
checkout of difit v5.0.12, by default in `difit/`:

```sh
git clone --branch v5.0.12 https://github.com/yoshiko-pg/difit
(cd difit && pnpm install && ./node_modules/.bin/tsc -p tsconfig.cli.json && ./node_modules/.bin/vite build)
make conformance
```

Releases are built by goreleaser when a `v*` tag is pushed.

## Layout

- `cmd/difit` — entrypoint
- `internal/` — Go CLI, server, git diff parsing, watcher, and embedded assets
- `web/` — React client (Vite), embedded into the binary at build time
- `testdata/golden` — fixtures shared by the Go and TypeScript tests
- `e2e/` — Playwright smoke test
- `difit/` — upstream TypeScript source, used locally as the conformance reference (gitignored)

## License

MIT; see [LICENSE](LICENSE). Derived from difit by yoshiko-pg.
