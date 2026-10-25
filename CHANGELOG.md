# Changelog

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/);
versions follow [Semantic Versioning](https://semver.org/) (0.x: a minor
release may change behaviour). The version is stamped from the git tag at
build time.

## [Unreleased]

### Breaking changes

- `nemau_kubernetes_cluster` requires `network_id` and `subnet_id`: the UUIDs
  of the project network and of the subnet the nodes live in. A configuration
  without them fails at plan time. Changing either one replaces the cluster.
  Every read takes `network_id`, `subnet_id`, `pod_cidr` and `service_cidr` from
  the cluster, so a state that lacks them is filled in on the next refresh, and
  a configuration that names other values plans a replacement.

### Added

- `nemau_instance.user_data`, a cloud-init document run at the first boot. It
  is sensitive, and a change forces a replacement. The state keeps the
  configured value; a document that the platform reports under another
  SHA-256 is dropped from the state, so the plan replaces the instance.
- `nemau_instance` gets a `backup_policy` block: `schedule` (`daily` or
  `weekly`), `hour` (UTC, default 2), `weekday` (0 Sunday to 6 Saturday, default
  0) and `retention_days` (1 to 35, required). The block changes in place and
  sends the whole policy. Removing it turns scheduled backups off. It is absent
  from the state when the platform has no schedule, so an instance without a
  policy shows no diff.
- `nemau_snapshot` gets the computed `origin` (`manual` or `scheduled`) and
  `expires_at`, which is null for a manual snapshot.
- `nemau_database`: a managed PostgreSQL database. The administrator password
  is exported as the sensitive `admin_password`, once, at creation and after
  each change of `password_rotation_trigger`. It is kept in the state, and an
  import cannot recover it. `restore_from` creates a point-in-time restore as a
  new database. Storage can only grow, and `security_group_ids` is set at
  creation only.
- `nemau_instance_group`: an instance group with its launch template
  (`user_data` is sensitive and never read back), load balancer pools,
  target-tracking `scaling_policy` blocks and `scheduled_action` blocks.
  `desired_capacity` is optional and computed: unset, the autoscaler owns it.
- `data.nemau_database` and `data.nemau_instance_group`, read by `id` or by
  `name`.
- `data.nemau_database_flavors` and `data.nemau_database_versions`: the
  flavors (name, vCPUs, memory in MB) and PostgreSQL versions a region offers.
  The provider does not check `flavor` against them at plan time; the platform
  refuses an unlisted value at apply.
- `nemau_instance_group.refresh_on_template_change` (bool, default `false`):
  start a rolling refresh when the launch template changes. Without it, the new
  template version is stored and the members move only when a refresh runs.
- `nemau_kubernetes_cluster` takes the optional `pod_cidr` and `service_cidr`,
  IPv4 CIDRs unique to the cluster. When omitted, the platform chooses them.
  Changing either one replaces the cluster. Reads take them from the cluster.
- `nemau_kubernetes_cluster` `default_node_pool` takes `labels` (`key=value`)
  and `taints` (`key[=value]:Effect`), at most 16 of each. Changing them
  replaces the cluster.
- Plan-time validation of the new and existing Kubernetes attributes: UUIDs,
  IPv4 CIDRs, node pool labels, taints and count limits, and the node class,
  which is `default-worker`. The `node_class` of `nemau_kubernetes_node_pool`
  is checked the same way.
- `nemau_kubernetes_node_pool` takes `labels` and `taints`, with the same rules
  as `default_node_pool`. Changing them replaces the pool: the API has no
  operation to change them in place.
- The plan refuses a shorter `retention_days` on `nemau_container`, and
  `versioning = false` together with `object_lock = true`, instead of failing
  at apply.
- Documentation in the Terraform Registry layout under `docs/`: a page for
  every resource and data source, with an example and a description of every
  attribute, and guides for the topics the README used to cover. The pages are
  generated from the schema with tfplugindocs.

### Changed

- `nemau_database`: an omitted `maintenance_window` means no window. Removing
  the block clears the window on the next apply, and a window the platform sets
  at creation is cleared when the configuration omits the block. Before, an
  omitted block left the window as it was.
- The registry address is `nemau-cloud/nemau`, after the GitHub organization
  the Terraform Registry publishes from. Configurations use
  `source = "nemau-cloud/nemau"`.

### Fixed

- A `nemau_kubernetes_cluster` whose `network_id`, `subnet_id`, `pod_cidr` or
  `service_cidr` is unknown in the plan now plans a replacement. Before, the
  unknown value was taken as unchanged.
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

### Upgrade notes

- A `nemau_instance_group` imported with v0.1.0 has no import marker in its
  state. After upgrading, a configured `service_level` on such a group rolls
  the launch template once, on the first apply. Expect that one template
  update in the plan.

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
