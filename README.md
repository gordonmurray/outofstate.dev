# Out of State (`oos`)

Find what's running outside your Terraform.

`oos` compares the resources in an AWS account with the Terraform states you supply, and reports the ones that no state tracks, and what they cost. The first supported resource type is RDS DB instances. More will follow.

> **Status:** early development. The current release only prints its version.

## Install

Releases are published on GitHub for Linux (amd64 and arm64). With the [GitHub CLI](https://cli.github.com/) logged in:

```bash
scripts/install.sh                  # latest release
scripts/install.sh --version v0.3.1 # a specific release
```

The script downloads the release into a fresh temporary directory, verifies its SHA-256 checksum, and installs `oos` into `~/.local/bin`.

## Usage (planned)

```bash
oos --profile default
oos --profile default --source ./app-infra --source ./shared-infra
oos --profile default --source ./app-infra --output outofstate.txt
oos --profile default --source ./app-infra --format json --output outofstate.json
```

## Development

Everything runs in Docker. You need Docker, `make`, `git` and `gh`. No local Go install.

```bash
make            # list targets
make test       # go test -race
make lint       # go vet + golangci-lint
make build      # static binary in dist/oos
make ci         # everything CI runs
```

Tool versions (Go, golangci-lint, GoReleaser, Terraform) are pinned in [`tools/Dockerfile`](tools/Dockerfile) and nowhere else.

### Releasing

```bash
git tag v0.1.0
git push origin v0.1.0
```

The `Release` workflow tests the tag, builds the binaries with GoReleaser and publishes a GitHub Release.

## License

Apache-2.0
