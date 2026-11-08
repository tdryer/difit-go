# difit-go

A Go port of [difit](https://github.com/yoshiko-pg/difit): a CLI that serves a
GitHub-like "Files changed" view of Git diffs in the browser, shipped as a
single standalone `difit` binary with the web UI embedded.

Work in progress; see [PLAN.md](PLAN.md).

## Building

Requires Go and, to build the UI, Node.js 22+ with pnpm.

```sh
make all      # build web/ into internal/web/assets/dist, then bin/difit
make test     # Go tests + web (vitest) tests
make check    # go vet, gofmt, oxlint, oxfmt
```

`go build ./cmd/difit` works without Node, but the binary then serves a
placeholder page instead of the UI.

At runtime difit needs only `git` on `PATH` (and `gh` for `--pr`).

## Layout

- `cmd/difit` — entrypoint
- `internal/` — Go CLI, server, and embedded assets
- `web/` — React client (Vite), embedded into the binary at build time
- `difit/` — upstream TypeScript source, kept locally as a reference during the port (gitignored)

## License

MIT; see [LICENSE](LICENSE). Derived from difit by yoshiko-pg.
