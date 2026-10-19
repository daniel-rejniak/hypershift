# Proposal: regional GAR for OpenShift images

**Decision overview · 19 October 2026**

## Overview

OpenShift downloads platform images from Quay and Red Hat's registry. Both the
control plane, which manages the cluster, and the workers, which run applications,
need access to these images during creation, replacement, and upgrades.

**Provide one regional Google Artifact Registry (GAR) address for those downloads.**
Use existing HyperShift settings and normal pull secrets, which contain registry
login details. Make this part of deployment, then restrict direct access to public
registries after validation.

The proof of concept (POC) demonstrated GAR image downloads for workers and the
control plane. The proposed pull-secret approach reuses existing components and
the standard operator build, with no additional credential helper on workers.

## Why two repositories and one shared address?

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

## Which images are covered?

| Source | Main image categories | Distinct image addresses |
| --- | --- | --- |
| Quay | OpenShift release images; API servers; cluster controllers; networking, storage, and monitoring components; the HyperShift operator | 30 |
| Red Hat | Operator catalogs for Red Hat, certified, and community operators; the DNS updater (`external-dns`) | 4 |

**The 34 distinct image addresses are an example from one evaluated control-plane
and management configuration.** Its 37 control-plane workloads used 32 different
images, with the HyperShift operator and DNS updater adding two more. Replicas
and repeated use of an image do not increase this count.

The recorded running control plane had **173 container image references across
58 active pods**. The same image can appear in many containers and replicas, so
this is a larger count. Neither figure is a total of network downloads; actual
downloads depend on node caching and restarts.

Workers also download images from the OpenShift release, including images used
when a node starts. The complete regional image set depends on supported OpenShift
versions and installed operators. Additional application registries need their
own configuration. Keeping both sources behind one address lets the regional
service supply these categories through the same GAR login.

## What the POC demonstrated

| Area | Evidence |
| --- | --- |
| Workers | OpenShift release images downloaded through GAR with the customer's outbound internet gateway removed. |
| Control plane | Image settings covered all 37 workloads. Successful GAR download events and authenticated image checks were recorded. |
| APIs and catalogs | Both API deployments had all three replicas ready. All three tested catalogs were ready. The HostedCluster reported Available and not Degraded. |
| Supporting components | The operator and DNS updater used GAR with their existing image versions. |
| Pull secrets | Secret Manager and External Secrets Operator (ESO), which copies stored credentials into Kubernetes, supplied combined pull secrets. |

These results support the proposed design.

## How the pull secret was generated, injected, and picked up

The high-level sequence is:

- Create a Google service account with permission to download the required GAR
  images.
- Generate a JSON key for that account.
- Use the key to create a Docker login entry for the regional GAR hostname.
- Upload the GAR login document to Google Secret Manager.
- Configure ESO to combine it with the existing OpenShift pull secret and publish
  the result for the operator and hosted clusters.

### Credential format and storage

The key is generated with `gcloud iam service-accounts keys create`. The Docker
login uses `_json_key` as its username and the full JSON key as its password.
Its `auth` value is `base64("_json_key:" + key_json)`. Base64 is encoding, not
encryption.

The resulting Docker configuration is a JSON document containing the GAR hostname
and its encoded login. It is uploaded as a secret version in Google Secret
Manager, where ESO can read it. Repeating the setup reuses the stored credential
instead of creating another key.
[Google's authentication documentation](https://docs.cloud.google.com/artifact-registry/docs/docker/authentication#json-key).

### Manual example: generate and upload the GAR login

Run these commands in one Bash session with `gcloud` and `jq` installed and
Google Cloud authentication configured. The regional virtual repository must
already exist, and the IAM and Secret Manager APIs must be enabled. Replace the
example project IDs. Skip resource-creation commands for resources that already
exist; normal deployments should reuse the stored credential.

**1. Create an account that can download from the virtual repository.**

```bash
credential_project="example-credentials-project"
gar_project="example-regional-project"
gar_region="us-central1"
gar_repository="openshift-pullthrough"
gar_host="${gar_region}-docker.pkg.dev"
pull_account="gar-puller@${credential_project}.iam.gserviceaccount.com"
secret_name="regional-gar-pull-secret"

gcloud iam service-accounts create gar-puller --project="$credential_project"
gcloud artifacts repositories add-iam-policy-binding "$gar_repository" \
  --project="$gar_project" --location="$gar_region" \
  --member="serviceAccount:$pull_account" --role=roles/artifactregistry.reader
```

The repository permission covers images available through the virtual repository.
[GAR access control](https://docs.cloud.google.com/artifact-registry/docs/access-control#granting_roles).

**2. Generate the key and turn it into a Docker login document.**

```bash
set +x
umask 077
work_dir="$(mktemp -d)"

gcloud iam service-accounts keys create "$work_dir/key.json" \
  --iam-account="$pull_account" --project="$credential_project"

jq -n --arg host "$gar_host" --rawfile key "$work_dir/key.json" \
  '{auths: {($host): {auth: ("_json_key:" + $key | @base64)}}}' \
  > "$work_dir/dockerconfig.json"
```

This produces the GAR-only login document in private temporary files. ESO combines
it with the existing OpenShift pull secret in the next section.

**3. Upload the document to Secret Manager.**

```bash
gcloud secrets create "$secret_name" --project="$credential_project" \
  --replication-policy=automatic
gcloud secrets versions add "$secret_name" --project="$credential_project" \
  --data-file="$work_dir/dockerconfig.json"
```

Secret Manager now holds the JSON document that ESO will read.
[Uploading a secret version](https://docs.cloud.google.com/secret-manager/docs/add-secret-version).
After a successful upload, remove the local copies; the stored credential stays
valid:

```bash
rm "$work_dir/key.json" "$work_dir/dockerconfig.json"
rmdir "$work_dir"
```

**4. Allow the existing ESO identity to read this secret.**

The POC used the `external-secrets` Kubernetes service account in the
`external-secrets-system` namespace on a GKE management cluster. For that setup:

```bash
management_project="example-management-project"
eso_member="serviceAccount:${management_project}.svc.id.goog[external-secrets-system/external-secrets]"

gcloud secrets add-iam-policy-binding "$secret_name" \
  --project="$credential_project" --member="$eso_member" \
  --role=roles/secretmanager.secretAccessor
```

Use the identity configured in the environment's ESO SecretStore. It is separate
from the image-download account created in step 1. The GAR SecretStore must point
to `credential_project`, and the merge definition's GAR `remoteRef.key` must be
`regional-gar-pull-secret`. The existing base-secret source stays unchanged.
[ESO Google authentication](https://external-secrets.io/latest/provider/google-secrets-manager/).

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

### Reuse the same process at scale

- Make key generation and storage a deployment job. Use private temporary files;
  keep credential values outside Git, logs, and Terraform state.
- Put the operator's ESO definition in infrastructure configuration. Have Gecko
  generate the equivalent definition for each HostedCluster.
- Reuse one merge template with the appropriate registry hostname, secret names,
  and namespace. The permanent definition can read both inputs and write
  `pull-secret` directly, without needing the intermediate secret.
- Wait for the combined secret to be ready before changing image addresses or
  creating workers. Keep one owner for each generated secret.
- Reuse credentials on normal deployments. During replacement, keep the old login
  valid until the new one has reached every consumer, including existing GCP
  workers and their startup data. A secret refresh alone does not update those
  workers' stored credentials. Key lifetime depends on organization policy and
  revocation.

## How mirroring and pull-secret authentication work

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

The YAML below shows the relevant fields, using example project and cluster names
and the proposed shared virtual repository. The operator tag and DNS image digest
are the versions used in the POC. These are excerpts for existing resources;
preserve their other fields and arguments.

**Permanent configuration:** the management-cluster settings belong in
`gcp-hcp-infra/argocd/config/management-cluster/hypershift/template.yaml`. This Argo
Application builds `kustomize/hypershift`: Deployment definitions are in
`hypershift.yaml`, and the operator version is pinned in `kustomization.yaml`.
The HostedCluster settings belong in Gecko's `buildHostedCluster` function in
`controllers/hc/manifest/manifests.go`.

### 1. Control-plane image mapping: operator argument

Add this entry under the Argo Application's `spec.source.kustomize.patches`:

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

This appends the mapping to the operator's existing arguments. HyperShift passes
the mapping to the control-plane operator and uses it when preparing component
images. For example, a Quay image under `openshift-release-dev/ocp-v4.0-art-dev`
gets the same path under the virtual repository. Its digest, which identifies
the image content, stays the same. Generated control-plane workloads then contain
GAR image addresses.

### 2. Operator images: main container and startup container

The operator's own images have explicit addresses in its Deployment. Both need
the GAR address. The resulting fields are:

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

The infrastructure template can set these two `image` fields with patches. Keep
them aligned with the operator version pinned in `kustomization.yaml`.

### 3. DNS updater: its own Deployment image

Set the image on Deployment `external-dns` separately, preserving its digest:

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

Both management Deployments use `hypershift/pull-secret` for authentication.

### 4. Operator catalogs: HostedCluster annotation

Set this annotation on each HostedCluster. HyperShift copies it to the hosted
control plane and uses it for catalog image lookups and imports:

```yaml
apiVersion: hypershift.openshift.io/v1beta1
kind: HostedCluster
metadata:
  name: example-cluster
  namespace: clusters-example
  annotations:
    hypershift.openshift.io/olm-catalogs-is-registry-overrides: registry.redhat.io/redhat=us-central1-docker.pkg.dev/example-project/openshift-pullthrough/redhat
```

The destination includes `/redhat` because the source includes that path. Gecko
would add the annotation alongside the existing HostedCluster annotations.

### 5. Worker mirrors: HostedCluster imageContentSources

Add the worker mapping to the same HostedCluster:

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

HyperShift turns this into worker configuration. Workers can keep the original
Quay image names: their normal image-download software, CRI-O, tries GAR for
matching digest-based requests. Other image sources need matching rules of their
own. Gecko would generate these entries in the HostedCluster's `spec`.

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

## Proposed implementation PRs

| Proposed PR title | Changes | Repository |
| --- | --- | --- |
| 1. Create regional Quay and Red Hat GAR caches with a shared virtual repository | Define the two remote repositories, their source credentials and access permissions, and the virtual repository. Publish its address for cluster configuration. | `gcp-hcp-infra` |
| 2. Generate GAR pull credentials and add them to the operator pull secret | Add the credential-generation job, store its output in Secret Manager, and update the operator's ExternalSecret to merge the GAR login with its existing credentials. Include credential replacement. | `gcp-hcp-infra` |
| 3. Merge GAR credentials into HostedCluster pull secrets | Update the ExternalSecret generated in `controllers/hc/manifest/manifests.go` to combine the base pull secret with the regional GAR login. Keep the existing `pull-secret` name and preserve other registry entries. | `gecko` |
| 4. Configure HyperShift and external-dns images to use regional GAR | Update the operator deployment template with `--registry-overrides` and GAR image addresses for the operator, its startup container, and external-dns. Preserve image versions. | `gcp-hcp-infra` |
| 5. Point HostedCluster operator catalogs at regional GAR | Have Gecko set `hypershift.openshift.io/olm-catalogs-is-registry-overrides`, preserving the `redhat` path in the destination. | `gecko` |
| 6. Add regional GAR mirrors to worker image configuration | Have Gecko populate `HostedCluster.spec.imageContentSources` with the required source-to-GAR mappings, preserving existing entries. | `gecko` |
| 7. Restrict direct public-registry access from control-plane and worker networks | Add network configuration that blocks direct registry access while retaining GAR, required Google services, and internal connections. | Infrastructure/network configuration |

Argo applies configuration from Git; Gecko creates and maintains HostedCluster
configuration. Put the GAR settings in these existing deployment paths so they
are applied automatically to new environments and retained after redeployment.
Use existing HyperShift features; any required fixes should be separate PRs.

Implementation references:

- [POC setup, manifests, and recorded results](examples/gcp-gar-pull-secret-poc/README.md)
  are collected on the `gar-pull-secret-poc-reference` branch.
- Saved operator settings: `gcp-hcp-infra`,
  `argocd/config/management-cluster/hypershift/template.yaml` and
  `kustomize/hypershift/secrets.yaml`.
- Cluster and secret generation: `gecko`, `controllers/hc/manifest/manifests.go`.
- [Worker credential-update guidance](docs/content/how-to/common/global-pull-secret.md).
