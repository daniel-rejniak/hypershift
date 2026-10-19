# GAR pull-secret POC reference

This directory collects the setup commands, ESO definitions, and image patches
used to validate GAR downloads for HyperShift control planes and workers. It uses
the standard operator and normal pull-secret authentication.

Start with the [proposal and manual examples](../../GAR-POC-OVERVIEW.md) for the
architecture, credential flow, exact YAML fields, and proposed implementation PRs.

The reference is based on HyperShift revision
`3659d94f893751bcb0dd48cd3ad0f026e4fb7095`. The tested operator image version was
`bc8675b2441e1f1d7f07d68687945f2de85e7526`. This branch adds examples and documentation;
it does not require building an operator image.

## Files

| File | Purpose |
| --- | --- |
| [setup.sh](setup.sh) | Create or reuse the GAR download account and store its Docker login in customer-project Secret Manager. |
| [setup-repositories.py](setup-repositories.py) | Add the Red Hat remote repository and shared virtual repository beside the existing Quay cache; configure upstream credentials and access. |
| [gar-upstream-policies.json](gar-upstream-policies.json) | List the virtual repository's two upstream repositories and their priorities. |
| [external-secret.yaml](external-secret.yaml) | Create the GAR SecretStore and combined pull secrets for the operator and HostedCluster. |
| [managed-pull-secret-patch.yaml](managed-pull-secret-patch.yaml) | Make the existing ExternalSecrets import the combined credentials while keeping the `pull-secret` name. |
| [operator-mirror-patch.yaml](operator-mirror-patch.yaml) | Set the operator and startup images plus control-plane registry mappings. |
| [external-dns-mirror-patch.yaml](external-dns-mirror-patch.yaml) | Set the DNS updater image to GAR. |
| [catalog-mirror-patch.yaml](catalog-mirror-patch.yaml) | Set the HostedCluster's catalog registry mapping. |
| [catalog-credentials-patch.yaml](catalog-credentials-patch.yaml) | Record the selective Red Hat credential merge, already included in `external-secret.yaml`. |
| [Worker mirror patch](../gcp-gar-mirror-patch.yaml) | Record the worker mapping tested with the Quay remote repository. The proposal shows the equivalent shared virtual address. |
| [Alternative pull-secret reference patch](../gcp-gar-pull-secret-patch.yaml) | Earlier example for clusters whose pull-secret reference is managed manually. For Gecko-managed clusters, use the ESO overlay above. |

## Environment and prerequisites

The files contain the actual POC's resource names:

- Management project: `dev-mgt-us-c1-drejniak`; region: `us-central1`.
- Customer project: `drejniak-hcp-dev`.
- HostedCluster: `drejniak`, namespace
  `clusters-795f77ca-a68b-4b5c-af76-e30749b30a67`.
- Base pull secrets: `default-openshift-pull-secret-ci` and
  `default-openshift-pull-secret` in `gcp-hcp-commons-dev`, plus the operator's
  existing source secret.

Use an existing dev all-in-one environment with a HostedCluster, ESO, its base
SecretStores, and an authenticated Quay remote repository named `quay-pullthrough`.
That repository is supplied by the infrastructure's `gar-pull-through-cache`
Terraform module. `setup-repositories.py` adds Red Hat and the virtual repository;
it expects the Quay repository to exist.

Install `gcloud`, `kubectl`, `jq`, and Python 3. Authenticate to Google Cloud and
the management cluster with permissions to create service-account keys, manage
repository access, and read the source pull secrets. For another environment,
adapt the resource names in the scripts, manifests, and upstream policy together;
`setup-repositories.py` also contains the management project number.

## Reproduce the configuration

Run from the repository root. The commands below change the named development
environment when executed.

```sh
POC=examples/gcp-gar-pull-secret-poc
CTX=gke_dev-mgt-us-c1-drejniak_us-central1_dev-mgt-us-c1-drejniak-gke
HCNS=clusters-795f77ca-a68b-4b5c-af76-e30749b30a67
HCNAME=drejniak

# Create/reuse the GAR login, then configure the repositories.
bash "$POC/setup.sh"
python3 "$POC/setup-repositories.py"

# Deliver the combined credentials into both namespaces.
kubectl --context="$CTX" apply -f "$POC/external-secret.yaml"
kubectl --context="$CTX" -n "$HCNS" wait externalsecret/gcp-gar-pull-secret \
  --for=condition=Ready --timeout=120s
kubectl --context="$CTX" -n hypershift wait externalsecret/gcp-gar-pull-secret \
  --for=condition=Ready --timeout=120s

kubectl --context="$CTX" -n "$HCNS" patch externalsecret pull-secret \
  --type=merge --patch-file="$POC/managed-pull-secret-patch.yaml"
kubectl --context="$CTX" -n hypershift patch externalsecret pull-secret \
  --type=merge --patch-file="$POC/managed-pull-secret-patch.yaml"

kubectl --context="$CTX" -n "$HCNS" annotate externalsecret pull-secret \
  force-sync="$(date +%s)" --overwrite
kubectl --context="$CTX" -n hypershift annotate externalsecret pull-secret \
  force-sync="$(date +%s)" --overwrite
```

Verify the resulting `pull-secret` contains the GAR entry in both namespaces
before changing images. This check prints only `true` or `false`:

```sh
for namespace in "$HCNS" hypershift; do
  kubectl --context="$CTX" -n "$namespace" get secret pull-secret -o json \
    | jq -e '.data[".dockerconfigjson"] | @base64d | fromjson
             | (.auths["us-central1-docker.pkg.dev"].auth // "") | length > 0'
done
```

The POC temporarily suspended reconciliation of the `hypershift` Argo Application
so deployment patches persisted. For permanent use, put the settings into the
infrastructure files identified in the proposal. For the manual POC:

```sh
kubectl --context="$CTX" -n argocd annotate application hypershift \
  argocd.argoproj.io/skip-reconcile=true --overwrite

kubectl --context="$CTX" -n hypershift patch deployment operator \
  --type=strategic --patch-file="$POC/operator-mirror-patch.yaml"
kubectl --context="$CTX" -n hypershift patch deployment external-dns \
  --type=strategic --patch-file="$POC/external-dns-mirror-patch.yaml"
kubectl --context="$CTX" -n "$HCNS" patch hostedcluster "$HCNAME" \
  --type=merge --patch-file="$POC/catalog-mirror-patch.yaml"
```

For workers, apply the tested mirror mapping before creating NodePools:

```sh
kubectl --context="$CTX" -n "$HCNS" patch hostedcluster "$HCNAME" \
  --type=merge --patch-file=examples/gcp-gar-mirror-patch.yaml
```

On an existing HostedCluster, changing `imageContentSources` rolls every NodePool.
Preserve any other entries: a merge patch replaces the list. Keep
`spec.pullSecret.name` set to `pull-secret` so Gecko and ESO maintain the same
resource. Image mappings and credential delivery do not configure network access.

## Recorded validation

Validation on 19 October 2026 established:

- Workers downloaded release images through GAR with customer NAT removed.
- ESO supplied combined credentials to the operator and HostedCluster namespaces.
- The virtual repository served authenticated Quay and Red Hat image requests.
- All 37 control-plane workload templates and 173 image references in 58 active
  pods used the virtual repository. Both API deployments reached 3/3 Ready, all
  three tested catalogs were ready, and the HostedCluster reported Available
  and not Degraded.
- The operator and DNS updater retained their original image versions.

Credential replacement must reach existing GCP workers as well as Kubernetes
Secrets and future worker setup data before revoking an old key. Existing
bootstrap tokens also capture a pull-secret hash, so changing credentials requires
checking bootstrap recovery. The [credential-update guide](../../docs/content/how-to/common/global-pull-secret.md)
explains the platform-specific behavior.

When returning to Git-managed deployments, save the desired image settings in
Git before removing the Argo `skip-reconcile` annotation. Retain usable credentials
until every consumer has its replacement.
