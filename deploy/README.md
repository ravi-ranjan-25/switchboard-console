# Deploying Switchboard

These manifests are a **reference** for running Switchboard as a Deployment in the cluster it manages (they assume Flux, Traefik, External Secrets
and a `watchtower`-style ops namespace; rename freely). Treat them as an example to copy into your GitOps repo, not as a turnkey install.

* `deployment.yaml`, `service.yaml`, `kustomization.yaml`, `rbac.yaml`: the app. It runs as non-root with a read-only root filesystem, drops all
  capabilities, uses seccomp `RuntimeDefault`, and has a single replica (the pending-change lock is in memory).
* `flux-kustomization.example`: the Flux `Kustomization` that deploys it, with the `postBuild.substitute` variables.
* `image-automation.example.yaml`: optional Flux image automation for Switchboard itself (not recommended: the console that edits the repo should
  not be rolled by the loop it can break; bump the tag by hand).

## Build

`.github/workflows/build.yml` runs `go vet` and `go test -race`, then builds a `linux/arm64` image and pushes
`ghcr.io/<owner>/<repo>:<UTC yyyymmddHHMMSS>-<sha7>` (pull requests only run the tests). Adjust `platforms:` for your nodes. Keep the package private
and pull it with an image pull secret.

## Required

1. **A GitHub App** with *Contents: read & write* and *Metadata: read*, installed on the GitOps repository only. Switchboard makes its commits with it.
   Mount its private key as a file (`GITHUB_PRIVATE_KEY_PATH`) and set `GITHUB_APP_ID`, `GITHUB_INSTALLATION_ID`.
2. **Identity in front of it.** Run it behind an auth proxy that sets `X-Auth-Request-Email` (for example oauth2-proxy via Traefik forwardAuth) and
   set `ALLOWED_EMAILS`; it refuses to start without that list. Restrict who can reach the pod with a NetworkPolicy, because the header is only
   trustworthy if nothing else can talk to the pod directly.
3. **RBAC** (`rbac.yaml`): namespaced Roles only, no ClusterRole. Read Flux `Kustomization`s, `ImageRepository`/`ImagePolicy`, pods, pod logs and
   `ExternalSecret`s; the single write is `patch` on `GitRepository/<name>` to request a fetch.

## Optional features

* **New project** (creates a private repo from a stack template). A GitHub App installation cannot create repositories under a *personal* account,
  so this needs a separate fine-grained personal access token (*Administration, Contents, Workflows: write; Metadata: read*, shortest practical expiry).
  Store it in your secret store, mount it as a file, and set `GITHUB_REPO_TOKEN_PATH`. Without it the option is greyed out. This is the most
  powerful credential in the system (Administration write could also delete repos, although Switchboard has no code path that does); scope and rotate it accordingly.
* **Secrets screen** (OCI Vault via the node's instance principal; adapt `vault.go` for another store). Needs `OCI_VAULT_ID`, `OCI_COMPARTMENT_ID`,
  `OCI_KEY_ID` and an IAM policy for the instance's dynamic group:
  ```
  Allow dynamic-group <group> to read vaults in <compartment>
  Allow dynamic-group <group> to inspect secrets in <compartment>
  Allow dynamic-group <group> to manage secrets in <compartment> where target.secret.name = /app-*/
  Allow dynamic-group <group> to read secret-bundles in <compartment> where target.secret.name = /app-*/
  Allow dynamic-group <group> to use keys in <compartment> where target.key.id = '<master key OCID>'
  ```
  `inspect secrets` is unconditional because a list request has no single target for a name condition; it exposes names and metadata, never values.
  **Creating a secret did not work** in the author's tenancy: OCI answered 404 for `CreateSecret` under the name-scoped grant, under
  `request.permission = 'SECRET_CREATE'` and under an unconditional `manage secrets`. The missing piece is unknown (probably a permission on the master key). Listing, revealing
  and adding versions work. The instance principal belongs to the host, so any pod on that node can use the same grant.

## RBAC at a glance

| Namespace | Grant |
|---|---|
| Flux namespace | `kustomizations` get/list/watch; `imagerepositories`, `imagepolicies` get/list; `gitrepositories/<name>` **patch** (the only write) |
| app namespaces | `pods` get/list, `pods/log` get, `externalsecrets` get/list |
| cluster-wide | nothing |
