# Contributing

Thanks for helping. Issues and pull requests are welcome.

## Before you start

For anything larger than a small fix, open an issue first so the approach can be agreed before you spend time on it. New resource types are planned; an issue is the place to suggest one.

## Development setup

Everything runs in Docker, so you need Docker, `make` and `git`. No local Go install.

```bash
make test    # tests with the race detector
make lint    # go vet and golangci-lint
make fmt     # format code
make ci      # everything CI runs
```

Tool versions are pinned in `tools/Dockerfile`.

## Pull requests

- Keep each pull request focused on one change.
- Add or update tests. Golden files under `cmd/oos/testdata` are regenerated with `go test ./cmd/oos -update` (run it inside `make shell`); review the diff.
- `make ci` must pass.
- If you change the JSON report, update `docs/report.schema.json` and follow the compatibility rules described in it.

## Ground rules for the code

- `oos` is read-only. It must never change AWS resources or Terraform state.
- Never print, log or store Terraform state contents or Terraform's own error output. Errors use fixed messages.
- Prefer a clear "unable to verify" over a guess.

## The end-to-end test

`make e2e` creates billable RDS instances in a real AWS account. Read the "End-to-end test" section of the README before running it.
