# ADR-000X: Multi-host libvirt — a "libvirt cluster" provider architecture

## Status

**Proposed (2026-06-24) — RFC / draft for maintainer review.** This document deliberately
**records no decision**: it is an RFC in ADR form — it frames a choice and gathers input. Once the
maintainers choose a direction, it should be revised into a decision-recording ADR (chosen option
+ rationale + resolved questions), with the unchosen options kept as alternatives.

> **Key Value:** libvirt has no cluster platform (no "vCenter"). Reaching capability parity with
> vSphere and Proxmox requires building the **cluster-management layer** that libvirt lacks —
> host inventory, scheduling, intra-cluster migration, per-host maintenance. This ADR frames
> **how that layer should be structured** and leaves the decision to the maintainers.

**Author**: Komh ([@jing2uo](https://github.com/jing2uo))

**Tracking**: [#257](https://github.com/projectbeskar/virtrigaud/issues/257).

**Related**:
- [ADR-0001](./0001-transport-grpc-and-capi-integration.md) — gRPC is the
  manager↔provider transport; product capabilities are built on VirtRigaud's **CRDs**, not the
  gRPC contract. None of the options below changes the gRPC contract.
- [ADR-0006](./0006-storage-backend-agnostic-cross-hypervisor-migration.md) / #236 — the
  `ExportDisk`/`ImportDisk` relay is reused for **intra-cluster cold migration**.

---

## Context

### The current model, traced through the live code

vSphere and Proxmox front many hosts through an **external platform**, and the VirtRigaud
provider is a thin client to it. libvirt has no such platform — a standalone `libvirtd` is a
single host. Laying the stack out by responsibility (top = the VirtRigaud-facing request layer;
bottom = where VMs actually run):

| Layer (responsibility) | vSphere | Proxmox | libvirt (today) |
|---|---|---|---|
| **Provider** (VirtRigaud request layer) | thin; govmomi → vCenter API | thin; REST → PVE (disk over SSH) | today's libvirt provider, **directly bound to one `libvirtd`** |
| **Cluster mgmt** (inventory / scheduling / migration / HA / aggregation) | vCenter | PVE cluster (corosync / ha-manager / `qm migrate`) | **❌ none** |
| **Host mgmt** (single-host VM ops / state) | hostd / vpxa | pve-daemon @ node | `libvirtd` (single host) |
| **Virtualization** (runs the VM) | ESXi → qemu | node → qemu | `libvirtd` + qemu |

The **Cluster-mgmt** row is the whole story: vSphere/Proxmox get it for free from vCenter/PVE;
**libvirt's is empty**, and because there is no cluster layer, the libvirt provider is forced to
bind directly to a single `libvirtd` (provider and host-management collapsed into one).
*(The vCenter/PVE capabilities in that row are external product facts, not from this repo; the
libvirt column and everything below is verified in code.)*

Verified against `origin/main` / `v0.3.11`:

- **One Provider = one host.** `Provider.spec.endpoint` is a single URI
  (`api/infra.virtrigaud.io/v1beta1/provider_types.go:227`); there is no multi-host/nodes field.
- **No cluster layer exists anywhere in the project.** `internal/controller` has no
  scheduler/placement controller; `VMPlacementPolicy` is a fully-specced CRD with **no
  controller** (`api/.../vmplacementpolicy_types.go`); `VMSet` is an explicit stub
  (`"VMSet has no active controller in this release (#179)"`).
- **vSphere delegates placement to vCenter/DRS.** The clone `RelocateSpec` sets
  `Datastore`+`Pool` but **not** a host; `placement.Host` is parsed into `spec.Host` and logged
  but never sent to vCenter (`internal/providers/vsphere/server.go:2217`) — so even the host hint
  is effectively a no-op today.
- **Proxmox picks a node naively.** `FindNode()` returns `NodeSelector[0]` or `nodes[0]` —
  literal first-node, no capacity awareness (`internal/providers/proxmox/pveapi/client.go:793`).
- **No native migration anywhere.** The provider gRPC has 15 RPCs and **no `Migrate`**
  (`proto/provider/v1/provider.proto`); migration is manager-orchestrated
  `ExportDisk → relay → ImportDisk`.

→ The gap is precisely the **cluster-management layer**, and the project has never built one for
any provider.

### Not in scope (why not an existing stack)

This concerns managing **external libvirt hosts** — VirtRigaud's existing premise — not replacing
them with KubeVirt, nor adopting a heavyweight external cluster manager (oVirt/OpenStack). The
layer discussed here is a lightweight, VirtRigaud-native one.

---

## Decision

**Deferred to the maintainers.** Capability parity means filling the empty *Cluster-mgmt* row.
The four capability layers are fixed; the open choice is **how that layer is packaged, and what
today's libvirt provider becomes**. Three candidates, presented objectively.

Common to all three: the four layers are fixed; the scheduler, inventory, and migration
orchestration are **net-new in every option**; and **none changes the shared gRPC contract**,
because host selection happens at or below the provider, never as a `manager→provider` field
(so vSphere/Proxmox are unaffected — consistent with ADR-0001).

### Option 1 — Monolithic provider (simplest)

One provider component holds all host connections **plus** the cluster layer, connecting directly
to each `libvirtd`. *Today's libvirt provider grows into this one component.* This is the **most
literal reading of "one provider, many hosts."**

- **For**: fewest components; fastest to a first version; no shared-proto change (the scheduler
  lives inside the provider).
- **Trade-off — single point of failure**: that one pod is the failure domain for the whole
  cluster (it holds the cluster brain **and** every host connection), and it requires rewriting
  the provider's single-connection model into an N-connection pool with per-host
  health/reconnect. No per-host isolation.

### Option 2 — Cluster runtime + host agents (middle)

A new **`LibvirtCluster` runtime** (the manager-facing provider **and** the cluster layer) routes
to **host agents**, one per host. *Today's libvirt provider becomes the host agent* — single
host, single connection, the natural home for #257's native client; the cluster layer is a new
VirtRigaud component on top.

- **For**: per-host failure isolation (a host agent's crash affects one host; the cluster runtime
  restarting does not drop host connections); reuses today's provider as the host agent; the
  manager still sees one provider; no shared-proto change.
- **Trade-off — least consistent with the project's provider model**: the vSphere and Proxmox
  providers are *thin* clients to an **external** platform (vCenter/PVE) and contain **no** cluster
  management. Here VirtRigaud builds the cluster layer as its **own** component, so libvirt's
  "provider" carries cluster management that the other two delegate outward — an asymmetry in how
  providers are structured. Costs N+1 pods + one in-cluster hop, and the cluster runtime is itself
  a control-plane SPOF for cluster operations (host agents keep running).

### Option 3 — Thin provider + standalone cluster platform (most layered)

A genuinely **thin provider** (a true peer of the vSphere/Proxmox providers) requests a **separate
libvirt-cluster platform** that owns the cluster layer; host agents sit below it. *Today's libvirt
provider becomes a thin adapter.*

- **For**: **functionally the closest** to treating vSphere/PVE's structure as the template — a
  thin provider in front of a separate cluster platform, exactly as the vSphere provider sits in
  front of vCenter. Most faithful to "the platform is part of the architecture."
- **Trade-off — over-engineering / scope**: that separate platform is effectively a **standalone
  product** (its own API, deployment, lifecycle, and **state store — a second source of truth
  alongside the K8s CRDs; see *Long-term considerations*)** — two stacked control planes for what
  is, within VirtRigaud, one provider's feature. (Note: Option 2 → Option 3 is a clean later
  evolution, so a smaller start does not foreclose it.)

### Resource model (sketch — mainly relevant to Options 2/3, pending the direction)

New CRDs would likely be **`LibvirtCluster`** (the cluster the manager sees) and **`LibvirtHost`**
(one host, modeled like a Kubernetes Node — `unschedulable`/cordon, `taints`, `overcommit{cpu}`;
`status` carries `capacity`/`allocatable`/`conditions`). The scheduler would consume the existing
**cross-provider** `VMPlacementPolicy` (host-selection subset), giving it its first controller.
`VirtualMachine` would gain `spec.clusterRef` + `status.hostRef`; `spec.providerRef` is retained
for the single-host path. (Option 1 could instead carry the host list in the Provider spec.)

---

## Consequences (shared by all options)

- The project gets its **first real scheduler**; libvirt reaches cluster parity; per-host
  maintenance (cordon / drain / overcommit) via a Node-like model; `VMPlacementPolicy` gets its
  first controller (a cross-provider pattern, later generalizable to Proxmox's first-node stub).
- The single-host path stays backward-compatible.
- Option-specific consequences are the trade-offs listed under each option above (SPOF for 1;
  N+1 pods/hop + provider-model asymmetry for 2; standalone-product scope for 3).

---

## Phasing (illustrative, for the host-agent options 2/3; host agent = today's provider, unchanged)

1. Cluster runtime forwarding to a **single** host agent — proves the shape; behavior-neutral.
2. **+ inventory** (N host agents) **+ scheduler** (consumes `VMPlacementPolicy` host-selection
   subset) — multi-host create.
3. **+ migration orchestration** (cordon → drain → cold `ExportDisk`/`ImportDisk` between host
   agents) — node-maintenance loop.
4. **+ HA, native live migration** (`virsh migrate --live`, host-to-host data plane), resource
   pools, cluster networking — later slices.

(Option 1 would instead start with the provider single-connection → N-connection-pool refactor.)

---

## Long-term considerations

These shape which option is right *over time*, beyond the MVP:

1. **The choice is coupled to how far the cluster layer is meant to go.** "Cluster management" is
   not one capability but a multi-year surface (inventory → scheduling → migration → HA/fencing →
   storage → network → resource pools → multi-tenancy). The options age differently: **Option 1's
   debt compounds** as the layer grows (one pod accumulates everything, widest blast radius);
   **Option 3's "over-engineering" becomes "right-sizing"** as the layer grows (a large cluster
   surface is naturally its own product); Option 2 sits between and is pushed toward Option 3 as
   the surface grows. **Deciding the packaging without a stance on the intended ambition is half a
   decision.**
2. **Source of truth (sharpens Option 3).** Options 1/2 keep a single source of truth — Kubernetes
   CRDs/etcd, reconciled from observed state. Option 3's standalone platform has its **own** state
   store: two control planes and two state stores to keep in sync — a long-term
   drift/reconciliation hazard.
3. **Is scheduling per-provider or project-wide?** This design puts the scheduler in the libvirt
   cluster layer — libvirt-local. vSphere/Proxmox delegate scheduling to their external platform,
   so a shared scheduler does not fit them today; but libvirt's is VirtRigaud's **first**, and
   long-term it could either seed a cross-provider scheduler hoisted above providers or stay
   libvirt-local. Moving it later is costly, so decide deliberately (deeper than Q4, which is only
   about the policy CRD).
4. **Host heterogeneity is the norm, not an edge case.** N hosts will have differing libvirt/qemu
   versions and capabilities. Per-host capabilities should be first-class in the model
   (`LibvirtHost.status`), and scheduling/migration must respect them rather than assume a uniform
   cluster.
5. **Scheduling is not cleanly separable from storage and network.** Correct placement needs a
   host's storage and network availability; treating the scheduler (early) and storage/network
   (later) as independent is likely a false decomposition — the cluster model should integrate them
   earlier than a naive slicing suggests.
6. **Live migration imposes cluster-wide invariants.** Though a later slice, native live migration
   needs migration domains / compatible host groups / shared (or block-migratable) storage /
   reachable migration ports. The MVP cluster CRDs should leave room for these so they are not a
   rewrite later.

---

## Open questions (for the maintainers to decide)

1. **First, how far is the cluster layer meant to go?** A minimal scheduler + cold migration, or a
   path toward a full PVE-class cluster (HA/fencing, live migration, storage/network, resource
   pools, multi-tenancy)? This largely drives the packaging (see *Long-term considerations* §1).
   **Then: which option — 1, 2, or 3?**
2. **Is `LibvirtHost` a first-class CRD or internal to the cluster?** *k8s-native* (CRD;
   `kubectl cordon/drain` + RBAC; great ergonomics, but you don't `kubectl` an ESXi host) vs
   *platform-encapsulated* (owned by the cluster, managed via its API/console; most faithful to
   vCenter/PVE). A middle path projects host inventory as read-mostly CRDs. Sets the API surface.
3. **One epic with #257?** The native per-host libvirt client is exactly the host agent's
   connection.
4. **Cross-provider policy coupling** — consume the cross-provider `VMPlacementPolicy` now
   (host-selection subset), or start with a cluster-local policy and adopt it once its
   cross-provider semantics settle?
5. **Migration sequencing** — cold first (reuse Export/Import), native intra-cluster live
   migration as a later slice — acceptable?

---

## References

- [#257](https://github.com/projectbeskar/virtrigaud/issues/257) — tracking issue.
- ADR-0001 (transport/layering), ADR-0006 / #236 (migration relay reused).
- `VMPlacementPolicy` (`api/infra.virtrigaud.io/v1beta1/vmplacementpolicy_types.go`).
- Current-state code citations inline in *Context*.
