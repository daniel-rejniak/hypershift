# Dev-only setup. Upstream credential values remain in memory and Secret Manager.
import base64
import json
import os
from pathlib import Path
import subprocess
import sys

GCLOUD = os.environ.get('GCLOUD_BIN', 'gcloud')
PROJECT = 'dev-mgt-us-c1-drejniak'
NUMBER = '525670108009'
LOCATION = 'us-central1'
SECRET = 'redhat-pullthrough-upstream-password'
AGENT = 'service-' + NUMBER + '@gcp-sa-artifactregistry.iam.gserviceaccount.com'
PULLER = 'gar-pull-secret-poc@drejniak-hcp-dev.iam.gserviceaccount.com'
ROOT = Path(__file__).resolve().parent
os.umask(0o077)

def command(label, args, data=None, optional=False):
    result = subprocess.run([GCLOUD] + args + ['--quiet'], input=data, capture_output=True, text=True)
    if result.returncode and not optional:
        # Avoid printing command arguments, credentials, or raw secret output.
        print(json.dumps({'operation': label, 'status': 'failed', 'exitCode': result.returncode}), flush=True)
        if label == 'create Red Hat repository':
            safe = result.stderr.replace(username, '[upstream username]').replace(password, '[upstream password]')
            print(safe[-3500:], flush=True)
        sys.exit(result.returncode)
    return result

source = command('read existing Red Hat credential', ['secrets', 'versions', 'access', 'latest', '--secret=default-openshift-pull-secret', '--project=gcp-hcp-commons-dev'])
entry = json.loads(source.stdout)['auths']['registry.redhat.io']
if entry.get('auth'):
    username, password = base64.b64decode(entry['auth']).decode().split(':', 1)
else:
    username, password = entry['username'], entry['password']
assert username and password

exists = command('describe upstream secret', ['secrets', 'describe', SECRET, '--project=' + PROJECT, '--format=json'], optional=True)
if exists.returncode:
    command('create upstream secret', ['secrets', 'create', SECRET, '--project=' + PROJECT, '--replication-policy=automatic', '--labels=purpose=gar-pull-secret-poc'])
versions = command('list upstream secret versions', ['secrets', 'versions', 'list', SECRET, '--project=' + PROJECT, '--filter=state=ENABLED', '--sort-by=~createTime', '--limit=1', '--format=value(name)']).stdout.strip()
if not versions:
    command('publish upstream credential', ['secrets', 'versions', 'add', SECRET, '--project=' + PROJECT, '--data-file=-'], data=password)
    versions = command('read published version identifier', ['secrets', 'versions', 'list', SECRET, '--project=' + PROJECT, '--filter=state=ENABLED', '--sort-by=~createTime', '--limit=1', '--format=value(name)']).stdout.strip()
version = versions.rsplit('/', 1)[-1]
command('grant GAR access to upstream credential', ['secrets', 'add-iam-policy-binding', SECRET, '--project=' + PROJECT, '--member=serviceAccount:' + AGENT, '--role=roles/secretmanager.secretAccessor', '--format=json'])
print(json.dumps({'operation': 'upstream credential', 'secret': SECRET, 'version': version, 'status': 'ready'}), flush=True)

def describe(repo):
    return command('describe ' + repo, ['artifacts', 'repositories', 'describe', repo, '--project=' + PROJECT, '--location=' + LOCATION, '--format=json'], optional=True)

if describe('redhat-pullthrough').returncode:
    command('create Red Hat repository', ['artifacts', 'repositories', 'create', 'redhat-pullthrough', '--project=' + PROJECT, '--location=' + LOCATION, '--repository-format=docker', '--mode=remote-repository', '--remote-docker-repo=https://registry.redhat.io', '--remote-username=' + username, '--remote-password-secret-version=projects/' + NUMBER + '/secrets/' + SECRET + '/versions/' + version, '--description=Red Hat pull-through cache for drejniak zero-egress POC', '--labels=purpose=gar-pull-secret-poc', '--disable-vulnerability-scanning', '--format=json'])
print(json.dumps({'operation': 'remote repository', 'name': 'redhat-pullthrough', 'status': 'ready'}), flush=True)

for repo in ['quay-pullthrough', 'redhat-pullthrough']:
    command('grant virtual service agent access to ' + repo, ['artifacts', 'repositories', 'add-iam-policy-binding', repo, '--project=' + PROJECT, '--location=' + LOCATION, '--member=serviceAccount:' + AGENT, '--role=roles/artifactregistry.reader', '--format=json'])

policy = [{'id': 'quay', 'repository': 'projects/' + PROJECT + '/locations/' + LOCATION + '/repositories/quay-pullthrough', 'priority': 100}, {'id': 'redhat', 'repository': 'projects/' + PROJECT + '/locations/' + LOCATION + '/repositories/redhat-pullthrough', 'priority': 90}]
policy_file = ROOT / 'gar-upstream-policies.json'
assert json.loads(policy_file.read_text()) == policy, 'Upstream policy file does not match this environment'
if describe('openshift-pullthrough').returncode:
    command('create virtual repository', ['artifacts', 'repositories', 'create', 'openshift-pullthrough', '--project=' + PROJECT, '--location=' + LOCATION, '--repository-format=docker', '--mode=virtual-repository', '--upstream-policy-file=' + str(policy_file), '--description=Quay and Red Hat virtual mirror for drejniak zero-egress POC', '--labels=purpose=gar-pull-secret-poc', '--format=json'])
command('grant existing pull identity virtual access', ['artifacts', 'repositories', 'add-iam-policy-binding', 'openshift-pullthrough', '--project=' + PROJECT, '--location=' + LOCATION, '--member=serviceAccount:' + PULLER, '--role=roles/artifactregistry.reader', '--format=json'])
print(json.dumps({'operation': 'virtual repository', 'name': 'openshift-pullthrough', 'status': 'ready', 'upstreams': policy}), flush=True)
