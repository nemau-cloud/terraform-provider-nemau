# Changelog

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions follow [Semantic Versioning](https://semver.org/) (0.x: a minor
release may change behaviour). The version is stamped from the git tag at
build time.

## [Unreleased]

### Changed

- The registry address is `nemau-cloud/nemau`, after the GitHub organization
  the Terraform Registry publishes from. Configurations use
  `source = "nemau-cloud/nemau"`.

### Fixed

- Renaming a `nemau_instance` or changing its `description` now reaches the
  platform. The update was never sent, so the apply failed with an
  inconsistent result while the instance kept its old name.
- `nemau_container` updates now reach the platform. Changing `versioning`
  or raising `retention_days` sends the update and waits for its task; before,
  the new values were written to the state without any API call. The state is
  rebuilt from what the object store reports, and an update the store does not
  reflect fails the apply.
- Changing `name` on `nemau_container` now plans a replacement: the API
  cannot rename a container.
- An update of `nemau_container` that omits `object_lock` or `retention_mode`
  no longer plans a replacement of the container: both keep their state when
  omitted, as do `versioning` and `retention_days`.

### Added

- The plan refuses a shorter `retention_days` on `nemau_container`, and
  `versioning = false` together with `object_lock = true`, instead of failing
  at apply.
- Documentation in the Terraform Registry layout under `docs/`: a page for
  every resource and data source, with an example and a description of every
  attribute, and guides for the topics the README used to cover. The pages are
  generated from the schema with tfplugindocs.

## [0.5.0]

Built on `nemau-sdk-go` v0.5.0, which this release is versioned with.

### Added

- Every write carries an `Idempotency-Key`, generated per operation and reused
  when the provider retries it, so a retried apply step never creates a
  resource twice. A key mismatch is explained in the diagnostic.

### Changed

- Changing `kubernetes_version` on `nemau_kubernetes_cluster` upgrades the
  cluster in place through the guided upgrade task (preflight, maintenance
  window, rolling upgrade) instead of failing.
- A `429` or `503` is retried with the `Retry-After` it names (up to 60 s; a
  longer wait fails the operation with the platform's message). Writes are
  limited to 60 per minute per credential; the retries absorb a large plan.
- The synchronous writes (`nemau_keypair`, `nemau_load_balancer_certificate`,
  `nemau_container_credential`) carry no key and are not retried after a `429`
  or `5xx`: run `terraform apply` again.
- Built with Go 1.27.2 for the standard library security fixes
  (GO-2026-6603 to GO-2026-6617).

### Notes

- Access keys expire: 90 days by default, 365 at most. After the expiry the API
  answers `401`; rotate the key before then.
