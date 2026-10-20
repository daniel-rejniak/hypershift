# Draft implementation plan: GAR pull-secret lifecycle

## Goal and scope

After deploying the POC configuration, generate the GAR login automatically,
deliver it to control planes and workers, and replace it without interrupting
image downloads. Test the complete replacement process before enabling automatic
removal of old keys.

Keep GAR in the current management project for this work. Use the tested
service-account JSON-key login, Secret Manager, and External Secrets Operator
(ESO). The proposal assumes service-account key creation is permitted and one
shared download account per region. Rotation periods remain configurable.

This is a draft plan. The jobs and rotation behavior described below have not
been implemented or validated.

For the proposed API calls, version selection, verification process, and saved
rotation state, see [how it would work under the hood](PULL-SECRET-LIFECYCLE-DESIGN.md).

## What can be reused

- [setup.sh](../scripts/setup.sh) already creates the download account and login,
  uploads the login to Secret Manager, and reuses an existing enabled version.
  It does not rotate existing credentials.
- [ESO definitions](../examples/external-secret.yaml) already merge the GAR entry
  with existing registry credentials for the operator and HostedCluster.
- HyperShift copies those credentials into the control-plane namespace and the
  hosted cluster. Its existing global pull-secret syncer can update eligible
  workers, but GCP workers are not enabled by default.

## 1. Automate initial credential creation

- Keep account creation, repository permissions, and the job's Workload Identity
  configuration in deployment setup. Give the download account read access to
  the virtual GAR repository.
- Separate credential generation from infrastructure setup. Have an initial
  Kubernetes Job run the generation code using Workload Identity, without a
  separate stored key for the job itself.
- Give the job permission to manage keys only for the download account and
  read/write credential versions only in the designated Secret Manager secret.
  ESO keeps read-only access to that secret.
- Reuse the current credential when present. Otherwise, create a key, build the
  Docker login, verify GAR access with that key, and upload the document.
- Keep private files temporary and exclude credential values from logs. Ensure
  the job can start using existing GKE image access before the login exists.

**Complete when:** a deployment starting without a GAR login produces one, and
repeating the deployment does not create another key or secret version.

## 2. Automate delivery through ESO

- Deploy the existing merge definitions and their references through the POC's
  deployment configuration rather than requiring separate manual patches.
- Keep the name `pull-secret` and preserve the existing Quay, Red Hat, and other
  registry entries. Keep one ExternalSecret responsible for each final Secret.
- Use a short refresh interval during testing, such as one minute, on both the
  intermediate merge and the final ExternalSecret. A refresh must not require
  a manual annotation.
- Read the GAR login through a Secret Manager version alias named `current` so
  uploading a candidate does not automatically activate it. Set the alias before
  changing ESO to read it, and verify support in the installed ESO version.
- Wait for the final operator and HostedCluster Secrets before enabling GAR
  image addresses or provisioning workers.

**Complete when:** a new Secret Manager version reaches both final pull secrets
and the control-plane and hosted-cluster copies automatically. Compare only
version identifiers or credential fingerprints in test output.

## 3. Prove existing workers and future workers receive replacements

- On explicitly selected GCP Replace NodePools, test the existing global
  pull-secret syncer by opting POC workers into its scheduling label. Confirm
  refreshed credentials reach `/var/lib/kubelet/config.json` and survive reboot.
  This is a POC experiment, not established GCP support; exclude InPlace pools.
- Check label propagation for both existing and newly created workers. If the
  existing path is insufficient, isolate the required GCP change for review.
- Check worker bootstrap after changing Secret contents while retaining the
  same `spec.pullSecret.name`. Existing bootstrap tokens record a pull-secret
  hash when created, and the ignition server checks it when generating data.
- Test scale-out of an existing NodePool and regeneration after restarting the
  ignition server. Resolve stale hashes and cached credentials before treating
  rotation as complete; creating a brand-new NodePool alone is insufficient.
- Check whether a proposed fix affects NodePool config hashes or causes worker
  replacement. Any hash-affecting implementation must meet the repository's
  fleet rollout review requirements and pass
  `e2e-aws-upgrade-hypershift-operator`.

**Complete when:** existing workers receive the new login and a new worker in an
existing NodePool starts with it, including after ignition cache regeneration.
See the [global pull-secret behavior](../../docs/content/how-to/common/global-pull-secret.md).

## 4. Add scheduled rotation and recovery

Use the same credential code for the initial Job and a CronJob. The CronJob can
check daily and rotate at a configured age; 30 days is a proposed starting value
for discussion. Tests can request a replacement immediately without changing
the normal schedule.

The replacement sequence is:

1. Create one replacement key and confirm it can read a required GAR image.
2. Store its Docker login as a new Secret Manager version, then point `current`
   at that version, keeping the old key enabled. Record the previous and
   replacement key/version identifiers.
3. Wait for the replacement to reach all configured consumers, including worker
   credentials and bootstrap data. Readiness alone is insufficient: verify the
   expected credential version and fresh registry access.
4. Disable the old key, then verify fresh downloads and worker bootstrap again.
5. After a configured observation period, delete the old key and retire its old
   secret versions.

Prevent overlapping runs and resume an unfinished replacement after a job
restart. Do not create another replacement on every retry. Store progress and
consumer acknowledgments without private key material. Remove newly created
keys if their publication fails, and report failures clearly.

If a consumer has not updated, leave the old key enabled and report which
consumer is outstanding. If checks fail after disabling it, re-enable the old
key and restore its login as the current secret content. Keep the old secret
version available until recovery is no longer needed. Removing the job must
not delete credentials still in use.

This follows Google's [create, distribute, disable, then delete sequence](https://docs.cloud.google.com/iam/docs/key-rotation).

**Complete when:** a forced replacement succeeds through old-key retirement,
and failed or interrupted runs recover without losing usable credentials.

## 5. Validate the complete POC

| Test | Required result |
| --- | --- |
| First deployment | Login generated and delivered without manually copying credentials. |
| Repeat deployment | Existing working login reused; no extra keys created. |
| Forced replacement | ESO and all selected consumers receive the replacement automatically. |
| Existing workers | Updated credentials used for fresh downloads without replacing the workers. |
| Existing NodePool scale-out | New worker boots after rotation and after ignition server restart. |
| Old key disabled | Fresh downloads and bootstrap still work; cached images do not count as proof. |
| Upload, ESO, or worker update failure | No premature old-key removal; progress identifies the failure. |
| Job restart or concurrent trigger | Replacement resumes without duplicate keys or conflicting updates. |
| Rollback | Previous key and login restored successfully before deletion. |

Use a separate test account and secret for destructive failure cases, and keep
direct public-registry access blocked for the image-download tests. If a later
rotation is still waiting for consumers, stop creating keys and report the delay.

## Where the implementation belongs

| Location | Proposed work |
| --- | --- |
| POC `scripts/` | Separate generation/rotation code from infrastructure setup; add verification and recovery helpers. |
| POC `examples/` | Initial Job, CronJob, Workload Identity/RBAC definitions, and ESO configuration. |
| HyperShift worker/bootstrap code, if required | Isolated changes for GCP worker updates and bootstrap consistency, with targeted tests. |
| `gcp-hcp-infra`, later | Deploy regional account, IAM, job, secret, and operator ExternalSecret configuration. |
| Gecko, later | Generate each HostedCluster's ExternalSecret from its region's credential settings. |

## When GAR becomes regional

Run one credential job for the region. Store its login in regional Secret Manager
and grant each management cluster's ESO identity read access. Each management
cluster merges the same regional login into its operator and HostedCluster pull
secrets; a new cluster receives the current login without generating another key.

Make project IDs, the download account, GAR hostname, secret name, and consumer
list configuration inputs now. The POC can retain its current management-project
GAR and customer-project credential; future deployments supply regional values.

Maintain a list of participating clusters and their reported credential versions.
Retire an old shared key only when all required consumers have updated. An
unreachable cluster must be visible as an outstanding consumer, not silently
ignored. The process needs this verification before regional automatic key
removal can be enabled.

Deleting one HostedCluster removes it from the consumer list after its resources
are gone; it does not revoke a credential shared by other clusters. Remove the
regional credential only after the region has no remaining consumers.
