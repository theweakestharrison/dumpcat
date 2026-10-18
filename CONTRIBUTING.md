# Contributing to DumpCat

Thanks for taking the time to help. DumpCat looks after other people's data, so changes are judged first on whether
they keep backups correct and restorable, and only then on everything else.

## Reporting a bug

Open an issue and include:

- the DumpCat version (`dumpcat version`) and the operating system;
- the database server and its version (`SELECT VERSION();`);
- the backup method and the storage type involved;
- what you did, what you expected and what happened;
- the task log from the backup or restore detail page. Secrets are masked in logs, but read it once before posting.

Security problems are the exception: do not open a public issue, follow [SECURITY.md](SECURITY.md) instead.

## Suggesting a feature

Open an issue describing the problem you want solved before writing a large change. It saves you from building
something that does not fit, and it lets others point out existing ways to do it.

## Development setup

You need Go 1.26 or newer. No C compiler is needed: everything builds with `CGO_ENABLED=0`, and SQLite is pure Go.
For manual testing, use a MySQL or MariaDB server you can afford to break, for example a local container.

```bash
go build -o dumpcat ./cmd/dumpcat
DUMPCAT_DATA_DIR=./data DUMPCAT_LISTEN=127.0.0.1:6369 DUMPCAT_WEB_DIR=./web ./dumpcat serve
```

With `DUMPCAT_WEB_DIR` the UI is served from `web/` on disk, so a page reload picks up UI changes. `./data` holds the
development database and master key; it is ignored by git.

The source layout is described in the [manual](docs/manual.md#15-development).

## Before you open a pull request

```bash
gofmt -l ./cmd ./internal     # must print nothing
go vet ./...
go test ./...
```

On Windows, also run the tests there if you touched Windows-specific code (`*_windows.go`, `scripts/install.ps1`,
`build.ps1`).

## Guidelines

- **Restorability first.** A change to the backup, encryption or restore path needs a test, or a clear description of
  how you checked that existing backups still restore.
- **Keep the database untouched until it is safe.** Restores check every file before they drop or replace anything;
  keep it that way.
- **Secrets.** Never put passwords or keys on a command line or in a log. Database passwords go through temporary option
  files (0600), and stored secrets are encrypted with the master key.
- **Schema changes.** Append a new entry to `migrations` in `internal/store/store.go`. Never edit an existing entry:
  installed databases have already applied it.
- **Dependencies.** Prefer the Go standard library. A new module needs a good reason and a license compatible with
  Apache-2.0. After changing `go.mod`, run `go run ./scripts/notices` to regenerate `THIRD_PARTY_NOTICES.md`; the
  build scripts refuse to package an out-of-date file.
- **UI.** The web UI is plain HTML, CSS and JavaScript in `web/`, with no build step and no external CDN, so that it
  works offline.
- **Platforms.** DumpCat runs on Linux and Windows. Put OS-specific code in `_windows.go` / `_other.go` files and keep
  both building.
- **Documentation.** If users will notice the change, update [docs/manual.md](docs/manual.md) and
  [docs/manual.vi.md](docs/manual.vi.md). If you cannot write the Vietnamese part, say so in the pull request and
  someone will fill it in.
- **Changelog.** Add a line describing the change under the top section of [CHANGELOG.md](CHANGELOG.md). That section
  becomes the release notes shown in the UI.

## Pull requests

- Keep each pull request to one topic.
- Explain what changed and why, and how you tested it (database version, operating system, backup method).
- Small commits with clear messages make review easier.

## License

DumpCat is licensed under the [Apache License 2.0](LICENSE). By submitting a contribution, you agree that it is
licensed under the same terms, as described in section 5 of the license.
