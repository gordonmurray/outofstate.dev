# Out of State (`oos`)

Find what's running outside your Terraform.

`oos` compares the resources in an AWS account with the Terraform states you supply, reports the ones that no state tracks, and shows what the account spends on that type of resource. The first supported resource type is RDS DB instances. More will follow.

`oos` reads Terraform state format version 4 (Terraform 0.12 and later, and OpenTofu).

> **Status:** early development. The current release only prints its version.

## Quick start

Install the latest release (Linux, amd64 or arm64) into `~/.local/bin`.

While the repository is private, use the [GitHub CLI](https://cli.github.com/), logged in with `gh auth login`:

```bash
gh api -H "Accept: application/vnd.github.raw" "repos/gordonmurray/outofstate.dev/contents/scripts/install.sh?ref=main" | bash
```

Once the repository is public, plain `curl` works too:

```bash
curl -fsSL https://raw.githubusercontent.com/gordonmurray/outofstate.dev/main/scripts/install.sh | bash
```

Then check it. If `~/.local/bin` is not on your `PATH` yet, the installer says so. Add it with `export PATH="$HOME/.local/bin:$PATH"` (and to your shell profile).

```bash
oos --version
```

To install a specific release, pass options after `bash -s --`:

```bash
... | bash -s -- --version v0.3.0
... | bash -s -- --dest /usr/local/bin
```

The script downloads the release into a fresh temporary directory, verifies its SHA-256 checksum, and only then installs `oos`. From a clone of the repository you can also run `scripts/install.sh` directly.

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
