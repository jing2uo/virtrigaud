# Spec: the multi-host libvirt Provider under CAPI / multi-provider MachinePools

> Naming note: "cluster" below always means the *capability layer* libvirt lacks
> (inventory / scheduling / migration / maintenance), never a `LibvirtCluster` API
> object — §3.1 deliberately removes that CRD.

**Status**: Draft for maintainer review — a concrete design proposal that revises the
[libvirt-cluster RFC](https://github.com/jing2uo/virtrigaud/blob/docs/adr-libvirt-cluster/docs/adr/000X-libvirt-cluster.md)
in light of wrkode's direction signal on
[#257](https://github.com/projectbeskar/virtrigaud/issues/257):
*"considering the future plans for CAPI and multi-provider `machinePools`"*.

**Author**: Komh ([@jing2uo](https://github.com/jing2uo))

**Inputs synthesized**:
- ADR-0001 — CAPI integration sits on VirtRigaud **CRDs**, never the provider gRPC contract.
- The libvirt-cluster RFC (#257) — three packaging options, previously undecided.
- ADR-0007/0008 + PR #291 — the native single-host, single-connection control transport.
- ADR-0006 / #236 — cross-provider migration (`VMMigration` + the
  `ExportDisk` → PVC/NFS/S3 relay → `ImportDisk` data plane), **shipped**.
- Current API surface: `Provider` (single `spec.endpoint`), `VirtualMachine`
  (`spec.providerRef`, `spec.placement`, `spec.placementRef`), `VMPlacementPolicy`
  (fully specced, no controller), `VMSet` (stub, #179).

---

## 1. What "multi-provider MachinePool" actually demands

CAPI's MachinePool contract is: the infrastructure provider receives `replicas` + an
instance template, **owns pool membership**, and reports provider IDs and ready
replicas. Layered onto VirtRigaud per ADR-0001, a future
`cluster-api-provider-virtrigaud` reconciles a `VirtRigaudMachinePool` and emits
`VirtualMachine` CRs (or a `VMSet`). "Multi-provider" means **one pool's replicas span
multiple `Provider` CRs** — e.g. worker nodes spread across a vSphere cluster *and* a
fleet of libvirt hosts.

Tracing that through the existing architecture yields five requirements. These are the
evaluation criteria for the cluster-layer packaging decision:

- **R1 — Provider selection happens above the providers.** Some component must choose
  *which Provider CR* gets each replica. That decision is cross-provider by definition,
  so it can only live at the manager level (or in the CAPI provider). It must **not**
  be buried inside a libvirt-only component.
- **R2 — Every Provider CR must present as an elastic capacity pool with uniform
  semantics.** The CAPI provider emits identical `VirtualMachine` CRs regardless of
  provider type. vSphere and Proxmox already satisfy this because vCenter/PVE stand
  behind them; libvirt satisfies it only once the empty cluster-mgmt row is filled —
  behind the **same `Provider` abstraction**, not a new manager-facing kind.
- **R3 — Capacity must be legible upward.** Spreading replicas across providers needs
  per-provider capacity/allocatable in CRD `status`, reconciled from observed state.
  A capacity store *outside* etcd is invisible to any manager-level scheduler.
- **R4 — Scale-in must be safe at rate.** MachinePool scale-in deletes `VirtualMachine`
  CRs wholesale. Deletion semantics must be explicit (destroy vs detach — exactly the
  question #286 raises for adopted VMs) and must compose with cordon/drain.
- **R5 — Pool operations are bursty.** Scale N→N+20 is a burst of `Create`/`Describe`
  against many hosts at once. Per-host connection health must be isolated so one slow
  or dead host degrades one host's replicas, not the whole pool (the #290 storm class,
  fleet-wide).
- **R6 — The provider must be a first-class migration endpoint.** Cross-provider
  migration (V2V into the libvirt fleet, out of it, between fleets) is a project goal
  in its own right and is **already shipped** as the manager-orchestrated `VMMigration`
  flow (ADR-0006: `ExportDisk` → PVC/NFS/S3 relay → `ImportDisk`). A multi-host
  libvirt Provider must serve `ExportDisk`/`ImportDisk` with the same semantics the
  single-host provider has today — which forces the data-plane locality design of §5.

## 2. Design principle: two-level placement

The RFC's hardest open question — *"is scheduling per-provider or project-wide?"*
(Long-term §3) — dissolves once placement is split at the natural boundary that R1/R2
expose:

| Level | Decides | Lives | Consumes | Exists today? |
|---|---|---|---|---|
| **A — provider selection** | which `Provider` CR gets a VM | manager (or CAPI provider) | cross-provider subset of `VMPlacementPolicy` | no — CAPI-era work, **not built now** |
| **B — host selection** | which host inside the provider runs it | inside each provider's platform | host-selection subset of `VMPlacementPolicy` | vSphere: vCenter/DRS · Proxmox: PVE · libvirt: **the layer this spec adds** |

Two consequences:

1. **The provider-model asymmetry objection to RFC Option 2 dissolves.** From the
   manager's viewpoint every provider is symmetric: the manager picks a provider; the
   provider's *platform* picks a host. vSphere and Proxmox outsource their platform;
   libvirt embeds its own. Above the `Provider` CR the shape is identical — which is
   precisely what a multi-provider MachinePool needs.
2. **A boundary rule falls out** (consistent with ADR-0001 and the RFC): host selection
   is never a `manager→provider` gRPC field. The host *result* surfaces upward as
   observed state (`VirtualMachine.status`), and host *constraints* flow as declarative
   policy (`VMPlacementPolicy`), not as an imperative parameter.

### 2.1 The same split governs migration

Migration is not one capability but two, and they land on the same two levels:

| Level | Migration | Orchestrator | Status |
|---|---|---|---|
| **A — cross-provider** | vSphere ↔ libvirt ↔ Proxmox (V2V) | the manager's `VMMigration` controller (ADR-0006 relay) | **shipped today** |
| **B — intra-cluster host→host** | drain, rebalance | the libvirt cluster runtime, invisible to the manager — the DRS-vMotion analogue: only `status.hostRef` changes | this spec, Phase 3 |

The boundary rule mirrors placement: **`VMMigration` never names a host; drain never
crosses a Provider.** Level B reuses the same `ExportDisk`/`ImportDisk` data plane
between host agents where storage is not shared, and degrades to a zero-copy
re-define where it is (§5).

## 3. Decision proposal: Option 2, amended for CAPI

**Adopt RFC Option 2 (cluster runtime + host agents), with two CAPI-driven
amendments to the RFC's resource-model sketch.**

### 3.1 Amendment 1 — no `LibvirtCluster` CRD; the `Provider` CR *is* the cluster

The RFC sketched a new manager-facing `LibvirtCluster` CRD plus
`VirtualMachine.spec.clusterRef`. Under R2 that is wrong: a second manager-facing unit
and a second reference field make libvirt a special case exactly where the CAPI
provider needs uniformity. Instead:

- `Provider` (type `libvirt`) gains a multi-host mode: `spec.hostSelector` (label
  selector over `LibvirtHost` CRs in the same namespace). `spec.endpoint` remains valid
  for the single-host path — untouched, backward compatible.
- `VirtualMachine.spec.providerRef` is **unchanged**. The CAPI provider emits the same
  object for vSphere, Proxmox, and libvirt.
- The observed host lands in `VirtualMachine.status.hostRef` (see §3.3 for how the
  binding is made durable), read-only.

**API transition** (today `endpoint`, `credentialSecretRef`, and `runtime` are all
required — `provider_types.go` — and the provider controller unconditionally injects
`PROVIDER_ENDPOINT` from `spec.endpoint`):

- `spec.endpoint` becomes `+optional`; a CEL rule enforces exactly one of
  `endpoint` / `hostSelector` (`has(self.endpoint) != has(self.hostSelector)`), and
  `hostSelector` is rejected for non-libvirt types. Required→optional is a
  backward-compatible CRD loosening; existing objects are unaffected.
- `spec.credentialSecretRef` **stays required** and becomes the fleet-default
  credential; `LibvirtHost.spec.credentialSecretRef` overrides per host. (No second
  field loosening needed.)
- In multi-host mode the controller omits `PROVIDER_ENDPOINT` and the runtime
  configures itself from its own `Provider` CR via the already-injected
  `PROVIDER_NAME`/`PROVIDER_NAMESPACE`.
- **New requirement — RBAC**: the cluster runtime is a controller (it reconciles
  `LibvirtHost`, deploys host agents, writes status), so its ServiceAccount needs
  get/list/watch on `LibvirtHost` + referenced Secrets and create/patch on host-agent
  Deployments. Today's provider pods touch no kube API; this is a deliberate,
  libvirt-runtime-only widening and must be scoped by namespace.

**Deployment model** (one image, two modes): the existing libvirt provider image gains
a host-agent entrypoint (`--mode=host-agent`, one host, one native connection — the
PR #291 binary). `Provider.spec.runtime` keeps deploying exactly one thing — the
manager-facing pod; in multi-host mode that pod is the cluster runtime, which in turn
creates one host-agent Deployment per selected `LibvirtHost` (ownerRef to the
`LibvirtHost`, same image). Host agents are **Kubernetes-managed, not externally
registered endpoints** — external registration would add a new trust/authn surface for
no current need, and host reachability is unchanged (outbound SSH/libssh2 from a pod,
as today).

**Consolidation path for existing single-host Providers** (what fleet operators
actually run today): create the `LibvirtHost` objects, point one multi-host `Provider`
at them, and re-adopt the VMs through the existing adoption flow (`ListVMs`).
Re-adoption is also where `status.ID` moves to the host-qualified format of §3.3, and
where `deletionPolicy: Detach` (§4) protects the machines while the old per-host CRs
are retired. Whether a live `VirtualMachine.spec.providerRef` may instead be patched
in place (avoiding CR re-creation) depends on its immutability rules — open question 5.

### 3.2 Amendment 2 — `LibvirtHost` is a first-class, namespaced CRD

(Resolves RFC open question 2 in the *k8s-native* direction, driven by R3/R4.)

```yaml
apiVersion: infra.virtrigaud.io/v1beta1
kind: LibvirtHost
spec:
  uri: qemu+libssh2://virt@host1/system?known_hosts_verify=...   # ADR-0008 transport
  credentialSecretRef: {name: host1-ssh}
  unschedulable: false          # cordon
  taints: []                    # maintenance windows, dedicated hosts
  overcommit: {cpu: "4"}
status:
  capacity:      {cpu: "64",  memory: 256Gi}
  allocatable:   {cpu: "256", memory: 240Gi}   # after overcommit/reserved
  capabilities:  {libvirt: "8.0.0", qemu: "6.2", kvm: true}  # heterogeneity, RFC §4
  storagePools:                                # observed pools WITH backend identity (§5)
    - {name: default, backend: "nfs:10.0.0.5:/vol/vms", capacity: 2Ti, available: 800Gi}
  preparedImages: [ubuntu-22.04]               # image locality input for the scheduler (§5)
  conditions:    [{type: Ready, ...}]
```

Why a CRD and not platform-internal state: capacity in etcd is what makes R3 possible
(the future Level-A scheduler reads `Provider.status` aggregated from these);
`kubectl cordon`-style maintenance falls out of `spec.unschedulable`; and it keeps the
**single source of truth** property (RFC Long-term §2) that Option 3 would break.

**Capacity semantics** (concrete, since today `Provider.status` only carries
`resourceUsage` stats and the provider contract has no capacity RPC):

- Quantities use `resource.Quantity` / `corev1.ResourceList` semantics (`cpu` in
  cores, `memory` as byte quantities) — same units as Node status.
- `LibvirtHost.status`: `capacity` (host totals), `allocatable`
  (capacity × overcommit − reserved), `allocated` (sum of defined domains' allocation),
  plus `lastHeartbeatTime` set by the cluster runtime on each successful poll.
- `Provider.status.hosts`: `{total, ready, schedulable}` counts;
  `Provider.status.capacitySummary`: `{allocatable, allocated}` = sum over hosts that
  are `Ready` **and** schedulable. A host whose heartbeat is stale (> 3× poll interval)
  or `NotReady` drops out of the sums and sets a `CapacityDegraded` condition on the
  Provider; its already-placed VMs are unaffected (existence/state handling stays the
  normal reconcile path).
- **Deliberately no capacity RPC is added to the provider gRPC contract.** The cluster
  runtime writes `LibvirtHost.status` itself; capacity flows entirely on the CRD plane
  — the same "product surface is CRDs, not gRPC" line ADR-0001 draws. This should be
  stated in the ADR so a capacity verb doesn't creep into the proto later.

### 3.3 VM→host binding: encode the host into the provider ID

The provider contract returns only an opaque `ID` on `Create`, and every later verb
(`Describe`/`Delete`/`Power`/`Reconfigure`/…) takes only that ID. The cluster runtime
must therefore route by ID alone, across restarts — an in-memory VM→host map is not
acceptable state.

Since the ID is already contract-opaque ("provider-specific identifier"), the binding
rides inside it: **`ID = <libvirthost-name>/<domain-uuid>`**.

- The host part is a **routing hint**; the domain UUID is the **identity**. Routing is
  stateless: any runtime replica can route any ID with zero recovery protocol.
- `VirtualMachine.status.hostRef` is a human/scheduler-facing *projection* of the ID
  (surfaced via `DescribeResponse.ProviderRaw`), never an input.
- **Miss handling**: if the domain UUID is absent on the hinted host, the runtime does
  one bounded fleet-wide lookup by UUID; found elsewhere (a future live migration moved
  it) → serve the call and surface the corrected host in `ProviderRaw`; found nowhere →
  `NotFound` (which the manager's delete path already treats as success, keeping
  `Delete` idempotent).
- **Crash windows**: `Create` is already required to be idempotent by name; a runtime
  crash between domain creation and returning the ID is healed by the existing
  create-if-not-exists contract on retry. Single-host mode keeps today's ID format —
  no migration of existing `status.ID` values.
- **Free horizontal scale**: because routing state lives in the ID, the data-plane RPC
  path needs no leader — `Provider.spec.runtime.replicas > 1` scales it horizontally,
  and only the controller loops (`LibvirtHost` reconcile, scheduling, drain) take the
  standard leader election. This directly softens the RFC's "cluster runtime is a
  control-plane SPOF" trade-off.

### 3.4 The components

```
CAPI controllers                                     (future, other repo)
  └─ cluster-api-provider-virtrigaud                 (future, other repo)
       └─ emits VirtualMachine CRs (uniform across provider types)
            └─ VirtRigaud manager  ── Level A: provider selection (future)
                 └─ gRPC (existing 15-verb contract, unchanged)
                      └─ libvirt cluster runtime  ── Level B: host selection
                           │    (the manager-facing provider pod for this Provider CR;
                           │     reconciles LibvirtHost; schedules; orchestrates drain)
                           └─ gRPC (the SAME provider contract, reused southbound)
                                └─ host agents, one per LibvirtHost
                                     (today's provider + PR #291 native transport:
                                      single host, single connection, fail-closed)
                                     └─ libvirtd
```

The cluster runtime speaks the **existing provider proto on both sides**: northbound it
is a normal provider to the manager; southbound each host agent is today's provider
binary pointed at one host. No proto change (ADR-0001 holds), maximal reuse of the
conformance harness, and the host agent is exactly the primitive PR #291 is building —
single-host, single-connection, deliberately *not* an in-process multi-host pool
(which would be Option 1's SPOF path).

### 3.5 Why not Options 1 / 3, restated against R1–R6

| | R1 provider-selection above | R2 uniform Provider | R3 capacity in CRDs | R4 safe scale-in | R5 burst isolation | R6 migration endpoint |
|---|---|---|---|---|---|---|
| **Opt 1** monolith | ok | ok | ok | ok | ✗ one pod = whole-fleet failure domain; N-connection pool rebuild; #290 class goes fleet-wide | ok (import lands in the one pod) |
| **Opt 2** (this spec) | ok | ok (amended: no new manager-facing kind) | ok (`LibvirtHost` → `Provider.status`) | ok (drain + deletionPolicy) | ok (per-host agent) | ok (import routed by runtime; staging record in CRDs, §5) |
| **Opt 3** standalone platform | ok | ✗ second control plane behind the provider | ✗ own state store, invisible to manager scheduler | risky (two-phase deletes across two control planes) | ok | ✗ staging/placement record lives in the second state store — one more cross-plane sync with the manager's `VMMigration` flow |

Option 3's "right-sizing at scale" argument (RFC Long-term §1) is not foreclosed:
Option 2 → 3 remains a clean later evolution if the cluster surface ever justifies a
standalone product. Under a CAPI future, though, Option 3's second state store is a
direct R3 violation *today*.

## 4. Scale-in semantics (R4, resolves #286 within this frame)

MachinePool scale-in makes #286 urgent rather than philosophical: CR deletion at rate
must not be able to destroy machines the CR does not "own". Proposal:

- `VirtualMachine.spec.lifecycle.deletionPolicy: Delete | Detach`. Concretely (today
  `VirtualMachineLifecycle` has no such field and the deletion path unconditionally
  calls provider `Delete` whenever `status.ID` is set):
  - **Unset ⇒ `Delete`** — current behavior is preserved exactly; no conversion, no
    defaulting webhook guessing intent.
  - **`Detach`**: the finalizer path skips the provider `Delete` call entirely, emits a
    `VMDetached` event recording the provider ID left behind, and removes the
    finalizer. The hypervisor domain keeps running, eligible for re-adoption.
  - **Adoption flows set `Detach` explicitly at adoption time** (that is where the
    "this CR does not own the machine" intent is known). *Existing* adopted VMs cannot
    be retro-classified safely — the migration note in the release doc tells operators
    to patch them; no automatic backfill.
- The cluster runtime's drain path (cordon → migrate/evacuate → delete) respects the
  same field, so node maintenance and pool scale-in share one deletion semantic.

This is a small, provider-agnostic API addition and is worth deciding once, here,
because every provider type faces it the moment MachinePools exist.

## 5. Data-plane locality: the cluster as a migration target

R6 meets multi-host at the data plane, where four locality couplings must be designed
rather than discovered:

1. **Staging pins placement.** In a cross-provider migration into the fleet,
   `ImportDisk` runs *before* `Create`, and the disk bytes land in one host's storage
   pool — host selection is forced forward to the first data-plane touch. The cluster
   runtime therefore runs the scheduler at `ImportDisk` time, records the staging host
   durably (keyed by the VM identity, surviving runtime restarts — the same
   no-in-memory-state rule as §3.3), and pins the subsequent `Create` to that host.
   `ExportDisk` needs no such design: it routes by the §3.3 ID like any other verb.
2. **Image distribution.** Today `ImagePrepare` targets *the* host; with N hosts the
   MVP answer is **prepare-on-placement** (lazy — first boot on a cold host pays the
   fetch), with prepared images recorded per host in `LibvirtHost.status` and used as
   a soft scheduling preference (image locality). Pre-warming policies can come later
   without a model change.
3. **Shared-storage identity.** `LibvirtHost.status` records not just which pools a
   host has but *which storage they are* (a backend identity — e.g. the export
   path/UUID behind the pool), so two hosts on the same NFS export are recognizably
   on shared storage. That one field makes intra-cluster cold migration a zero-copy
   re-define on shared storage, and is the room native live migration later needs
   (RFC long-term §6) — left out now, it is a model rewrite later.
4. **Clone/snapshot locality.** Linked clones cannot leave their backing storage:
   clone placement defaults to the source host (or its shared-storage group), and
   only a full-copy clone may be scheduled freely. Snapshots are host-local state and
   travel only via the migration path.

## 6. What is deliberately NOT in scope

- **The Level-A cross-provider scheduler.** CAPI-era work in manager (or CAPI-provider)
  territory; this spec only guarantees the libvirt layer doesn't steal its job and
  publishes the capacity it will need (R1/R3).
- **`VMSet` revival / MachinePool mapping** (ADR-0001 open Q3): whether the CAPI
  provider emits N `VirtualMachine`s or one `VMSet` is decided in the future CAPI-provider
  design (ADR-0001 F4). Nothing here constrains it either way.
- **Live migration / HA / fencing.** Later slices per the RFC phasing; the CRD shapes
  above (host capabilities, taints, drain) leave room for them (RFC Long-term §6).
- **Any provider gRPC contract change.** None needed (verified: host selection stays
  below or beside the contract; migration reuses `ExportDisk`/`ImportDisk`, ADR-0006).

## 7. Phasing (each phase independently shippable)

0. **Land the host-agent primitive** — PR #291 and its follow-on phases (ADR-0007
   Phases 1–5: `Validate`, `Describe`, `Reconfigure`, `ListVMs`, lifecycle). Unchanged;
   this spec is the answer to "what will this be the unit *of*" (RFC open Q3: yes,
   one epic with #257).
1. **`LibvirtHost` CRD + cluster runtime skeleton** — `Provider.spec.hostSelector`,
   runtime fans out to host agents, naive placement (first-fit on `allocatable`),
   inventory reconciled into `LibvirtHost.status` (including storage-pool backend
   identity and prepared images, §5). Single-host path untouched.
2. **Host scheduler + maintenance** — consume the host-selection subset of
   `VMPlacementPolicy` (`hosts`/`excludedHosts`/`hostAntiAffinity` — its first
   controller), honor `unschedulable`/taints, cordon.
3. **Drain via cold migration + migration-target routing** — orchestrate
   `ExportDisk → relay → ImportDisk` between host agents (ADR-0006 reuse), zero-copy
   re-define where shared storage is detected (§5.3); inbound `ImportDisk` routing
   with staging-pins-placement (§5.1); `deletionPolicy` lands here (with #286).
4. **Capacity aggregation upward** — roll `LibvirtHost.status` up into
   `Provider.status`; burst-create hardening (R5 verification under pool-scale load).
5. **Later** — native live migration, HA, resource pools (per RFC).

## 8. Open questions for maintainers

1. Does the **two-level placement boundary** (§2) match where you see CAPI and
   multi-provider MachinePools going — i.e. provider selection at manager/CAPI level,
   host selection inside the provider platform?
2. **Amendment 1** removes the RFC's `LibvirtCluster` CRD in favor of extending
   `Provider`. Agreed, or do you want a distinct kind for the cluster even at the cost
   of a libvirt-special manager surface?
3. Is `deletionPolicy` (§4) acceptable as a **provider-agnostic** `VirtualMachine`
   field now, given both #286 and future scale-in need it?
4. Should `VMPlacementPolicy`'s first controller (Phase 2) be scoped libvirt-local as
   proposed, with the cross-provider subset explicitly reserved for Level A?
5. For consolidating existing single-host Providers (§3.1): is patching a live
   `VirtualMachine.spec.providerRef` acceptable (is it mutable today, and should it
   be?), or is re-adoption under the new Provider the only supported path?
