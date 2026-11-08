---
name: oos
description: Find AWS resources that no Terraform state tracks, using the oos (Out of State) CLI, and help bring chosen ones under Terraform with import blocks. Use when the user asks what is running outside Terraform, which AWS resources are unmanaged, untracked or missing from Terraform, or wants to import existing RDS DB instances, load balancers, S3 buckets, Lambda functions or NAT gateways into Terraform.
---

# Out of State (oos)

`oos` compares the AWS resources in an account with the Terraform states the user supplies and reports the ones no state tracks. It supports RDS DB instances (`rds`), Application and Network Load Balancers (`lb`), S3 general purpose buckets (`s3`), Lambda functions (`lambda`) and NAT gateways (`nat`). It is read-only: it lists resources through the AWS API and reads state with `terraform state pull`.

## Safety rules (always)

- Never run `terraform apply`, `terraform destroy`, `terraform import` (the command) or any `terraform state` command that writes. Never run AWS commands that create, change or delete anything. Only read.
- Only work on the resources the user picked. Do not import everything `oos` lists.
- Never write secrets into files or output. Database passwords, keys and tokens belong in variables or a secrets manager. If generated configuration contains one, replace it before showing the result.
- Do not paste account IDs, ARNs or bucket names into anything shared outside the user's machine (issues, chats, commits to public repositories) unless the user asks.
- Stop and ask the user before each step that changes their files.

## 1. Make sure oos is installed

```bash
oos --version
```

If it is missing, offer to install it (Linux and macOS):

```bash
curl -fsSL https://outofstate.dev/install.sh | bash
```

It installs into `~/.local/bin`, which must be on `PATH`.

## 2. Run a scan

Ask the user (or infer from the repository) for:

- the AWS profile (`--profile`) and region(s) (`--region`, repeatable);
- every Terraform working directory that manages resources in that account (`--source`, repeatable). Each must already be initialised (`terraform init` run there). Supplying all the relevant projects matters: anything in a project that is left out will look untracked.
- the workspace: each directory is read in its currently selected workspace only. If a project uses several workspaces (for example `staging` and `prod`), ask which apply to this account and select each in turn (`TF_WORKSPACE=prod oos …`) or run `oos` once per workspace. Otherwise resources in the other workspaces look untracked.

Then run, writing JSON for you to read:

```bash
oos --profile <profile> --region <region> \
  --source <dir-1> --source <dir-2> \
  --format json --output oos-report.json
```

Useful flags: `--type rds|lb|s3|lambda|nat` (repeatable; default all), `--state-file <file>` for a state JSON file, `--tf-binary tofu` for OpenTofu, `--cost` for RDS spend (slower, costs a little).

Exit codes: `0` everything in scope is tracked; `1` error, no report (read stderr); `2` complete scan with untracked resources; `3` partial scan (a state source or region could not be read fully). Exit codes 2 and 3 are normal results, not failures.

## 3. Read the report correctly

The JSON follows `docs/report.schema.json` in the oos repository. Check `scan.complete` first. If it is false, look at `sources[]` and `regions[]` and fix those before trusting the findings. A failed source has an `error` and, for working directories, usually a `hint` command to run by hand to see Terraform's own message. A `partial` source lists the state entries it could not read in `problems[]`.

Also check the top-level `warnings[]`: `duplicate_snapshot` (the same state was supplied twice, counted once) and `diverged_copies` (several versions of one state were supplied; only the newest, listed first, was used).

Each entry in `findings[]` has a `resource_type` (`aws_db_instance`, `aws_lb`, `aws_s3_bucket`, `aws_lambda_function`, `aws_nat_gateway`), `kind`, `identifier`, `arn`, `region`, `import_id` and a `status`:

| status | Meaning | What to do |
|---|---|---|
| `tracked` | A supplied state manages it. `matches[]` says which state and address. | Nothing. If `matches[].weak` is true, mention that the match used weaker evidence. |
| `no_match` | Not in any **supplied** state. | An import candidate, not proof that nobody manages it. It may be in a state that was not supplied, or managed by another tool. |
| `unable_to_verify` | No match, but a state could not be read fully. | Fix the failing source first, then rescan. |
| `not_covered` | Outside what oos checks (for example Aurora cluster members, Gateway Load Balancers). | Mention it only if asked. |

Warnings on a finding (`warnings[].kind`):

- `managed_by_other_tool` (with `tool`: `cloudformation`, `elastic_beanstalk`, `kubernetes`): its tags say another tool manages it. Do not import it into Terraform without the user confirming. Importing would put two tools in charge of one resource.
- `likely_created_by` (S3 and Lambda): the name follows a tool's convention, for example a CDK bootstrap bucket or an AWS Control Tower function. Usually leave it alone.
- `stale_state_entry`: a state has an entry with the same name but for a different (older) resource. The real resource was probably recreated outside Terraform; that state entry needs attention too.
- `multiple_owners` or `multiple_addresses`: more than one state or address claims it. Point this out; it is a problem in itself.
- `tags_unavailable`: tags could not be read, so other-tool hints may be missing.

Summarise for the user: how many resources are untracked, grouped by type, with the warnings above called out. Then ask which ones they want to bring under Terraform, and into which project.

## 4. Bring a chosen resource under Terraform

Work in the Terraform project the user chose, on a new branch if it is a git repository.

1. **Write an import block** using the finding's `import_id` and a sensible resource address that fits the project's naming:

   ```hcl
   import {
     to = aws_db_instance.reports
     id = "<import_id>"
   }
   ```

   Import IDs: `aws_db_instance` takes the DB identifier, `aws_lb` takes the load balancer ARN, `aws_s3_bucket` takes the bucket name, `aws_lambda_function` takes the function name, `aws_nat_gateway` takes the gateway ID. `oos` already puts the right one in `import_id`.

2. **Generate a starting configuration** (Terraform 1.5 or later). This changes nothing in AWS or in state, but it does write the named file, so tell the user first:

   ```bash
   terraform plan -generate-config-out=generated_<name>.tf
   ```

   Then tidy the generated file: move it into the project's structure, use the project's variables, modules and naming, and remove arguments that only restate defaults.

3. **Handle secrets, and do not change them.** The goal is an import that changes nothing. For `aws_db_instance` the master password is the trap:
   - Find out how the **real instance** manages its password (read-only): `aws rds describe-db-instances --db-instance-identifier <id> --query 'DBInstances[0].MasterUserSecret'`. If that returns a secret, the instance uses Secrets Manager: keep `manage_master_user_password = true`. If it returns nothing, the instance uses a classic password: do **not** set `manage_master_user_password` (turning it on makes AWS generate a new password) and leave `password` out of the configuration entirely.
   - Do not add `password = var.…` during the import either. Imported state never contains the password, so Terraform would plan to set it, which is a change, even if the value is the same.
   - If an import-only plan is impossible without deciding how the password is managed, stop and explain the choice to the user. Do not make it for them.
   - Never write a password or other secret into a file.

4. **Check the plan** until it shows the import and no unexpected changes:

   ```bash
   terraform plan
   ```

   A clean result says the resource will be imported with 0 to add, 0 to change and 0 to destroy. If it wants to change or replace the resource, adjust the configuration to match reality. Do not accept a plan that changes, replaces or destroys anything unless the user explicitly asks for that change. Before showing the plan to the user, check it for secrets and leave them out; it is fine for them to see their own identifiers locally.

5. **Stop.** The user runs `terraform apply` themselves after reviewing the plan.

6. **Afterwards**, once the user has applied, rerun the same `oos` command. The resource should now show as `tracked`.

## Notes by resource type

- **RDS (`aws_db_instance`)**: parameter groups, option groups, subnet groups and security groups the instance uses may also be outside Terraform. Mention them; do not import them unless asked.
- **Load balancers (`aws_lb`)**: listeners, rules and target groups are separate Terraform resources (`aws_lb_listener`, `aws_lb_target_group`, ...) and oos does not check them yet. Importing the load balancer does not import them. They may already be tracked in another state, or not at all; tell the user and check before importing them.
- **S3 (`aws_s3_bucket`)**: bucket settings such as versioning, encryption, lifecycle and policy are separate resources in current AWS provider versions (`aws_s3_bucket_versioning`, ...). The generated configuration may need splitting into them.
- **Lambda (`aws_lambda_function`)**: the function's code is not part of the import, so the configuration needs a code source (`filename`, `s3_bucket` and `s3_key`, or `image_uri`) and the plan may still show a change to it. Generated configuration can contain environment variables, which can hold secrets: check it before showing or committing it. Aliases, permissions and event source mappings are separate resources that `oos` does not check.
- **NAT gateways (`aws_nat_gateway`)**: the Elastic IP (`aws_eip`) and the routes that use the gateway are separate resources that `oos` does not check. Changing the subnet or Elastic IP in the configuration replaces the gateway and interrupts outbound traffic, so make sure the plan does not. `matches[].weak` means the state was written by an older provider version and does not record the account and region.
