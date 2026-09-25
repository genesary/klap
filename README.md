# 👏 klap

[![Status](https://img.shields.io/badge/status-WIP-orange)](https://github.com/Ripolin/klap)
[![Go](https://img.shields.io/badge/Go-1.26-blue?logo=go)](go.mod)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

**klap** is a Kubernetes operator that declaratively manages LDAP directory entries via custom resources.

## Overview

`klap` reconciles Kubernetes custom resources with remote LDAP directories. Define your directory entries as Kubernetes objects and let the operator handle creates, updates and deletes.

Two CRDs are provided:

| CRD      | Purpose                                           |
| -------- | ------------------------------------------------- |
| `Server` | Centralizes LDAP connection and TLS configuration |
| `Entry`  | Declares a single LDAP directory entry to manage  |

**Key features:**
- Declarative LDAP entry lifecycle (create, update, prune on delete)
- Namespace-scoped access control per Server (name regex or label selector)
- Controlled adoption of pre-existing entries (`adopt`)
- Ownership-verified deletion (GUID/DN match before pruning)
- TLS and StartTLS support with custom CA bundles
- DN validation via admission webhooks
- Supports OpenLDAP and Active Directory *(beta)*
- Remote entry UUID/GUID tracked in resource status
- Helm chart available as OCI artifact

## Installation

### Prerequisites

- Kubernetes cluster with admission webhook support
- [cert-manager](https://cert-manager.io/) (required for webhook certificates)
- Helm 3

### Via Helm (recommended)

```sh
helm install klap oci://ghcr.io/ripolin/helm/klap --version <version> \
  --namespace klap-system \
  --create-namespace
```

### Via kubectl

Apply the consolidated manifest from a tagged release (bundles the CRDs, RBAC,
webhooks and controller):

```sh
kubectl apply -f https://raw.githubusercontent.com/ripolin/klap/<version>/dist/install.yaml
```

### Uninstall

```sh
helm uninstall klap --namespace klap-system                                              # Helm
kubectl delete -f https://raw.githubusercontent.com/ripolin/klap/<version>/dist/install.yaml  # kubectl
```

> Deleting the CRDs removes all `Server` and `Entry` resources. Entries with
> `prune: true` will have their LDAP objects deleted as their finalizers run, so
> remove or set `prune: false` on any Entry whose directory object must survive
> before uninstalling.

> ⚠️ `kubectl delete -f install.yaml` tears down the controller **and** the CRDs
> in one apply, with no ordering guarantee. If the controller Deployment is
> removed before the Entries, their finalizers can no longer run: the Entries get
> stuck in `Terminating` and the CRD deletion hangs. Uninstall in order instead —
> delete (or reconcile) all Entries first, let their finalizers clear, then remove
> the operator:
>
> ```sh
> kubectl delete entries --all --all-namespaces   # let finalizers run while the controller is still up
> kubectl delete -f https://raw.githubusercontent.com/ripolin/klap/<version>/dist/install.yaml
> ```

## Quick start

### 1. Create a Server

```yaml
apiVersion: klap.ripolin.github.com/v1alpha1
kind: Server
metadata:
  name: ldap-server
  namespace: default
spec:
  url: ldap://ldap.openldap.svc.cluster.local
  baseDN: dc=example,dc=org
  bindDN: cn=admin,dc=example,dc=org
  passwordSecretRef:
    name: ldap-passwd
  implementation: openldap  # or activedirectory (beta)
  startTLS: false
```

### 2. Create the bind password Secret

```sh
kubectl create secret generic ldap-passwd \
  --from-literal=password=<your-password>
```

### 3. Declare an Entry

```yaml
apiVersion: klap.ripolin.github.com/v1alpha1
kind: Entry
metadata:
  name: joe
  namespace: default
spec:
  dn: cn=joe,dc=example,dc=org
  prune: true       # delete LDAP entry when this resource is deleted
  force: false      # allow destructive attribute changes
  adopt: true       # take over a pre-existing LDAP entry with the same DN
  attributes:
    objectClass:
      - inetOrgPerson
    sn:
      - Doe
    mail:
      - joe@example.org
  serverRef:
    name: ldap-server
    namespace: default
```

Once applied, `klap` creates or updates the corresponding entry in the LDAP directory. The remote entry UUID is stored in `status.guid`.

### Observing state

Each `Entry` carries an `Available` status condition reflecting the last
reconcile, surfaced as the `AVAILABLE` column:

```sh
kubectl get entries
# NAME   DN                    SERVER        AVAILABLE   AGE
# joe    cn=joe,dc=example...  ldap-server   True        2m
```

- `True` — reconciled successfully.
- `False` — reconcile failed (e.g. DN outside the base tree, bind failure); the
  condition `message` carries the reason.
- `Unknown` — an error occurred after the entry had already been created, so its
  remote state is uncertain.

Use `kubectl describe entry <name>` to read the full condition message.

## CRD Reference

### Server

| Field                    | Type              | Default    | Description                                               |
| ------------------------ | ----------------- | ---------- | --------------------------------------------------------- |
| `spec.url`               | string            | —          | LDAP URL (`ldap://` or `ldaps://`)                        |
| `spec.baseDN`            | string            | —          | Base DN for searches                                      |
| `spec.bindDN`            | string            | —          | Bind DN for authentication                                |
| `spec.passwordSecretRef` | SecretRef         | —          | Secret containing the `password` key                      |
| `spec.implementation`    | enum              | `openldap` | `openldap` or `activedirectory` *(beta)*                  |
| `spec.tlsSecretRef`      | SecretRef         | —          | Secret with `ca.crt` for custom CA trust                  |
| `spec.startTLS`          | bool              | `false`    | StartTLS on plain `ldap://` (admission warns if left off) |
| `spec.allowedNamespaces` | NamespaceSelector | —          | Restrict which namespaces' Entries may use this Server    |
| `spec.timeout`           | Duration          | `30s`      | Maximum duration for any operation against the server     |

#### Restricting which namespaces may use a Server

By default an `Entry` may only reference a `Server` located in its **own
namespace**. Set `spec.allowedNamespaces` to grant access to Entries in other
namespaces. An `Entry` is granted access when **any** of these is true:

- it lives in the **same namespace** as the `Server` (always allowed);
- its namespace **name** matches the `namePattern` regular expression (anchored to the full name);
- its namespace carries **labels** matching `labelSelector`.

```yaml
apiVersion: klap.ripolin.github.com/v1alpha1
kind: Server
metadata:
  name: ldap-server
  namespace: ldap-system
spec:
  # ...
  allowedNamespaces:
    # Allow any namespace whose name starts with "team-"
    namePattern: "team-.*"
    # ...and/or any namespace labelled klap.ripolin.github.com/ldap=true
    labelSelector:
      matchLabels:
        klap.ripolin.github.com/ldap: "true"
```

> When `allowedNamespaces` is omitted, only Entries from the Server's own
> namespace are allowed. When it is set, an Entry from a different namespace that
> matches none of the criteria is rejected and its status reports the error.

> ⚠️ `labelSelector` trusts **namespace labels**. Anyone able to label their own
> namespace could match the selector, so namespace labels used here must be
> controlled by cluster administrators. `namePattern` is harder to self-assign
> since namespace creation is usually more restricted.

#### The bind account

Every `Entry` reconciled against a `Server` acts through the single account
defined by `spec.bindDN` / `spec.passwordSecretRef`. That account's directory
privileges therefore define the **blast radius** of klap: any DN it is allowed
to create, modify or delete is reachable by *any* Entry permitted to use the
Server (see [namespace filtering](#restricting-which-namespaces-may-use-a-server)
and [adoption](#adopting-pre-existing-entries-adopt)).

Follow least privilege on the directory side:

- Grant the bind account write access only to the sub-tree(s) klap is meant to
  manage (e.g. `ou=managed,dc=example,dc=org`), not to the whole directory.
- Keep it out of administrative groups; it rarely needs to modify schema,
  ACLs or other accounts' credentials.
- Use a dedicated service account per `Server` so its rights can be scoped and
  audited independently.

> ⚠️ Namespace filtering and `adopt: false` restrict what klap *resources* can
> ask for, but the directory ACLs on the bind account are the real enforcement
> boundary. Do not rely on klap-side controls alone.

### Entry

| Field             | Type                | Default | Description                                                              |
| ----------------- | ------------------- | ------- | ------------------------------------------------------------------------ |
| `spec.dn`         | string              | —       | Distinguished name; must be a descendant of the Server `baseDN`          |
| `spec.prune`      | bool                | `true`  | On resource deletion, delete the LDAP entry (`false` leaves it in place) |
| `spec.force`      | bool                | `false` | Make the spec authoritative over the live LDAP state (see below)         |
| `spec.adopt`      | bool                | `true`  | Take over a pre-existing entry with the same DN (see below)              |
| `spec.attributes` | map[string][]string | —       | LDAP attributes to reconcile                                             |
| `spec.serverRef`  | ResourceRef         | —       | Reference to a `Server` (`namespace` defaults to the Entry's namespace)  |

#### DN must live under the Server baseDN

An `Entry`'s `spec.dn` must be a **descendant of the referenced Server's
`spec.baseDN`**. This is enforced at reconcile time: an Entry whose DN falls
outside the base tree is marked `Available: False` with a message such as
`cn=joe,dc=other,dc=org is not a descendant of dc=example,dc=org`, and no LDAP
operation is attempted.

For example, with a Server `baseDN: dc=example,dc=org`:

| Entry `dn`                            | Result      |
| ------------------------------------- | ----------- |
| `cn=joe,dc=example,dc=org`            | ✅ accepted |
| `cn=joe,ou=people,dc=example,dc=org`  | ✅ accepted |
| `cn=joe,dc=other,dc=org`              | ❌ rejected |
| `dc=example,dc=org` (the base itself) | ❌ rejected |

#### Attribute reconciliation (`force`)

`spec.force` decides whether the Entry's `attributes` are merged into the live
LDAP object or made authoritative over it:

- `force: false` (default) — **additive**. klap only ensures the declared
  attributes and values are present: missing attributes are added, missing values
  are appended. Extra values, and attributes not listed in the spec, are **left
  untouched** — klap never removes anything. Use this when other tools or admins
  may legitimately add data that klap should preserve.
- `force: true` — **authoritative**. The spec becomes the source of truth:
  attributes whose values differ are replaced with the spec's values, and any
  attribute present on the object but absent from the spec is removed (except the
  RDN attribute derived from the DN). The LDAP object is made to match the spec
  exactly. Use this when the Entry must fully own the object's attributes.

#### Adopting pre-existing entries (`adopt`)

When an `Entry` targets a DN that already exists in the directory, `spec.adopt`
controls what happens:

- `adopt: true` (default) — klap takes ownership of the existing entry, records
  its remote UUID/GUID in `status.guid`, and reconciles its attributes from then
  on. On deletion, klap only removes the entry if that same GUID still resolves
  to the expected DN.
- `adopt: false` — reconciliation fails with an *entry already exists* error and
  the object is left untouched. Use this to guarantee an `Entry` never manages a
  DN it did not create.

> ⚠️ Adoption grants an `Entry` control over a directory object klap did not
> create — including the ability to modify it (and, once adopted, to prune it).
> Set `adopt: false` when Entries must be restricted to their own objects.

#### Secret key override

A `SecretRef` has a `name` and an optional `key`; the referenced Secret is always
read from the **Server's own namespace** (unlike `serverRef`, a `ResourceRef`
that carries an explicit `namespace`). By default `passwordSecretRef` reads the
`password` key and `tlsSecretRef` reads `ca.crt`. Both keys can be overridden:

```yaml
spec:
  tlsSecretRef:
    name: ldap-tls
    key: myBundle
```

## Development

### Prerequisites

- Go (see `go.mod`)
- Docker
- `kubectl` and a local cluster (e.g. [kind](https://kind.sigs.k8s.io/))

### Common targets

```sh
make docker-build IMG=<registry>/klap:dev  # build the controller image
make install                               # install CRDs
make deploy IMG=<registry>/klap:dev        # deploy via Kustomize
make helm-deploy IMG=<registry>/klap:dev   # deploy via Helm
make test                                  # run unit tests
make test-e2e                              # run e2e tests (requires a cluster)
make undeploy && make uninstall            # cleanup
```

### Samples

Ready-to-use manifests are available in `config/samples/`.

## Contributing

Issues and pull requests are welcome. Please follow the existing commit convention (used to generate the changelog via [git-cliff](https://git-cliff.org/)).

## License

Apache 2.0 — see [LICENSE](LICENSE).
