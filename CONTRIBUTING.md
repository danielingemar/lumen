# Contributing

Thanks for helping. Lumen is small and standard-library only, so getting started is quick.

## Set up

```bash
git clone https://github.com/danielingemar/lumen.git
cd lumen
go version        # 1.22 or newer
make test         # gofmt check, go vet, go test ./...
```

No dependencies are downloaded: the project uses only the Go standard library. Keep it that way unless there is a strong reason.

To run the whole stack locally you need Docker: `./dev.sh` (see the README).

## Before you open a pull request

- `make test` passes. CI also runs the race detector, cross-builds the agent and checks the SQL against a real ClickHouse
  engine (`pip install chdb`, see "Development" in the README).
- New behaviour has tests. Look at `internal/server/*_test.go` for HTTP tests with fake stores and `internal/backup` for a fake database.
- UI is a single file, `internal/ui/index.html`. There is no build step. Text goes through the `el()` helper, which never
  interprets data as HTML: keep it that way, never use `innerHTML` with data.
- Anything that touches tenants, permissions or secrets needs a test that proves another tenant or a read-only user is refused.
- Update the README and `CHANGELOG.md` if behaviour changes.
- Scripts must stay executable in git: `git update-index --chmod=+x path/to/script.sh`.

## Commit messages

Short imperative subject (`Add host settings page`), then a paragraph on why if it is not obvious.

## Reporting bugs and ideas

Use the issue templates. For security problems see [SECURITY.md](SECURITY.md) and do not open a public issue.

By contributing you agree that your contribution is licensed under the Apache License 2.0 (the `ee/` directory is separately licensed).
