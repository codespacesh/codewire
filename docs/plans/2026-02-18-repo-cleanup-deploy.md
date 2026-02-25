# Repo Cleanup & Private Deploy Architecture

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Scrub `git.noel.sh`, `infisical.noel.sh`, and all internal cluster details from the public codewire repo history, and move all deployment logic into the private infra repo.

**Architecture:** codewire (public GitHub) becomes pure application code + public CI/CD only. The private infra repo gains a Gitea Actions deploy workflow that checks out codewire from GitHub, builds and pushes images to the private registry, and applies k8s manifests to the app cluster — keeping all internal hostnames, credentials, and cluster details private.

**Tech Stack:** git-filter-repo, Gitea Actions, kustomize, kubectl, Docker Buildx, RKE2/nginx ingress, cert-manager, Infisical operator

---

## What Gets Removed from codewire History

| Path | Why sensitive |
|------|--------------|
| `.gitea/workflows/ci.yaml` | `git.noel.sh`, `infisical.noel.sh`, identity IDs |
| `.gitea/workflows/deploy-prod.yaml` | `APP_K8S_SERVER`, cluster token paths |
| `deployments/k8s/` | `infisical.noel.sh` in registry-secret.yaml |

`demo/k8s/` in current committed state (ghcr.io refs, public domain) is not sensitive — but it should move to infra for clean separation. We'll delete it from history too since the new workflow will manage it from infra.

## New Architecture

```
codewire (public GitHub)
├── .github/workflows/ci.yml       # go test/vet/build — no internal details
├── .github/workflows/release.yml  # binaries, Helm, Homebrew — all public
├── demo/                          # source only (Dockerfile, showcase.sh, broker/)
├── docs/                          # website source
└── charts/                        # Helm chart (public)

infra (private)
├── .gitea/workflows/
│   └── deploy-codewire.yml        # builds images + deploys to app cluster
└── apps/
    ├── codewire-docs/k8s/         # moved from codewire deployments/k8s/
    └── codewire-demo/k8s/         # moved from codewire demo/k8s/
```

The infra deploy workflow triggers on `workflow_dispatch` (manual + optionally webhook from GitHub release).

---

## Task 1: Revert Uncommitted Changes in codewire

These were applied during investigation — don't commit them.

**Files:**
- Revert: `demo/k8s/broker-deployment.yaml`
- Delete: `demo/k8s/registry-secret.yaml`

**Step 1:** Revert modified file and delete new file

```bash
git restore demo/k8s/broker-deployment.yaml
rm demo/k8s/registry-secret.yaml
```

**Step 2:** Verify clean state

```bash
git status
```

Expected: only `.claude/settings.local.json` shows as modified (that's fine, not committed).

---

## Task 2: Scrub Sensitive Paths from Git History

**Files:** Affects entire git history of `/Users/noel/src/sonica/codewire`

**Step 1:** Install git-filter-repo if not present

```bash
pip3 install git-filter-repo 2>/dev/null || brew install git-filter-repo
git filter-repo --version
```

**Step 2:** Remove sensitive paths from history

```bash
cd /Users/noel/src/sonica/codewire
git filter-repo \
  --path .gitea --invert-paths \
  --path deployments --invert-paths \
  --path demo/k8s --invert-paths \
  --force
```

**Step 3:** Verify paths are gone

```bash
git log --all --full-history -- .gitea/workflows/ci.yaml | head -5
git log --all --full-history -- deployments/k8s/base/registry-secret.yaml | head -5
```

Expected: no output (history clean).

**Step 4:** Re-add remotes (filter-repo removes them)

```bash
git remote add origin git@github.com:codewiresh/codewire.git
git remote add gitea git@git.noel.sh:codespace/codewire.git
```

---

## Task 3: Force Push to Both Remotes

**Step 1:** Force push to GitHub

```bash
git push origin main --force
```

**Step 2:** Force push to Gitea

```bash
git push gitea main --force
```

**Step 3:** Verify on GitHub that `.gitea/` and `deployments/` directories are gone

```bash
gh api repos/codewiresh/codewire/contents/.gitea 2>&1 | grep -c "Not Found"
gh api repos/codewiresh/codewire/contents/deployments 2>&1 | grep -c "Not Found"
```

Expected: both print `1` (Not Found).

---

## Task 4: Move k8s Manifests to Infra Repo

**Files:**
- Create: `infra/apps/codewire-docs/k8s/` (from codewire `deployments/k8s/`)
- Create: `infra/apps/codewire-demo/k8s/` (new, based on codewire `demo/k8s/` but with git.noel.sh images)

Note: We still have the files locally in codewire even after the push (filter-repo only rewrites history; working tree is untouched).

**Step 1:** Copy docs manifests to infra

```bash
mkdir -p /Users/noel/src/sonica/infra/apps/codewire-docs/k8s
cp /Users/noel/src/sonica/codewire/deployments/k8s/base/* \
   /Users/noel/src/sonica/infra/apps/codewire-docs/k8s/
cp -r /Users/noel/src/sonica/codewire/deployments/k8s/overlays/production/ \
   /Users/noel/src/sonica/infra/apps/codewire-docs/k8s/overlays/production/
```

**Step 2:** Create demo k8s manifests in infra (fix image refs to git.noel.sh)

Create `infra/apps/codewire-demo/k8s/namespace.yaml` — copy from codewire as-is.

Create `infra/apps/codewire-demo/k8s/rbac.yaml` — copy from codewire as-is.

Create `infra/apps/codewire-demo/k8s/broker-deployment.yaml` — copy from codewire but change:
- `image: ghcr.io/codewiresh/codewire-demo-broker:latest` → `git.noel.sh/codespace/codewire-demo-broker:latest`
- `DEMO_IMAGE: ghcr.io/codewiresh/codewire-demo:latest` → `git.noel.sh/codespace/codewire-demo:latest`
- Add `imagePullSecrets: [{name: gitea-registry}]` to pod spec

Copy broker-service.yaml, ingress.yaml, networkpolicy.yaml as-is.

Create `infra/apps/codewire-demo/k8s/registry-secret.yaml`:

```yaml
apiVersion: secrets.infisical.com/v1alpha1
kind: InfisicalSecret
metadata:
  name: gitea-registry-sync
  namespace: codewire-demo
spec:
  hostAPI: https://infisical.noel.sh/api
  resyncInterval: 3600
  authentication:
    universalAuth:
      secretsScope:
        projectSlug: codespace
        envSlug: prod
        secretsPath: /ci
      credentialsRef:
        secretName: infisical-universal-auth
        secretNamespace: codewire-demo
  managedSecretReference:
    secretName: gitea-registry
    secretNamespace: codewire-demo
    secretType: kubernetes.io/dockerconfigjson
    creationPolicy: Owner
    template:
      includeAllSecrets: false
      data:
        .dockerconfigjson: '{"auths":{"git.noel.sh":{"username":"ci","password":"{{ .GITEA_REGISTRY_TOKEN.Value }}","auth":"{{ printf "ci:%s" .GITEA_REGISTRY_TOKEN.Value | b64enc }}"}}}'
```

**Step 3:** Commit and push to infra

```bash
cd /Users/noel/src/sonica/infra
git add apps/
git commit --no-gpg-sign -m "feat: move codewire-docs and codewire-demo k8s manifests from public repo"
git push
```

---

## Task 5: Create Infra Deploy Workflow

**File:** Create `.gitea/workflows/deploy-codewire.yml` in infra repo

This workflow:
1. Checks out codewire from GitHub (public — no auth needed)
2. Builds docs + demo + broker images, pushes to git.noel.sh registry
3. Applies k8s manifests to app cluster

```yaml
name: Deploy Codewire

on:
  workflow_dispatch:
    inputs:
      ref:
        description: 'codewire git ref to deploy (tag or branch)'
        required: false
        default: 'main'

jobs:
  build-and-deploy:
    name: Build Images & Deploy
    runs-on: ubuntu-latest
    steps:
      - name: Checkout infra
        uses: actions/checkout@v4

      - name: Checkout codewire
        uses: actions/checkout@v4
        with:
          repository: codewiresh/codewire
          ref: ${{ inputs.ref || 'main' }}
          path: codewire

      - name: Set up Go
        uses: actions/setup-go@v5
        with:
          go-version-file: codewire/go.mod

      - name: Build cw binary for demo image
        run: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o codewire/demo/cw ./codewire/cmd/cw

      - name: Authenticate to Infisical
        run: |
          RESPONSE=$(curl -s -X POST "${INFISICAL_API_URL}/v1/auth/oidc-auth/login" \
            -H "Content-Type: application/json" \
            -d "{\"identityId\":\"${INFISICAL_IDENTITY_ID}\",\"jwt\":\"$(cat ${INFISICAL_SA_TOKEN_PATH})\"}")
          ACCESS_TOKEN=$(echo "$RESPONSE" | jq -r '.accessToken')
          if [ "$ACCESS_TOKEN" = "null" ] || [ -z "$ACCESS_TOKEN" ]; then
            echo "Infisical auth failed"; echo "$RESPONSE"; exit 1
          fi
          echo "INFISICAL_TOKEN=$ACCESS_TOKEN" >> $GITHUB_ENV

      - name: Login to Gitea registry
        run: |
          REGISTRY_TOKEN=$(curl -s "${INFISICAL_API_URL}/v3/secrets/raw/GITEA_REGISTRY_TOKEN?workspaceSlug=codespace&environment=prod&secretPath=/ci" \
            -H "Authorization: Bearer ${INFISICAL_TOKEN}" | jq -r '.secret.secretValue')
          echo "$REGISTRY_TOKEN" | docker login git.noel.sh -u ci --password-stdin

      - name: Set up Docker Buildx
        uses: docker/setup-buildx-action@v3
        with:
          driver: docker

      - name: Determine image tag
        id: tag
        run: |
          REF="${{ inputs.ref || 'main' }}"
          echo "tag=${REF}" >> $GITHUB_OUTPUT

      - name: Build and push docs image
        uses: docker/build-push-action@v5
        with:
          context: codewire/docs
          file: codewire/docs/Dockerfile
          push: true
          tags: |
            git.noel.sh/codespace/codewire.sh:${{ steps.tag.outputs.tag }}
            git.noel.sh/codespace/codewire.sh:latest

      - name: Build and push demo image
        uses: docker/build-push-action@v5
        with:
          context: codewire/demo
          file: codewire/demo/Dockerfile
          push: true
          tags: |
            git.noel.sh/codespace/codewire-demo:${{ steps.tag.outputs.tag }}
            git.noel.sh/codespace/codewire-demo:latest

      - name: Build and push broker image
        uses: docker/build-push-action@v5
        with:
          context: codewire/demo/broker
          file: codewire/demo/broker/Dockerfile
          push: true
          tags: |
            git.noel.sh/codespace/codewire-demo-broker:${{ steps.tag.outputs.tag }}
            git.noel.sh/codespace/codewire-demo-broker:latest

      - name: Configure kubectl
        run: |
          TOKEN=$(cat ${APP_TOKEN_PATH})
          kubectl config set-cluster app --server="${APP_K8S_SERVER}" --insecure-skip-tls-verify=true
          kubectl config set-credentials ci-deployer --token="$TOKEN"
          kubectl config set-context app --cluster=app --user=ci-deployer
          kubectl config use-context app

      - name: Deploy docs
        run: |
          cd apps/codewire-docs/k8s
          kustomize edit set image \
            git.noel.sh/codespace/codewire.sh=git.noel.sh/codespace/codewire.sh:${{ steps.tag.outputs.tag }}
          kubectl apply -k .
          kubectl rollout status deployment/codewire-docs -n codewire-prod --timeout=120s
          # restore kustomization.yaml (don't commit the tag change)
          git checkout -- .

      - name: Deploy demo
        run: |
          kubectl apply -f apps/codewire-demo/k8s/namespace.yaml
          kubectl apply -f apps/codewire-demo/k8s/registry-secret.yaml
          # Bootstrap infisical-universal-auth if missing
          kubectl get secret infisical-universal-auth -n codewire-demo 2>/dev/null || \
            kubectl get secret infisical-universal-auth -n codewire-prod -o json | \
            python3 -c "import json,sys; s=json.load(sys.stdin); s['metadata']={'name':s['metadata']['name'],'namespace':'codewire-demo'}; print(json.dumps(s))" | \
            kubectl apply -f -
          kubectl apply -f apps/codewire-demo/k8s/
          kubectl rollout status deployment/demo-broker -n codewire-demo --timeout=120s
```

**Step 1:** Write the workflow file at `infra/.gitea/workflows/deploy-codewire.yml`

**Step 2:** Commit and push

```bash
cd /Users/noel/src/sonica/infra
git add .gitea/workflows/deploy-codewire.yml
git commit --no-gpg-sign -m "feat: add deploy-codewire Gitea workflow (builds images + deploys docs and demo)"
git push
```

---

## Task 6: Apply Demo to App Cluster Now (Bootstrap)

Since the workflow is `workflow_dispatch`, run the initial deploy manually via kubectl to get the demo up immediately.

**Step 1:** Apply namespace + bootstrap infisical auth

```bash
KUBECONFIG=/Users/noel/src/sonica/infra/kubeconfig-app.yaml
kubectl apply -f infra/apps/codewire-demo/k8s/namespace.yaml

# Copy infisical-universal-auth from codewire-prod to codewire-demo
kubectl get secret infisical-universal-auth -n codewire-prod \
  --kubeconfig $KUBECONFIG -o json | \
  python3 -c "
import json, sys
s = json.load(sys.stdin)
s['metadata'] = {'name': s['metadata']['name'], 'namespace': 'codewire-demo'}
print(json.dumps(s))" | kubectl apply --kubeconfig $KUBECONFIG -f -

kubectl apply -f infra/apps/codewire-demo/k8s/ --kubeconfig $KUBECONFIG
```

**Step 2:** Wait for broker to come up

```bash
kubectl rollout status deployment/demo-broker -n codewire-demo \
  --kubeconfig /Users/noel/src/sonica/infra/kubeconfig-app.yaml --timeout=120s
```

**Step 3:** Verify demo endpoint

```bash
curl -s https://demo.codewire.sh/api/health
```

Expected: `{"status":"ok","warm":3,"assigned":0}` (after pool fills ~30s)

---

## Task 7: Remove Sensitive Files from codewire Working Tree

After filter-repo, the files still exist locally (filter-repo only rewrites history). Clean them up.

**Step 1:**

```bash
cd /Users/noel/src/sonica/codewire
rm -rf .gitea deployments demo/k8s
```

**Step 2:** Verify working tree matches the cleaned history

```bash
git status
# Nothing to stage — these paths no longer exist in the index after filter-repo
```

---

## Verification

```bash
# 1. History is clean
git log --all --full-history -- .gitea/ | wc -l   # 0
git log --all --full-history -- deployments/ | wc -l  # 0

# 2. Public repo has no sensitive dirs
gh api repos/codewiresh/codewire/contents/.gitea 2>&1 | grep "Not Found"

# 3. Demo is live
curl -s https://demo.codewire.sh/api/health | jq .

# 4. Docs still live
curl -sI https://codewire.sh/ | grep "200"

# 5. Infra deploy workflow exists
gh api repos/codewiresh/infra/contents/.gitea/workflows/deploy-codewire.yml \
  --hostname git.noel.sh 2>&1 | grep "name"
```
