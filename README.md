# Out of State (`oos`)

Find what's running outside your Terraform.

`oos` compares the resources in an AWS account with the Terraform states you supply and reports the ones that no state tracks. The first supported resource type is RDS DB instances. More will follow.

`oos` reads Terraform state format version 4 (Terraform 0.12 and later, and OpenTofu).

> **Status:** early development. RDS DB instances only.

## Quick start

Install the latest release into `~/.local/bin`. Builds are available for Linux and macOS, on Intel/AMD and ARM (Apple Silicon):

```bash
curl -fsSL https://raw.githubusercontent.com/gordonmurray/outofstate.dev/main/scripts/install.sh | bash
```

If you prefer to read the script first, download it, check it, then run it with `bash install.sh`. With the [GitHub CLI](https://cli.github.com/) logged in, the script downloads through `gh` instead of `curl` and also verifies the release's build provenance (see below).

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

### Verifying a download

Every release from v0.5.0 on is built by GitHub Actions from a tagged commit, and each archive carries a signed [build provenance attestation](https://docs.github.com/en/actions/security-for-github-actions/using-artifact-attestations). To check that an archive you downloaded was built by this repository's release workflow:

```bash
gh attestation verify oos_0.5.0_darwin_arm64.tar.gz --repo gordonmurray/outofstate.dev
```

The install script does this automatically when the GitHub CLI is logged in.

## Usage

`oos` uses your normal AWS configuration (profiles, SSO, environment variables) to list RDS DB instances, and `terraform state pull` in each Terraform working directory you give it to read state through its configured backend.

```bash
# The current directory is the Terraform working directory
oos --profile default

# Several working directories, for example an app and a shared infrastructure repo
oos --profile default --source ./app-infra --source ./shared-infra

# Save the report for later
oos --profile default --source ./app-infra --source ./shared-infra --output outofstate.txt

# JSON, with full details for scripts and AI coding agents (schema: docs/report.schema.json)
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
| `--cost` | off | Also report RDS spend (slower, see below) |
| `--no-color` | | Disable colour (also `NO_COLOR`) |

### What the report means

| Status | Meaning |
|---|---|
| **Tracked in state** | A managed `aws_db_instance` in one of your states matches this instance. The report shows which state and address. |
| **No match found** | No supplied state tracks it, and every state was read fully. These are **import candidates**: review the configuration and run `terraform plan` before importing. |
| **Unable to verify** | No match, but a state could not be read fully, so the instance might be in it. |
| **Not covered** | Outside what `oos` checks so far: Aurora and Multi-AZ DB cluster members, DocumentDB, Neptune. |

"No match found" means *not in the states you supplied*. The instance may be managed by another state, by CloudFormation (its tags are in the `--format json` output), or by hand.

### Cost (optional)

With `--cost`, the report header also shows RDS spend for the scanned account and regions from AWS Cost Explorer: the last six complete months and the last 30 days, amortised, with a breakdown by usage type (instances, Multi-AZ, storage, backup, proxy, other). It is off by default because the Cost Explorer calls make a run noticeably slower.

```
RDS charges in us-west-2 (amortised, USD): $1,794.24 May–Oct 2026 · $499.05 7 Oct–5 Nov 2026 (estimated)
  last 30 days: instances $227.26 · Multi-AZ $193.03 · storage $56.20 · proxy $21.51 · other $1.05
```

These are **all** RDS charges in those regions, not the cost of the import candidates: AWS does not normally attribute RDS cost to individual instances. Each Cost Explorer request costs US$0.01 and a run with `--cost` makes at least two (more if results are paginated). If cost cannot be read (no permission, Cost Explorer not enabled), the report says why and everything else still works.

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
        "rds:ListTagsForResource",
        "ce:GetCostAndUsage"
      ],
      "Resource": "*"
    }
  ]
}
```

`ce:GetCostAndUsage` is only needed with `--cost`. Reading Terraform state uses your existing backend access (for example read access to the S3 state bucket).

### Secrets

Terraform state contains secrets such as database passwords. `oos` reads only the resource identifiers it needs, never writes state anywhere, and never copies Terraform's own error output into a report. When a source fails, the report says why and gives the command to run yourself to see Terraform's full output.

## Contributing

Bug reports and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md). To report a security problem, see [SECURITY.md](SECURITY.md).

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

`make e2e` runs the full loop against a real AWS sandbox account: it creates three `db.t4g.micro` instances with Terraform, removes one from state, checks `oos` reports exactly that one, imports it back, checks `oos` is clean, and destroys everything it created. It costs a little money, so it needs explicit confirmation and an otherwise empty region.

Cleanup first runs `terraform destroy` on the test projects, which removes only what their own state holds. As a fallback it deletes leftover instances, but only ones that carry both the run's name prefix and its tag, then checks that none remain. If cleanup fails (for example an AWS error), the script exits with code 3 and keeps the Terraform state in `test/e2e/artifacts/<run id>/`.

If the run is killed outright (for example `kill -9`, or the container is stopped), no cleanup runs at all and there may be no error message. After any run that did not print `PASS`, check the region for `oos-e2e-` instances and delete them: they keep costing money until you do.

```bash
AWS_PROFILE=sandbox AWS_REGION=eu-west-1 E2E_CONFIRM=yes make e2e
```

### Releasing

```bash
git tag v0.1.0
git push origin v0.1.0
```

The `Release` workflow tests the tag, builds Linux and macOS binaries with GoReleaser, publishes a GitHub Release with build provenance attestations, then installs the release on Linux and macOS (Apple Silicon and Intel) to check it.
