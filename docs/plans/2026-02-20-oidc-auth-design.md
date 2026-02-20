# OIDC Authentication for Codewire Relay

**Goal:** Replace GitHub OAuth with standard OIDC (Codespace.sh Dex IdP) for both the relay admin UI and node registration via device flow.

**Date:** 2026-02-20

---

## Overview

Add `authMode: "oidc"` to the relay. A single OIDC client registered in Codespace.sh Dex serves two flows:

- **Admin web UI** — OIDC authorization code flow (browser login)
- **Node registration** — RFC 8628 device authorization flow, proxied through the relay

Existing `token` and `none` modes are unchanged (backwards compat).

---

## Auth Flows

### Admin Web UI (authorization code flow)

1. User visits relay URL → redirected to Codespace.sh Dex authorize endpoint
2. User authenticates at Dex (Gitea upstream)
3. Callback to `/auth/oidc/callback` with code
4. Relay exchanges code for ID token, calls userinfo endpoint
5. Validates `groups` claim against `allowedGroups` config
6. Creates session cookie (`cw_session`), stores in SQLite sessions table
7. Session uses `sub` (OIDC subject) as the user identifier

### Node Registration (device flow via `cw setup`)

```bash
# OIDC relay — no token needed
cw setup https://user.relay.codespace.sh

# Token/invite relay — token as second positional arg (unchanged)
cw setup https://user.relay.codespace.sh <invite-token>
```

1. `cw setup <relay-url>` calls `GET /api/v1/auth/config` (unauthenticated)
2. Response: `{"auth_mode": "oidc"}` → device flow path
3. CLI calls `POST /api/v1/device/authorize` on relay
4. Relay initiates device auth with Dex, stores `device_code` in SQLite
5. Relay returns `user_code` + `verification_uri` to CLI
6. CLI prints: `Open https://auth.codespace.sh/activate and enter code: WXYZ-1234`
7. CLI polls `POST /api/v1/device/poll` (passes relay's opaque poll token)
8. Relay polls Dex token endpoint until approved
9. Relay validates `groups` claim on the issued token
10. Relay issues a node token, stores it in `device_flows` table
11. CLI receives node token, saves to `~/.codewire/config.toml`

The OIDC client secret never leaves the relay.

---

## Access Control

`allowedGroups []string` — configurable list of OIDC `groups` claim values.

- Empty list → any authenticated Codespace.sh user is allowed
- Non-empty → user must be a member of at least one listed group
- Matches the pattern already used for k8s RBAC in the infra (same `groups` claim from Dex/Gitea)

---

## API Changes

New endpoints when `authMode == "oidc"`:

| Method | Path | Auth | Purpose |
|--------|------|------|---------|
| `GET` | `/api/v1/auth/config` | none | Returns `{"auth_mode":"oidc"}` |
| `GET` | `/auth/oidc` | none | Redirect to Dex authorize |
| `GET` | `/auth/oidc/callback` | none | Exchange code → session cookie |
| `POST` | `/api/v1/device/authorize` | none | Initiate device flow |
| `POST` | `/api/v1/device/poll` | none | Poll device flow status |

Existing endpoints unchanged. GitHub-specific endpoints (`/auth/github/*`) removed when `authMode == "oidc"`.

`GET /auth/session` reused as-is.

---

## Config

New `relay.Config` fields:

```go
OIDCIssuer        string   // e.g. https://auth.codespace.sh
OIDCClientID      string
OIDCClientSecret  string
OIDCAllowedGroups []string // e.g. ["sonica"]
```

OIDC endpoints discovered automatically from `<issuer>/.well-known/openid-configuration` at relay startup.

---

## Store Changes

### Schema changes

**`users` table** — `github_id INTEGER` → `sub TEXT` (OIDC subject claim)

**`sessions` table** — `github_id` FK → `sub TEXT`

**`github_app` table** — dropped

**New `device_flows` table:**

```sql
CREATE TABLE device_flows (
    device_code  TEXT PRIMARY KEY,
    user_code    TEXT NOT NULL,
    poll_token   TEXT NOT NULL UNIQUE,  -- opaque token given to CLI
    expires_at   DATETIME NOT NULL,
    node_token   TEXT                   -- populated when approved
);
```

---

## Code Changes

### New / changed files

| File | Change |
|------|--------|
| `internal/oauth/oidc.go` | New — OIDC discovery, auth code flow, device flow, token validation |
| `internal/oauth/github.go` | Kept but only used when `authMode == "github"` (deprecated) |
| `internal/relay/relay.go` | Add `oidc` branch in mux setup; add `/api/v1/auth/config` |
| `internal/relay/setup.go` | `cw setup` gains device flow path; positional args `<url> [token]` |
| `internal/store/store.go` | `User.GitHubID` → `User.Sub`; add device flow CRUD |
| `internal/store/sqlite.go` | Schema migration; new device_flows table |
| `internal/config/config.go` | Add OIDC fields |
| `operator/api/v1alpha1/types.go` | Add `OIDCSpec` |
| `operator/api/v1alpha1/zz_generated.deepcopy.go` | Regenerate |
| `operator/internal/controller/codewirerelay_controller.go` | Mount OIDC secret, pass env vars |
| `charts/codewire-relay/values.yaml` | Add `oidc:` block |
| `charts/codewire-relay/templates/deployment.yaml` | Mount OIDC secret |
| `infra/components/terraform/dex/main.tf` | Add `codewire-relay` static client |

---

## Infra

**Dex** (`infra/components/terraform/dex/main.tf`):
- Add `codewire-relay` to `static_clients`
- Generate client secret via `random_password`
- Store in Infisical at `/app/codewire-relay/OIDC_CLIENT_SECRET`

**Operator** (`operator/api/v1alpha1/types.go`):
```go
type OIDCSpec struct {
    Issuer          string       `json:"issuer"`
    ClientID        string       `json:"clientID"`
    ClientSecretRef SecretKeyRef `json:"clientSecretRef"`
    AllowedGroups   []string     `json:"allowedGroups,omitempty"`
}
```
Added as optional field `OIDC *OIDCSpec` on `CodewireRelaySpec`.

---

## Backwards Compatibility

- `authMode: "token"` and `authMode: "none"` unchanged
- `authMode: "github"` kept but marked deprecated
- Invite tokens still work alongside OIDC (relay accepts both)
- `cw setup <url> <token>` still works for token-mode relays
