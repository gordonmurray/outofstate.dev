# Out of State (`oos`)

Find what's running outside your Terraform.

`oos` compares the resources in an AWS account with the Terraform states you supply and reports the ones that no state tracks. The first supported resource type is RDS DB instances. More will follow, along with cost.

`oos` reads Terraform state format version 4 (Terraform 0.12 and later, and OpenTofu).

> **Status:** early development. RDS DB instances only; cost is not reported yet.

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

## Usage

`oos` uses your normal AWS configuration (profiles, SSO, environment variables) to list RDS DB instances, and `terraform state pull` in each Terraform working directory you give it to read state through its configured backend.

```bash
# The current directory is the Terraform working directory
oos --profile default

# Several working directories, for example an app and a shared infrastructure repo
oos --profile default --source ./app-infra --source ./shared-infra

# Save the report for later, or for an AI coding agent to read
oos --profile default --source ./app-infra --source ./shared-infra --output outofstate.txt

# JSON, for scripts and agents (schema: docs/report.schema.json)
oos --profile default --source ./app-infra --format json --output outofstate.json
```

Each `--source` must be an initialised working directory (`terraform init` already run). `oos` never runs `init`, `plan`, `apply` or anything that writes. It reads the currently selected workspace; set `TF_WORKSPACE` to choose another. Use `--tf-binary tofu` for OpenTofu.

A state file works too, with `--state-file terraform.tfstate` (repeatable).

| Option | Default | Meaning |
|---|---|---|
| `--profile` | AWS default chain | AWS profile for discovery |
| `--region` | your profile's region | Region to scan, repeatable |
| `--source` | `.` if no `--source` or `--state-file` | Terraform working directory, repeatable |
| `--state-file` | | Terraform state JSON file, repeatable |
| `--tf-binary` | `terraform` | `terraform` or `tofu` |
| `--source-timeout` | `2m` | Deadline for each Terraform command |
| `--format` | `text` | `text` or `json` |
| `--output` | stdout | Write the report to a file |
| `--no-color` | | Disable colour (also `NO_COLOR`) |

### What the report means

| Status | Meaning |
|---|---|
| **Tracked in state** | A managed `aws_db_instance` in one of your states matches this instance. The report shows which state and address. |
| **No match found** | No supplied state tracks it, and every state was read fully. These are **import candidates**: review the configuration and run `terraform plan` before importing. |
| **Unable to verify** | No match, but a state could not be read fully, so the instance might be in it. |
| **Not covered** | Outside what `oos` checks so far: Aurora and Multi-AZ DB cluster members, DocumentDB, Neptune. |

"No match found" means *not in the states you supplied*. The instance may be managed by another state, by CloudFormation (its tags are shown), or by hand.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | Complete scan, everything in scope is tracked |
| 1 | Error, no report produced |
| 2 | Complete scan, some instances have no match |
| 3 | Partial scan: a state or region could not be read fully |

### AWS permissions

`oos` is read-only. It needs:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "sts:GetCallerIdentity",
        "rds:DescribeDBInstances",
        "rds:ListTagsForResource"
      ],
      "Resource": "*"
    }
  ]
}
```

Reading Terraform state uses your existing backend access (for example read access to the S3 state bucket).

### Secrets

Terraform state contains secrets such as database passwords. `oos` reads only the resource identifiers it needs, never writes state anywhere, and never copies Terraform's own error output into a report. When a source fails, the report says why and gives the command to run yourself to see Terraform's full output.

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

### End-to-end test

`make e2e` runs the full loop against a real AWS sandbox account: it creates three `db.t4g.micro` instances with Terraform, removes one from state, checks `oos` reports exactly that one, imports it back, checks `oos` is clean, and always destroys everything. It costs a little money, so it needs explicit confirmation and an otherwise empty region:

```bash
AWS_PROFILE=sandbox AWS_REGION=eu-west-1 E2E_CONFIRM=yes make e2e
```

### Releasing

```bash
git tag v0.1.0
git push origin v0.1.0
```

The `Release` workflow tests the tag, builds the binaries with GoReleaser and publishes a GitHub Release.
