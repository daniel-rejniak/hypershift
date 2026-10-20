# Zero Egress POC

**Decision overview · 19 October 2026**

## Contents

1. [Overview](#overview)
2. [Create the regional GAR repositories](#create-the-regional-gar-repositories)

    - [Why two repositories and one shared address?](#why-two-repositories-and-one-shared-address)
    - [Repository setup sequence](#repository-setup-sequence)

3. [Create and store the GAR pull secret](#create-and-store-the-gar-pull-secret)

    - [Authentication options and POC choice](#authentication-options-and-poc-choice)
    - [Manual commands](#manual-commands)

4. [Inject the pull secret into the clusters](#inject-the-pull-secret-into-the-clusters)

    - [Add it to the existing pull secret](#add-it-to-the-existing-pull-secret)
    - [How the cluster picked it up](#how-the-cluster-picked-it-up)

5. [Redirect image downloads to GAR](#redirect-image-downloads-to-gar)

    - [1. Control-plane image mapping: operator argument](#1-control-plane-image-mapping-operator-argument)
    - [2. Operator images: main container and startup container](#2-operator-images-main-container-and-startup-container)
    - [3. DNS updater: its own Deployment image](#3-dns-updater-its-own-deployment-image)
    - [4. Operator catalogs: HostedCluster annotation](#4-operator-catalogs-hostedcluster-annotation)
    - [5. Worker mirrors: HostedCluster imageContentSources](#5-worker-mirrors-hostedcluster-imagecontentsources)
    - [How the pull secret authenticates these downloads](#how-the-pull-secret-authenticates-these-downloads)
    - [What GAR does after authentication](#what-gar-does-after-authentication)

6. [Automate credential creation and rotation](#automate-credential-creation-and-rotation)
7. [Proposed implementation sequence](#proposed-implementation-sequence)

    - [New epic: Regional GAR and pull-secret lifecycle](#new-epic-regional-gar-and-pull-secret-lifecycle)
    - [GCP-1268: Switch control-plane and worker image downloads to GAR](#gcp-1268-switch-control-plane-and-worker-image-downloads-to-gar)
    - [Tests and supporting improvements](#tests-and-supporting-improvements)

8. [Reference files](#reference-files)

## Overview

The Private Preview goal is to run customer clusters without direct internet
egress, supporting customers with strict security requirements. This includes
both the hosted control plane running in the management cluster and worker nodes
running in the customer project.

**Feature requirements:**

- Make all required OpenShift release and operator images available through
  Google Artifact Registry (GAR).
- Configure the control plane, workers, and supporting platform components to
  use GAR during cluster creation, node replacement, and upgrades.
- Automate image availability for new OpenShift releases and operator updates,
  and document the image-mirroring setup.
- Validate cluster and workload operation with direct internet access blocked.
  Workers must have no NAT gateway or external IP. Network tests must confirm
  the restrictions for both the control plane and workers.

The proposed design provides one regional GAR address backed by separate caches
for Quay and Red Hat images. Clusters download from GAR; GAR fetches and caches
images from the source registries. Required Google services and internal cluster
connections must remain reachable.

**Image coverage:** Quay supplies OpenShift release images, platform components,
and the HyperShift operator. Red Hat supplies operator catalogs and the DNS
updater. The evaluated control-plane and management configuration used **34
distinct image addresses**: 30 from Quay and 4 from Red Hat. Workers also download
release images. The full image set depends on OpenShift versions and operators.

**POC results:** Workers and the control plane downloaded images through GAR.
Workers were tested with the customer's NAT gateway removed; control-plane APIs
and all tested catalogs were ready. The setup used existing HyperShift settings,
the standard operator build, and normal pull secrets supplied through Secret
Manager and External Secrets Operator (ESO). The sections below describe the
setup and proposed implementation work.

Customer workload image mirroring, image scanning, and multi-region replication
are outside this feature's scope. This POC documents pull-secret creation and
delivery; lifecycle automation is covered separately as future work.

## Create the regional GAR repositories

### Why two repositories and one shared address?

A GAR remote repository fetches images from **one configured source registry**.
Quay and Red Hat have different addresses and login details, so each gets its own
repository. See [Google's remote repository configuration](https://docs.cloud.google.com/artifact-registry/docs/repositories/remote-repo).

A third, virtual repository provides one address covering both. Clusters use that
address and their GAR login; GAR manages access to the two sources.

```text
Control planes and workers
          |
          v
One regional GAR address (virtual repository)
          +--- Quay cache ------> quay.io
          +--- Red Hat cache --> registry.redhat.io
```

When an image is requested, Google's service downloads it from the source and
keeps a cached copy for later requests. The regional repositories are shared by
clusters in that region. Each cluster connects to GAR; Google's service makes
the requests to Quay or Red Hat. An uncached image still depends on its source
being available. [How GAR caching works](https://docs.cloud.google.com/artifact-registry/docs/repositories/remote-overview).

### Repository setup sequence

1. Create two Docker remote repositories in the chosen region:
   `quay-pullthrough` for `quay.io` and `redhat-pullthrough` for
   `registry.redhat.io`.
2. Store the source-registry passwords in Secret Manager and configure the
   corresponding remote repository to use each login. Allow GAR's service agent
   to read those secrets.
3. Give the service agent read access to both remote repositories.
4. Create the virtual repository `openshift-pullthrough` with both remote
   repositories as its sources.
5. Use its address, `REGION-docker.pkg.dev/REGIONAL_PROJECT/openshift-pullthrough`,
   in the image settings described below.

These repositories and source-registry logins are shared regional infrastructure.
The next section creates the separate GAR login used by clusters.

## Create and store the GAR pull secret

### Authentication options and POC choice

Two approaches were tested for GAR authentication:

| Approach | How it works | POC result and choice |
| --- | --- | --- |
| Short-lived access tokens through a credential helper | Google's `docker-credential-gcr` helper obtains tokens using the worker's Google identity. Ignition installs the helper and configures worker image downloads to use it. | Validated for workers on `v3-mirror-dynamic-creds`. Required a pinned helper image and changes to worker startup and runtime configuration. |
| Service-account JSON key in a pull secret | A Docker login using `_json_key` is stored in Secret Manager. ESO merges it into the existing pull secrets. | **Chosen for the current POC.** Validated for workers and the control plane, with minimal code changes and no additional worker credential helper. |

Google's official [GAR authentication documentation](https://docs.cloud.google.com/artifact-registry/docs/docker/authentication)
describes access tokens, service-account keys, and credential helpers. Access
tokens normally last **60 minutes**; JSON keys are long-lived. Google recommends
access tokens or credential helpers where possible.

The pull-secret approach reused the existing credential delivery and image
authentication paths. This was a POC choice; the production credential method
remains open. The dynamic worker POC used a local helper. Refreshing short-lived
tokens through ESO for both control-plane consumers and workers has not been
validated.

**POC setup:**

The POC started with worker nodes, so the download account and stored login
were created in the customer project. When the POC expanded to the control
plane, management-cluster ESO reused that login for both the operator and
HostedCluster pull secrets. The steps below describe this tested setup.

1. **Create a Google service account** named `gar-pull-secret-poc` in customer
   project `drejniak-hcp-dev`.
2. **Allow image downloads** by granting that account
   `roles/artifactregistry.reader` on the virtual repository
   `openshift-pullthrough` in management project `dev-mgt-us-c1-drejniak`.
3. **Generate a JSON key** for the account using `gcloud`.
4. **Create a Docker login** for `us-central1-docker.pkg.dev`, using `_json_key`
   as the username and the JSON key as the password.
5. **Upload that login to Secret Manager**, under the secret name
   `gar-pull-secret-poc` in the same POC customer project, `drejniak-hcp-dev`.
6. **Allow ESO to read the stored login**, granting its existing
   management-cluster identity `roles/secretmanager.secretAccessor` on that secret.

The next section explains how the POC used ESO to add the login to the
existing operator and HostedCluster pull secrets.

### Manual commands

<details>
<summary>Show the complete POC pull-secret setup</summary>

Run these blocks in order in the same authenticated **Bash** session, with
`gcloud`, `jq`, and `kubectl` installed. The commands use the actual POC names.
The virtual GAR repository, management cluster, ESO, and existing OpenShift pull
secrets and SecretStores must already exist.

The login created here lets clusters download from GAR. The credentials GAR uses
to reach Quay and Red Hat belong to the repository setup in the preceding section.

**1. Set the project names and enable the required APIs.**

```bash
# Stop on errors. Disable command tracing and restrict permissions on new files.
set -euo pipefail
set +x
umask 077

# The POC stored the account and login in the customer project.
credential_project="drejniak-hcp-dev"
account_id="gar-pull-secret-poc"
pull_account="${account_id}@${credential_project}.iam.gserviceaccount.com"
secret_name="gar-pull-secret-poc"

# GAR lived in the management project; downloads used the virtual repository.
gar_project="dev-mgt-us-c1-drejniak"
gar_region="us-central1"
gar_repository="openshift-pullthrough"
gar_host="${gar_region}-docker.pkg.dev"

# ESO uses its existing Kubernetes service account in the management cluster.
management_project="dev-mgt-us-c1-drejniak"
eso_member="serviceAccount:${management_project}.svc.id.goog[external-secrets-system/external-secrets]"
management_context="gke_dev-mgt-us-c1-drejniak_us-central1_dev-mgt-us-c1-drejniak-gke"
hc_namespace="clusters-795f77ca-a68b-4b5c-af76-e30749b30a67"

# Run from any directory inside this repository checkout.
poc_dir="$(git rev-parse --show-toplevel)/gar-pull-secret-poc"

# Enable account/key management and secret storage in the customer project.
gcloud services enable iam.googleapis.com secretmanager.googleapis.com \
  --project="$credential_project" --quiet
```

**2. Create the download account and allow it to read the virtual repository.**

```bash
# Reuse the account if it already exists.
if ! gcloud iam service-accounts describe "$pull_account" \
  --project="$credential_project" --quiet >/dev/null 2>&1; then
  gcloud iam service-accounts create "$account_id" \
    --project="$credential_project" \
    --display-name="Dev GAR pull-secret POC" --quiet
fi

# Cross-project access: this customer-project account can read this GAR
# repository in the management project. The grant is limited to the repository.
gcloud artifacts repositories add-iam-policy-binding "$gar_repository" \
  --project="$gar_project" --location="$gar_region" \
  --member="serviceAccount:$pull_account" \
  --role=roles/artifactregistry.reader --quiet >/dev/null
```

**3. Create the Secret Manager secret and allow ESO to read it.**

```bash
# Create an empty secret to hold the Docker login, or reuse the existing secret.
if ! gcloud secrets describe "$secret_name" \
  --project="$credential_project" --quiet >/dev/null 2>&1; then
  gcloud secrets create "$secret_name" --project="$credential_project" \
    --replication-policy=automatic --labels=purpose=gar-pull-secret-poc --quiet
fi

# This is a separate permission: ESO may read this one stored login.
# ESO authenticates using its existing Workload Identity configuration.
gcloud secrets add-iam-policy-binding "$secret_name" \
  --project="$credential_project" --member="$eso_member" \
  --role=roles/secretmanager.secretAccessor --quiet >/dev/null
```

**4. Generate the Docker login and upload it, or reuse the stored version.**

```bash
# Normal POC setup reused an enabled version instead of creating another key.
existing_version="$(gcloud secrets versions list "$secret_name" \
  --project="$credential_project" --filter='state=ENABLED' --limit=1 \
  --format='value(name)' --quiet)"

if [[ -n "$existing_version" ]]; then
  printf 'Reusing the stored login in %s/%s.\n' "$credential_project" "$secret_name"
else
  # Keep private files in a temporary directory with restricted permissions.
  work_dir="$(mktemp -d "${TMPDIR:-/tmp}/gar-pull-secret-poc.XXXXXXXX")"
  published=false

  # Always remove the local files. If upload fails, also remove the newly
  # generated Google key so it is not left active without a stored login.
  cleanup_gar_key_files() {
    if [[ "$published" != true && -s "$work_dir/key.json" ]]; then
      key_id="$(jq -er .private_key_id "$work_dir/key.json")"
      gcloud iam service-accounts keys delete "$key_id" \
        --iam-account="$pull_account" --project="$credential_project" --quiet || \
        printf 'Remove unpublished key %s manually.\n' "$key_id" >&2
    fi
    rm -f "$work_dir/key.json" "$work_dir/dockerconfig.json"
    rmdir "$work_dir"
  }
  trap cleanup_gar_key_files EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  # Generate a JSON key for the download account, without printing its contents.
  gcloud iam service-accounts keys create "$work_dir/key.json" \
    --iam-account="$pull_account" --project="$credential_project" \
    --key-file-type=json --quiet

  # GAR accepts username "_json_key" and the entire JSON key as the password.
  # Docker's auth field is base64(username:password). Its registry key is the
  # hostname only; repository paths belong to the image addresses.
  jq -n --arg host "$gar_host" --rawfile key "$work_dir/key.json" \
    '{auths: {($host): {auth: ("_json_key:" + $key | @base64)}}}' \
    > "$work_dir/dockerconfig.json"

  # Upload the Docker login as a new version of the Secret Manager secret.
  gcloud secrets versions add "$secret_name" --project="$credential_project" \
    --data-file="$work_dir/dockerconfig.json" --quiet
  published=true

  # The stored login is now available to ESO. Delete the local private files;
  # the published Google key stays active so clusters can use that login.
  cleanup_gar_key_files
  trap - EXIT INT TERM
fi
```

**5. Deliver the login to the operator and HostedCluster through ESO.**

The [ESO definitions](../examples/external-secret.yaml)
configure access to the customer-project secret and merge its GAR entry with
existing registry credentials in both namespaces. The
[merge patch](../examples/managed-pull-secret-patch.yaml)
then connects each existing `pull-secret` definition to that combined result.

```bash
# Create the GAR SecretStore and one merging ExternalSecret in each namespace.
# These POC definitions contain the project, cluster, and namespace names above.
kubectl --context="$management_context" apply \
  -f "$poc_dir/examples/external-secret.yaml"

for namespace in "$hc_namespace" hypershift; do
  # Wait until ESO has built the intermediate combined secret.
  kubectl --context="$management_context" -n "$namespace" wait \
    externalsecret/gcp-gar-pull-secret --for=condition=Ready --timeout=120s

  # Keep the existing secret name, its controller, and other registry entries.
  # HostedCluster.spec.pullSecret.name remains "pull-secret".
  kubectl --context="$management_context" -n "$namespace" patch \
    externalsecret pull-secret --type=merge \
    --patch-file="$poc_dir/examples/managed-pull-secret-patch.yaml"

  # Ask ESO to refresh immediately rather than waiting for its normal interval.
  kubectl --context="$management_context" -n "$namespace" annotate \
    externalsecret pull-secret force-sync="$(date +%s)" --overwrite

  kubectl --context="$management_context" -n "$namespace" wait \
    externalsecret/pull-secret --for=condition=Ready --timeout=120s
done
```

**6. Check storage and delivery without displaying credentials.**

```bash
# Show Secret Manager version metadata only.
gcloud secrets versions list "$secret_name" --project="$credential_project" \
  --filter='state=ENABLED' --format='table(name,state,createTime)'

# Each namespace should print "true": the final pull secret has a GAR entry.
# This checks delivery; actual image downloads are validated after redirects.
for namespace in "$hc_namespace" hypershift; do
  printf 'Checking namespace: %s\n' "$namespace"
  kubectl --context="$management_context" -n "$namespace" get \
    secret pull-secret -o json \
    | jq -e --arg host "$gar_host" \
      '.data[".dockerconfigjson"] | @base64d | fromjson
       | (.auths[$host].auth // "") | length > 0'
done
```

The next section explains the merge and how HyperShift passes the resulting
credentials to the control plane and workers.

</details>

## Inject the pull secret into the clusters

### Add it to the existing pull secret

ESO read two inputs: the existing OpenShift pull secret and the GAR login document.
Its template added the GAR entry to the existing `auths` map, preserving the other
registry entries and fields. The combined result was written to a Kubernetes
secret named `gcp-gar-pull-secret`.

The existing ExternalSecret named `pull-secret` then imported that combined value.
An ExternalSecret tells ESO how to produce a Kubernetes secret. This fragment
shows the link added to the existing definition:

```yaml
spec:
  target:
    name: pull-secret
    template:
      templateFrom:
        - secret:
            name: gcp-gar-pull-secret
            items:
              - key: .dockerconfigjson
                templateAs: Values
```

This kept `HostedCluster.spec.pullSecret.name` set to `pull-secret`. Updating the
ExternalSecret definition allowed the existing controller to maintain the combined
credentials automatically.

After configuring the ESO merge described above, check delivery in the
management-cluster Kubernetes context:

```bash
cluster_namespace="example-hosted-cluster-namespace"
kubectl -n "$cluster_namespace" wait --for=condition=Ready \
  externalsecret/gcp-gar-pull-secret externalsecret/pull-secret --timeout=120s

kubectl -n "$cluster_namespace" get secret pull-secret -o json \
  | jq -e --arg host "$gar_host" \
    '.data[".dockerconfigjson"] | @base64d | fromjson
     | (.auths[$host].auth // "") | length > 0'
```

The final command prints `true` when a GAR login entry is present, without
displaying it. Repeat the check with `cluster_namespace="hypershift"` for the
operator's secret.

### How the cluster picked it up

1. ESO updated `.dockerconfigjson` in the existing `pull-secret` in the
   HostedCluster's namespace.
2. HyperShift read that secret and copied it into the hosted control-plane
   namespace. Control-plane pods referenced this local copy for image downloads.
3. HyperShift also included the credentials in the initial setup data for new
   workers. The normal node software used them when downloading images.
4. Existing controllers copied the credentials into the hosted OpenShift cluster
   for its own registry operations.

The operator's own `pull-secret` in the `hypershift` namespace received the GAR
entry through the same ESO method. It is separate from each HostedCluster's secret.

## Redirect image downloads to GAR

**The image settings choose where to download; the pull secret supplies the login
for that destination.** Both must be configured.

There are five settings across three Kubernetes resources:

| Setting | Resource and namespace | Field |
| --- | --- | --- |
| Control-plane image mapping | Deployment `operator` in `hypershift` | Operator container's `args`: `--registry-overrides` |
| Operator and startup-container images | Same Deployment | `containers[].image` and `initContainers[].image` |
| DNS updater image | Deployment `external-dns` in `hypershift` | `containers[].image` |
| Operator catalog mapping | Each HostedCluster in its management-cluster namespace | `metadata.annotations` |
| Worker image mirrors | Same HostedCluster | `spec.imageContentSources` |

Each setting below shows its original source, the change for GAR, and the result.
Project and cluster names are examples. The operator tag and DNS image digest are
the versions used in the POC. The YAML shows relevant fields of existing resources;
preserve their other fields and arguments.

### 1. Control-plane image mapping: operator argument

**Original setting and source.** Gecko sets `HostedCluster.spec.release.image` in
`gecko/controllers/hc/manifest/manifests.go`, in `buildHostedCluster`. HyperShift
reads the component image addresses from `release-manifests/image-references`
inside that OpenShift release image. Those addresses normally point to Quay.
The operator's original argument list, stored in
`gcp-hcp-infra/kustomize/hypershift/hypershift.yaml`, has no GAR mapping.

**Change for GAR.** Add `--registry-overrides` to the operator's arguments, mapping
both public registries to the shared virtual repository. To keep this setting in
Git, add the following patch under `spec.source.kustomize.patches` in
`gcp-hcp-infra/argocd/config/management-cluster/hypershift/template.yaml`:

```yaml
- target:
    kind: Deployment
    name: operator
    namespace: hypershift
  patch: |-
    - op: add
      path: /spec/template/spec/containers/0/args/-
      value: --registry-overrides=quay.io=us-central1-docker.pkg.dev/example-project/openshift-pullthrough,registry.redhat.io=us-central1-docker.pkg.dev/example-project/openshift-pullthrough
```

**Result.** HyperShift passes the mapping to the control-plane operator. It
replaces the source registry prefix when preparing component images, retaining
the image path and digest. For example,
`quay.io/openshift-release-dev/ocp-v4.0-art-dev` becomes
`us-central1-docker.pkg.dev/example-project/openshift-pullthrough/openshift-release-dev/ocp-v4.0-art-dev`.
Generated control-plane workloads then contain GAR image addresses.

### 2. Operator images: main container and startup container

**Original setting and source.** The operator Deployment comes from
`gcp-hcp-infra/kustomize/hypershift/hypershift.yaml`, generated by `update.sh`
using `hypershift install render`. Its initial image is
`quay.io/hypershift/hypershift-operator:latest`. The adjacent
`kustomization.yaml` replaces that address for both `operator` and
`init-environment` with the pinned image used in the deployment:

```yaml
image: quay.io/acm-d/rhtap-hypershift-operator:bc8675b2441e1f1d7f07d68687945f2de85e7526
```

**Change for GAR.** Replace the `quay.io` prefix in both image fields with the
virtual repository address, keeping the image path and pinned tag. Save these
image changes in the same infrastructure Application template used in setting 1.
The resulting Deployment fields are:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: operator
  namespace: hypershift
spec:
  template:
    spec:
      imagePullSecrets:
        - name: pull-secret
      containers:
        - name: operator
          image: us-central1-docker.pkg.dev/example-project/openshift-pullthrough/acm-d/rhtap-hypershift-operator:bc8675b2441e1f1d7f07d68687945f2de85e7526
      initContainers:
        - name: init-environment
          image: us-central1-docker.pkg.dev/example-project/openshift-pullthrough/acm-d/rhtap-hypershift-operator:bc8675b2441e1f1d7f07d68687945f2de85e7526
```

**Result.** Both the main container and startup container download the same
operator version through GAR, using `hypershift/pull-secret`. Keep their tag
aligned with the version pinned in `kustomization.yaml` when updating the operator.

**Note** : [MutatingAdmissionPolicy](https://github.com/openshift-online/gcp-hcp-infra/blob/f862a47daf814b1d6ccc44e7c6c7ab18ba679826/helm/charts/gar-image-rewriter/templates/policy.yaml) can be used to avoid touching the original image ref.

### 3. DNS updater: its own Deployment image

**Original setting and source.** The `external-dns` Deployment is also generated
into `gcp-hcp-infra/kustomize/hypershift/hypershift.yaml`. Its image field points
directly to Red Hat's registry:

```yaml
image: registry.redhat.io/edo/external-dns-rhel9@sha256:4450e6eac021e7f88f756f62e1043d6f8d6baddf99080ec4ad367c68ef5191f5
```

**Change for GAR.** Replace `registry.redhat.io` with the virtual repository
address, keeping `edo/external-dns-rhel9` and its digest. Save this image change in
`gcp-hcp-infra/argocd/config/management-cluster/hypershift/template.yaml` as a patch
for Deployment `external-dns`. The resulting fields are:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: external-dns
  namespace: hypershift
spec:
  template:
    spec:
      imagePullSecrets:
        - name: pull-secret
      containers:
        - name: external-dns
          image: us-central1-docker.pkg.dev/example-project/openshift-pullthrough/edo/external-dns-rhel9@sha256:4450e6eac021e7f88f756f62e1043d6f8d6baddf99080ec4ad367c68ef5191f5
```

**Result.** The DNS updater downloads its existing image through GAR, using
`hypershift/pull-secret`.

**Note** : [MutatingAdmissionPolicy](https://github.com/openshift-online/gcp-hcp-infra/blob/f862a47daf814b1d6ccc44e7c6c7ab18ba679826/helm/charts/gar-image-rewriter/templates/policy.yaml) can be used to avoid touching the original image ref.


### 4. Operator catalogs: HostedCluster annotation

**Original setting and source.** HyperShift chooses the Red Hat, certified, and
community catalog images from its defaults in
`hypershift/support/catalogs/images.go`. Their source prefix is
`registry.redhat.io/redhat`, for example
`registry.redhat.io/redhat/redhat-operator-index:v<major>.<minor>`. HyperShift
selects the catalog version from the OpenShift release. The original
HostedCluster generated by Gecko does not set a catalog registry override.

**Change for GAR.** Add the following annotation to the HostedCluster. For
permanent configuration, add it to the annotations map in Gecko's
`buildHostedCluster` function in `controllers/hc/manifest/manifests.go`:

```yaml
apiVersion: hypershift.openshift.io/v1beta1
kind: HostedCluster
metadata:
  name: example-cluster
  namespace: clusters-example
  annotations:
    hypershift.openshift.io/olm-catalogs-is-registry-overrides: registry.redhat.io/redhat=us-central1-docker.pkg.dev/example-project/openshift-pullthrough/redhat
```

**Result.** HyperShift copies the annotation to the hosted control plane and uses
GAR for catalog lookups and imports. The source prefix
`registry.redhat.io/redhat` becomes
`us-central1-docker.pkg.dev/example-project/openshift-pullthrough/redhat`.
The destination includes `/redhat` to preserve the original image path.

### 5. Worker mirrors: HostedCluster imageContentSources

**Original setting and source.** Worker image addresses come from the OpenShift
release and normally point to Quay, including
`quay.io/openshift-release-dev/ocp-v4.0-art-dev`. The original HostedCluster built
by Gecko in `controllers/hc/manifest/manifests.go` has no `imageContentSources`
entry for GAR. Without a matching mirror rule, workers contact the source registry.

**Change for GAR.** Add a source-to-mirror entry under
`HostedCluster.spec.imageContentSources`. For permanent configuration, have
Gecko's `buildHostedCluster` add this entry to the HostedCluster's `spec`,
preserving any existing mirror rules and its `pull-secret` reference:

```yaml
apiVersion: hypershift.openshift.io/v1beta1
kind: HostedCluster
metadata:
  name: example-cluster
  namespace: clusters-example
spec:
  pullSecret:
    name: pull-secret
  imageContentSources:
    - source: quay.io/openshift-release-dev/ocp-v4.0-art-dev
      mirrors:
        - us-central1-docker.pkg.dev/example-project/openshift-pullthrough/openshift-release-dev/ocp-v4.0-art-dev
```

**Result.** HyperShift turns the entry into worker registry configuration. Workers
can keep the original Quay image names: CRI-O, their image-download software,
tries the GAR address for matching digest-based requests and authenticates with
the GAR entry in the pull secret. Other image sources need their own matching
rules.

**Changing the mirror list rolls out every NodePool in the HostedCluster**—all
its managed worker groups. Preserve existing entries when adding rules. Restricting
direct registry access remains a separate network setting.

### How the pull secret authenticates these downloads

- The operator and DNS updater reference `hypershift/pull-secret`, as shown above.
- Hosted control-plane pods reference the copy of `pull-secret` in their own
  namespace through `imagePullSecrets`. HyperShift also supplies its credentials
  for release-image and catalog lookups.
- New workers receive the combined credentials in their initial setup data.
  CRI-O uses the GAR login when contacting `us-central1-docker.pkg.dev`, even when
  the requested image originally had a Quay address.

The login entry matches the GAR hostname. The repository path chooses which GAR
repository serves the request. This uses normal pull-secret authentication and
requires no additional credential helper.

### What GAR does after authentication

GAR checks that the cluster's Google account has permission to download from the
virtual repository. It serves a cached image or retrieves it through one of the
backing repositories. GAR uses its own Quay or Red Hat login for that last step;
those source passwords are separate from the cluster's GAR login.

## Automate credential creation and rotation

The outline below follows the JSON-key option selected for the POC. Production
work must compare this with short-lived authentication and prove credential
refresh for every consumer.

**Initial idea:** one download account and login per region, managed by a
Job/CronJob in one management cluster. ESO distributes it to the region's
clusters. Regional automation remains untested.

| Step | Initial implementation idea |
| --- | --- |
| Setup | Infrastructure creates a GAR Reader account and an empty Secret Manager secret in the regional project. The Job uses its own Workload Identity with scoped management permissions; ESO gets read-only access. |
| Generate | Reuse a valid login, or create a JSON key, build the Docker login, test GAR access, and upload it. Keep credentials outside Git, logs, and Terraform state. |
| Deliver | Infrastructure configures the operator's ESO merge; Gecko configures each HostedCluster's merge. Preserve existing registry entries. Wait for final pull secrets before enabling GAR or creating workers. |
| Rotate | Create/test replacement → publish → confirm every consumer updated → disable old key → verify fresh pulls → delete after observation. Keep the old key enabled until all consumers update; save progress for recovery. |
| Remove | Remove deleted clusters from the consumer list. Retain credentials shared by remaining clusters. |

**To prove:** key policy, account sharing and rotation period; updates to existing
GCP workers and startup data; tracking delivery, handling unavailable clusters,
and recovering interrupted jobs. ESO sync alone does not prove worker updates.

**First spike:** prove setup without an existing login, reuse on redeployment,
and forced rotation. Include existing workers, scale-out of an existing NodePool
after an ignition-server restart, and recovery. Verify fresh downloads with the
old key disabled and direct public-registry access blocked.

See the [draft pull-secret lifecycle plan](PULL-SECRET-LIFECYCLE-PLAN.md) for details.

## Proposed implementation sequence

Complete the GAR and credential prerequisites first. Then switch workers,
control-plane components, and supporting services to GAR. Restrict internet
access last. Each PR must keep clusters working; test changes in one selected
environment before applying them more widely.

| Step | Work | Epic |
| --- | --- | --- |
| 1 | Create the regional GAR repositories | New prerequisite epic |
| 2 | Automate pull-secret creation, delivery, and rotation | New prerequisite epic |
| 3 | Switch worker image downloads to GAR | GCP-1268 |
| 4 | Switch control-plane and supporting image downloads to GAR | GCP-1268 |
| 5 | Limit network access to required connections | GCP-1268 |

Each dropdown describes the proposed PRs and checks for that step.

### New epic: Regional GAR and pull-secret lifecycle

Owns steps 1–2. GAR setup is straightforward; credential setup and rotation need
investigation first.

<details>
<summary>Spike: Prove GAR key setup and rotation</summary>

Confirm key policy, account sharing, permissions, and rotation period. Prove that
a job authenticated through Workload Identity can generate and store the JSON-key
login, ESO can deliver it, and consumers can use a replacement.

Include job startup before the login exists, an unavailable cluster, and recovery
from an interrupted replacement. Record the evidence and implementation PRs.

</details>

<details>
<summary>1. Create the regional GAR repositories</summary>

**PR — Provision regional image repositories (`gcp-hcp-infra`).** Create the Quay
and Red Hat remote repositories, their source credentials and permissions, and
one virtual repository. Publish the virtual repository address.

**Check:** fresh Quay and Red Hat image requests work through GAR. Existing
clusters keep their current image configuration.

</details>

<details>
<summary>2. Automate pull-secret creation, delivery, and rotation</summary>

Split this into separate PRs:

- **Create and reuse the GAR login (`gcp-hcp-infra`).** Generate and verify the
  login, store it in Secret Manager, and reuse it on redeployment.
- **Merge the login into the operator pull secret (`gcp-hcp-infra`).** Use ESO
  and preserve existing registry entries.
- **Merge the login into HostedCluster pull secrets (`gecko`).** Generate each
  cluster's ESO definition and keep the existing `pull-secret` name.
- **Refresh GCP worker credentials and startup data (`hypershift`, if needed).**
  Resolve updates for existing workers and workers added later.
- **Rotate credentials with recovery (`gcp-hcp-infra`).** Verify all consumers
  have updated before disabling the old key. Test fresh pulls before deleting it.

**Check:** initial setup, redeployment, forced rotation, and recovery work.
Existing workers and new workers in an existing NodePool can pull with the old
key disabled, including after an ignition-server restart. Keep image redirects
disabled outside the test setup until this is proven.

</details>

### GCP-1268: Switch control-plane and worker image downloads to GAR

Owns steps 3–5. Start with workers, then control-plane components. Keep existing
public-registry access until step 5.

<details>
<summary>3. Switch worker image downloads to GAR</summary>

**PR — Configure regional worker mirrors (`gecko`).** Set
`HostedCluster.spec.imageContentSources` for selected clusters, preserving other
entries. Test a new HostedCluster, then an existing one.

**Check:** worker creation, replacement, scale-out, and image downloads work
after rotation. Changing these mirrors rolls an existing cluster's NodePools;
schedule that transition. The control plane keeps its current registry access.

</details>

<details>
<summary>4. Switch control-plane and supporting image downloads to GAR</summary>

Deliver separate PRs, checking fresh downloads and readiness after each:

- **Operator catalogs (`gecko`).** Set the HostedCluster catalog annotation and
  verify the catalogs become ready.
- **Control-plane images (`gcp-hcp-infra`).** Configure `--registry-overrides` on
  a dedicated test management cluster first. It affects every HostedCluster
  served by the operator; all must have GAR credentials before enabling it.
- **DNS updater (`gcp-hcp-infra`).** Preserve its version and verify DNS updates.
  Validate pod image rewriting to retain the upstream image reference in Git.
- **Operator and startup container (`gcp-hcp-infra`).** Use the same rewriting
  approach and versions. Verify restart and continued reconciliation.

**Check:** APIs, catalogs, DNS, and operator reconciliation remain healthy
through restarts and credential rotation.

</details>

<details>
<summary>5. Limit network access to required connections</summary>

**Investigate first.** Record required destinations and ports for workers,
control-plane pods, and management nodes during creation, upgrades, and rotation.
Review existing broad access rules. Decide how public dependencies such as
telemetry, identity providers, or webhooks are disabled or reached privately.

Then deliver separate PRs:

- **Restricted network provisioning (`gcp-hcp-ctl`).** Both `network create` and
  `cluster create --setup-infra` currently create NAT. Add a restricted option
  retaining Private Google Access. Repeat setup must not recreate NAT; report
  migration needed for existing networks and preserve normal mode.
- **Worker restrictions.** Remove NAT and external worker IPs. Allow only the
  required GAR/Google access, approved DNS, cluster endpoints, and internal
  traffic. Enforce this with network rules as well as provisioning settings.
- **Control-plane pod restrictions.** Start with selected workloads. Preserve
  required DNS, API/etcd communication, worker connections, Google APIs, and
  [Workload Identity metadata access](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/network-policy).
- **Management-node restrictions.** Image downloads use the node's container
  runtime, so pod policies alone do not cover them. Restrict public-registry
  access at the node level too. Management NAT is shared: check other management
  services before removing it or use a separate node/network scope. See
  [image pulls](https://kubernetes.io/docs/concepts/containers/images/#image-pull-policy)
  and [NetworkPolicy behavior](https://kubernetes.io/docs/concepts/services-networking/network-policies/).

**DNS stays available.** Both workers and control planes need it. Use approved
resolvers and private DNS; block arbitrary public resolvers. Limiting queried
names needs resolver-level rules and tests. See [private GAR DNS setup](https://docs.cloud.google.com/artifact-registry/docs/gke-private-clusters).

**Check:** fresh pulls, cluster creation, restarts, rotation, and upgrades work.
Public registries and unrelated internet destinations are blocked from pods and
nodes. The exact control-plane connection list and enforcement method still
need validation.

</details>

### Tests and supporting improvements

<details>
<summary>Tests and operational requirements for every functional PR</summary>

- **Unit tests:** credential reuse, interrupted/failed rotation, registry-entry
  and image-version preservation, optional settings, and CLI behavior with NAT
  disabled, including repeat setup and cleanup.
- **Integration tests:** Secret Manager → ESO → control-plane/worker delivery;
  existing-worker updates, existing NodePool scale-out, rotation, and recovery
  after an ignition-server restart.
- **Network and upgrade tests:** required connections succeed; public registries,
  unrelated internet addresses, and unapproved DNS resolvers fail. Cover pods,
  nodes, enabled IP families, new/existing clusters, restarts, and upgrades.
  Use uncached images to verify downloads.
- **Operational improvements:** report credential age, failed rotation, and
  clusters awaiting updates without logging secrets. Document setup, migration,
  rollback, troubleshooting, and CLI usage.

Check whether image-address or startup-data changes alter NodePool hashes and
cause a rollout. Hash-affecting HyperShift changes must pass
`e2e-aws-upgrade-hypershift-operator`.

Keep credentials available during rollback. After blocking internet access,
restore the required registry access before reverting image redirects.

</details>

## Reference files

- [POC setup, manifests, and recorded results](../README.md)
  are collected on the `gar-pull-secret-poc-reference` branch.
- Saved operator settings: `gcp-hcp-infra`,
  `argocd/config/management-cluster/hypershift/template.yaml` and
  `kustomize/hypershift/secrets.yaml`.
- Cluster and secret generation: `gecko`, `controllers/hc/manifest/manifests.go`.
- [Worker credential-update guidance](../../docs/content/how-to/common/global-pull-secret.md).
