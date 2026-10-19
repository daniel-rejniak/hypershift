#!/usr/bin/env bash
# Dev-only GAR pull-secret POC. Private key material never enters Terraform state.
set -euo pipefail
set +x
umask 077

customer_project="${CUSTOMER_PROJECT:-drejniak-hcp-dev}"
mirror_project="${MIRROR_PROJECT:-dev-mgt-us-c1-drejniak}"
mirror_location="${MIRROR_LOCATION:-us-central1}"
mirror_repository="${MIRROR_REPOSITORY:-quay-pullthrough}"
management_project="${MANAGEMENT_PROJECT:-dev-mgt-us-c1-drejniak}"
account_id="gar-pull-secret-poc"
account="${account_id}@${customer_project}.iam.gserviceaccount.com"
secret="gar-pull-secret-poc"
eso_member="serviceAccount:${management_project}.svc.id.goog[external-secrets-system/external-secrets]"

for command in gcloud jq; do
  command -v "$command" >/dev/null || { echo "Missing command: $command" >&2; exit 1; }
done

gcloud services enable iam.googleapis.com secretmanager.googleapis.com \
  --project="$customer_project" --quiet

if ! gcloud iam service-accounts describe "$account" \
  --project="$customer_project" --quiet >/dev/null 2>&1; then
  gcloud iam service-accounts create "$account_id" \
    --project="$customer_project" --display-name="Dev GAR pull-secret POC" --quiet
fi

# Grant access to this repository only, not to the entire mirror project.
gcloud artifacts repositories add-iam-policy-binding "$mirror_repository" \
  --project="$mirror_project" --location="$mirror_location" \
  --member="serviceAccount:$account" --role=roles/artifactregistry.reader \
  --quiet >/dev/null

if ! gcloud secrets describe "$secret" --project="$customer_project" \
  --quiet >/dev/null 2>&1; then
  gcloud secrets create "$secret" --project="$customer_project" \
    --replication-policy=automatic --labels=purpose=gar-pull-secret-poc --quiet
fi

# Reuse the existing ESO Kubernetes identity across projects, with access to
# this secret only. The existing OpenShift pull secret stays in commons-dev.
gcloud secrets add-iam-policy-binding "$secret" --project="$customer_project" \
  --member="$eso_member" --role=roles/secretmanager.secretAccessor \
  --quiet >/dev/null

existing_version="$(gcloud secrets versions list "$secret" \
  --project="$customer_project" --filter='state=ENABLED' --limit=1 \
  --format='value(name)' --quiet)"
if [[ -n "$existing_version" ]]; then
  echo "An enabled version already exists in $customer_project/$secret; keeping it."
  exit 0
fi

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/gar-pull-secret-poc.XXXXXXXX")"
published=false
cleanup() {
  # Roll back a newly minted key if publishing its secret failed.
  if [[ "$published" != true && -s "$work_dir/key.json" ]]; then
    key_id="$(jq -er .private_key_id "$work_dir/key.json")"
    gcloud iam service-accounts keys delete "$key_id" \
      --iam-account="$account" --project="$customer_project" --quiet || \
      echo "Could not remove unpublished key $key_id; remove it manually." >&2
  fi
  rm -rf "$work_dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

gcloud iam service-accounts keys create "$work_dir/key.json" \
  --iam-account="$account" --project="$customer_project" \
  --key-file-type=json --quiet

# GAR accepts a service-account JSON key as the _json_key password.
jq -n --arg host "${mirror_location}-docker.pkg.dev" \
  --rawfile key "$work_dir/key.json" \
  '{auths: {($host): {auth: ("_json_key:" + $key | @base64)}}}' \
  > "$work_dir/dockerconfig.json"

gcloud secrets versions add "$secret" --project="$customer_project" \
  --data-file="$work_dir/dockerconfig.json" --quiet
published=true
echo "GAR credentials are stored in $customer_project/$secret."
echo "Apply external-secret.yaml to create the merged Kubernetes pull secret."
