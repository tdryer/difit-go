# difit-go plan

Goal: replace difit's Node runtime (CLI + Express server) with a single,
statically linked Go binary named `difit` that embeds the web UI. The project
is `difit-go`, a one-time fork of difit v5.0.12; it does not track upstream. Behavior, HTTP API, and CLI
surface stay compatible with difit v5.0.12 (`difit/`).

## Scope

| Part of `difit/`                         | Plan                                                                           |
| ---------------------------------------- | ------------------------------------------------------------------------------ |
| `src/cli` (~1.4k LOC)                    | Port to Go                                                                     |
| `src/server` (~2.7k LOC)                 | Port to Go                                                                     |
| `src/utils` used by the server           | Port to Go; TS copy stays for the client                                       |
| `src/client` (React UI)                  | **Keep as-is.** Build with Vite, embed the output with `go:embed`              |
| `src/site` (GitHub Pages demo)           | Out of scope; stays TS-only                                                     |
| `packages/vscode`                        | Dropped                                                                        |
| `scripts/` perf tooling                  | Out of scope; possibly rewrite later against the Go server                     |

Rewriting the UI is not worth it: it is the bulk of the code, has no Node
runtime dependency once built, and a static bundle embeds cleanly. Node/pnpm is
needed only at **build** time, never at runtime. Runtime dependency: `git` on
PATH (same as today), and `gh` for `--pr`.

## Repository layout

```
difit-go/
  go.mod                      module github.com/tdryer/difit-go
  cmd/difit/main.go           entrypoint
  internal/
    cli/                      flag parsing, arg resolution, stdin, untracked prompt, background, `comment` subcommand
    server/                   net/http handlers, comment sessions, SSE (watch, heartbeat), shutdown
    gitdiff/                  git invocation, unified-diff parser, blobs, line counts, revisions
    watcher/                  fsnotify-based recursive watcher, per-DiffMode config, debounce
    generated/                generated-file detection (path + content heuristics, gitattributes)
    comments/                 comment imports/merge/normalize/serialize, output formatting
    editor/                   editor options + spawn spec ("open in editor")
    userconfig/               ~/.difit/config.json read / patch
    github/                   PR URL parsing, `gh` patch + GraphQL review threads
    types/                    JSON structs mirroring src/types/diff.ts & watch.ts
    web/                      web.go (//go:embed all:assets): assets/dist (Vite output, gitignored)
                              and assets/fallback (checked-in "UI not built" page)
  web/                        the client: src/client, public/, vite/tailwind/postcss config,
                              package.json trimmed to client deps, client tests
  testdata/                   fixture repos (scripted), golden JSON shared by Go + TS tests
  Makefile                    `make web`, `make build`, `make test`, `make release`
  .goreleaser.yaml
```

`difit/` was a read-only reference (and the Node side of the conformance
harness) during the port. It has been removed; the conformance test now
replays responses recorded from it, and a fresh upstream checkout in `difit/`
is needed only to record new scenarios. The client lives in `web/` and is
edited freely.

## Technical decisions

- **Go ≥ 1.24, `CGO_ENABLED=0`.** Pure-Go deps only, so cross-compiling is trivial.
- **HTTP:** stdlib `net/http` with Go 1.22 method+wildcard patterns
  (`GET /api/blob/{path...}`). No framework. Middleware for the CORS headers
  and JSON / `text/plain` (sendBeacon) bodies.
- **Git:** shell out to the `git` binary (`os/exec`), exactly as `simple-git`
  does. Not go-git: it doesn't match git's diff output, `.gitattributes`
  handling, worktrees, or `--intent-to-add` closely enough.
- **Diff parsing:** port `GitDiffParser.parseUnifiedDiff/parseFileBlock/parseChunks`
  line by line (git format and plain `diff -u` format for stdin).
- **File watching:** `github.com/fsnotify/fsnotify` replaces `@parcel/watcher`.
  fsnotify isn't recursive, so walk the tree and add directories, add new
  directories on Create, and skip ignored paths (the same ignore globs, plus
  `git check-ignore --stdin` in batches for `.gitignore`). Watch the real git
  dir from `git rev-parse --git-dir` (worktrees). Keep the 300 ms debounce and
  the "relevant git file" filter per DiffMode. If the watcher fails, warn and continue, as today.
- **CLI:** `github.com/spf13/cobra` (pflag). Must reproduce commander's
  semantics: two optional positionals (`[commit-ish] [compare-with]`), `--no-open`,
  repeatable `--comment`, `-v/--version`, the `comment` subcommand,
  and "positional options" (options after a subcommand belong to it). If cobra
  fights this, use plain pflag with manual subcommand dispatch.
- **Browser open:** small internal helper (`xdg-open` / `open` /
  `rundll32 url.dll,FileProtocolHandler`), honoring `BROWSER` like `open` does.
- **Static files:** `http.FileServerFS` over the embedded `dist`, with
  `index.html` fallback for `/`. `DIFIT_DEV=1` serves nothing (Vite dev server
  proxies `/api` to the Go server, as today).
- **Background mode:** re-exec `os.Executable()` with `DIFIT_BACKGROUND_CHILD=1`,
  `SysProcAttr{Setsid: true}` (Windows: `DETACHED_PROCESS`), stdin to
  /dev/null, stderr piped, and stdout piped for the JSON handshake
  (`DIFIT_BACKGROUND_READY {...}` line) in place of Node IPC. Parent prints the
  handshake JSON and exits; same timeout / stderr-on-failure behavior.
- **Shutdown:** heartbeat SSE disconnect → 100 ms grace → stop watcher → print
  comments → exit (unless `--keep-alive`). On SIGINT, print comments straight
  from in-memory state; no HTTP round trip to itself.
- **Port fallback:** loop `net.Listen` from the preferred port (default 4966)
  on `EADDRINUSE`, logging the same "Port X is busy" message.
- **IDs/hashes:** `crypto/sha256` hex for repositoryId / commentImportId, so
  client localStorage keys stay identical (browser state keeps working across the switch).
- **Version:** injected with `-ldflags "-X main.version=..."`, sourced from the
  release tag.

## Keeping Go and TS in sync

`commentImports`, `commentFormatting`, `editorOptions`, `diffSelection`, and
`fileUtils` run in both the server and the client. They get ported to Go, and
the TS stays for the client. To stop drift:

1. Turn the existing vitest cases for these modules into table fixtures in
   `testdata/golden/*.json` (input → expected output).
2. A Go test and a small vitest file both run every fixture.

## Testing strategy

1. **Unit tests:** port the server/CLI vitest suites (`git-diff.test.ts`,
   `server.test.ts`, `index.test.ts`, `utils.test.ts`, `github.test.ts`,
   `comment.test.ts`, `background.test.ts`, `file-watcher.test.ts`,
   `generated-file-check.test.ts`, `user-config.test.ts`; ~6k LOC) to
   table-driven Go tests. Fixture repos are built in `t.TempDir()` with
   scripted git commands.
2. **API conformance harness (the key safety net):** a script that starts both
   the Node server (`difit/dist/cli`) and the Go binary on the same fixture
   repos and scenarios (working/staged/./commit/range/merge-base/stdin/
   ignoreWhitespace/context/renames/binary/images/generated files/untracked),
   hits every endpoint, and diffs the normalized JSON. It also compares the
   `/api/watch` event stream per diff mode and `--pr` imports through a fake
   `gh`. The Node responses are recorded in `internal/conformance/testdata`,
   so `go test` replays them on every platform without Node.
3. **Client tests:** existing vitest suite for `web/` runs unchanged.
4. **E2E smoke:** Playwright loads the UI from the Go binary, checks that
   files render, adds comments from the UI and the CLI, refreshes after a
   commit and a working-tree edit (watcher), answers the untracked-files
   prompt, checks that `--background` outlives the browser, and checks that
   closing the tab makes the process exit and print the comments.

## Distribution

- goreleaser: linux/darwin/windows × amd64/arm64, `CGO_ENABLED=0`, `-trimpath
  -s -w`. Before the build hook: `pnpm -C web install --frozen-lockfile && pnpm -C web build`.
- Artifacts: tarballs/zips + checksums, GitHub Release. No npm wrapper.
- Plain `go build ./cmd/difit` without the web build still compiles and serves
  `internal/web/assets/fallback/index.html` ("UI not built, run `make web`").

## Milestones

1. **Skeleton** — go.mod, layout, `web/` moved over, Vite outputs to
   `internal/web/assets/dist`, embed + static serving, Makefile, CI (go test, vitest, lint).
   ✅ binary serves the UI shell. **Done.**
2. **Diff core** — `types`, `gitdiff` (resolve/merge-base/diff/parse/stdin/
   blob/line-count/revisions/default branch), `generated`.
   ✅ ported git-diff + generated tests pass. **Done.**
3. **Server API** — all `/api/*` routes, comment sessions + versions,
   comment imports, user settings, open-in-editor, diff/generated-status
   caches, heartbeat/keep-alive shutdown. ✅ conformance harness green for
   non-watch scenarios. **Done.**
4. **CLI** — all flags and validations, arg resolution and DiffMode, stdin
   detection, untracked prompt + intent-to-add, `--pr` via `gh`, `comment`
   subcommand, background mode, SIGINT output. ✅ ported CLI tests pass. **Done.**
5. **Watcher** — fsnotify recursive watcher, `/api/watch` SSE, cache
   invalidation, `commentsChanged` events. ✅ watcher tests + E2E reload. **Done.**
6. **Release** — goreleaser, cross-platform smoke on Linux/macOS/Windows CI
   runners (paths, editor spawn, browser open, background detach), README.
   **Done** (goreleaser snapshot builds all six targets; the macOS/Windows CI
   jobs have not run yet, since there is no remote).
7. **Follow-ups (optional)** — site-data export via the binary, perf benchmarks.

## Decisions

- One-time fork: no upstream syncing; `web/` diverges freely.
- Binary is `difit`; project/repo is `difit-go`.
- No VS Code extension, no npm wrapper.

## Deliberate deviations from difit v5.0.12

- SIGINT prints the review comments in stdin mode too.
- Malformed JSON bodies get a JSON 400 instead of Express's HTML error page;
  bodies are limited to 10 MB.
- `OPTIONS` preflight requests return 204.
- Background handshake over stdout instead of Node IPC; errors from the
  child are not prefixed with `Error:` twice.
- `--help` uses cobra's layout.
- `--version`, the shortcuts dialog and `GET /api/version` name difit-go; the
  UI's "Star on GitHub" link is gone, so bugs aren't reported upstream.
