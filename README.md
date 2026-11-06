# Out of State (`oos`)

Find what's running outside your Terraform.

`oos` compares the resources in an AWS account with the Terraform states you supply and reports the ones that no state tracks. Supported so far: RDS DB instances, Application/Network Load Balancers and S3 buckets. More will follow.

`oos` reads Terraform state format version 4 (Terraform 0.12 and later, and OpenTofu).

```
$ oos --profile default --region eu-west-1 --source ./app-infra --source ./shared-infra
Out of State · account 123456789012 · eu-west-1 · 2026-11-06 10:42 UTC
Scan: complete
  source  ./app-infra (default) ✓ 10:42:00
  source  ./shared-infra (prod) ✓ 10:42:01

2 import candidate(s): resources with no match in the supplied states

  analytics-db             eu-west-1       rds   db.r6g.large     postgres 16.2            available    created 2025-03-02
  legacy-alb               eu-west-1       alb   internal         vpc-0a1b2c3d             active       created 2024-01-01

3 tracked in state
  orders-db                eu-west-1       rds   ./app-infra (default) aws_db_instance.orders (resource_id)
  reports-db               eu-west-1       rds   ./shared-infra (prod) aws_db_instance.reports (resource_id)
  web-alb                  eu-west-1       alb   ./app-infra (default) aws_lb.web (arn)

1 not covered
  billing-cluster-1        eu-west-1       rds   member of an Aurora or Multi-AZ DB cluster

Import candidates need a configuration review and a terraform plan before applying.
Use --format json for full details (ARNs, resource and import IDs, tags).
```

A database and a load balancer have no match in either state and need a look, three resources are tracked (with the state and address that tracks them), and one is outside what `oos` checks so far.

> **Status:** early development. RDS DB instances, load balancers and S3 buckets.

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
gh attestation verify oos_0.5.0_darwin_arm64.tar.gz \
  --repo gordonmurray/outofstate.dev \
  --signer-workflow gordonmurray/outofstate.dev/.github/workflows/release.yml \
  --source-ref refs/tags/v0.5.0
```

The install script does this automatically when the GitHub CLI is logged in and recent enough (2.49 or later; older versions get a note, and the checksum is still verified).

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
| `--type` | all | Resource type to check, repeatable: `rds`, `lb`, `s3` |
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
| **Not covered** | Outside what `oos` checks so far: instances that belong to Aurora or Multi-AZ DB clusters, DocumentDB, Neptune, and Gateway Load Balancers. Classic Load Balancers and S3 directory and table buckets are not checked at all yet. |

If you supply several snapshots of the same state (for example a working directory and an older exported copy), only the newest is used, so an old copy cannot make something look tracked. The report says which one it used.

"No match found" means *not in the states you supplied*. The resource may be managed by another state, by another tool, or by hand. When its tags show that CloudFormation, Elastic Beanstalk or Kubernetes (the AWS Load Balancer Controller) manages it, the report says so. When an S3 bucket follows a naming convention those tools use (for example a CDK bootstrap bucket), the report says it was likely created by that tool.

### Supported resource types

| Type | AWS | Terraform | Matched on |
|---|---|---|---|
| `rds` | RDS DB instances | `aws_db_instance` | resource ID (`db-…`), falling back to ARN or name only when the state has no resource ID |
| `lb` | Application and Network Load Balancers | `aws_lb` (and its older name `aws_alb`) | load balancer ARN (a recreated load balancer gets a new one) |
| `s3` | S3 general purpose buckets owned by the account, in the scanned regions | `aws_s3_bucket` (bucket policies, versioning and other sub-resources do not count) | bucket name, plus the region when the state records it. Buckets have no immutable ID, so a bucket deleted and recreated with the same name in the same region looks the same |

S3 reads the tags of every bucket (one request per bucket) so it can recognise buckets created by other tools. With many buckets this adds a second or two; `--type rds --type lb` skips S3.

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
        "elasticloadbalancing:DescribeLoadBalancers",
        "elasticloadbalancing:DescribeTags",
        "s3:ListAllMyBuckets",
        "s3:GetBucketTagging",
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

## Using oos with an AI coding agent

The repository includes an [agent skill](skills/oos/SKILL.md) that teaches a coding agent how to run `oos`, read its report correctly, and help bring resources you choose under Terraform: it writes `import` blocks, drafts configuration with `terraform plan -generate-config-out`, and stops at a reviewed plan. It never runs `terraform apply`.

In Claude Code, install it as a plugin:

```
/plugin marketplace add gordonmurray/outofstate.dev
/plugin install oos@outofstate
```

Or copy the skill into your personal or project skills directory:

```bash
mkdir -p ~/.claude/skills/oos
curl -fsSL https://raw.githubusercontent.com/gordonmurray/outofstate.dev/main/skills/oos/SKILL.md -o ~/.claude/skills/oos/SKILL.md
```

Then ask something like "what's running in this AWS account outside our Terraform?". The skill uses the Agent Skills format, so other agents that support it can use the same file.

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
