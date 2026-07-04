# Libvirt Multi-Host Provider Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Take the libvirt provider from "virsh-over-SSH, one host" to the spec's end state — native SDK control plane, `LibvirtHost` CRD, multi-host `Provider` with a cluster runtime, scheduling, capacity, and migration-target routing — in independently shippable, small-blast-radius steps.

**Architecture:** Three arcs, strictly ordered. **M1** finishes the virsh→native control-plane migration verb-by-verb behind the existing `LIBVIRT_CONTROL_TRANSPORT` gate (ADR-0007 Phases 3–5). **M2** lands the API groundwork (`deletionPolicy`, `LibvirtHost` CRD, `Provider.spec.hostSelector`, host-qualified IDs) with zero behavior change for existing users. **M3–M5** build the cluster runtime on top: same provider image in a `cluster` mode that deploys one host-agent (today's provider) per `LibvirtHost` and routes the existing gRPC contract by host-qualified ID; then inventory + scheduler + capacity aggregation; then drain and migration-target locality.

**Tech Stack:** Go 1.26, `libvirt.org/go/libvirt` (CGO, behind `//go:build libvirt_native`), controller-runtime + envtest, kubebuilder CRD markers + CEL, existing `internal/transport/grpc` client (reused southbound), kind for E2E.

**Spec:** `SPEC-libvirt-multihost-provider.md` (repo root). **Code baseline:** branch `feat/libvirt-native-transport` (= PR #291: seam in `transport.go`, native `Validate`/`Describe` with keepalive+redial in `transport_native.go`, fail-closed gate in both constructors).

## Global Constraints

- **Default transport stays `virsh`.** Every M1 task is inert unless `LIBVIRT_CONTROL_TRANSPORT=native`. Flipping the default is OUT OF SCOPE (gated on password-auth parity, ADR-0008 Consequences).
- **No provider gRPC/proto changes anywhere in this plan** (ADR-0001; spec §3.2 "deliberately no capacity RPC").
- **CGO isolation:** anything importing `libvirt.org/go/libvirt` lives in `//go:build libvirt_native` files. The default `go build ./...` / `go test ./...` must always pass on a machine without `libvirt-dev`.
- **Native transport error semantics:** transport-down or lookup failure returns an error — **never** `Exists:false` / silent fallback to virsh (ADR-0007 §3/§5).
- **Data plane stays on SSH** (disk staging, qemu-img, cloud-init ISO, SELinux relabel) — ADR-0007 §1. M1 migrates control verbs only.
- **Single-host behavior is frozen:** existing `Provider.spec.endpoint` users see no change in any task. Multi-host is opt-in via `spec.hostSelector`.
- **ID format:** single-host mode keeps today's bare-name IDs. Host-qualified IDs (`<libvirthost-name>/<domain-name>`) exist only in cluster mode (spec §3.3; note: name not UUID for M3 parity with existing IDs — UUID upgrade is a recorded follow-up).
- **Verification commands** (run per task):
  - Default build: `go build ./... && go vet ./... && go test ./internal/providers/libvirt/ -count=1`
  - Native compile (no libvirt-dev on the laptop — use the rootful-podman container):
    `podman run --rm -v $PWD:/src:z -v "$(go env GOMODCACHE)":/go/pkg/mod:z -w /src docker.io/library/golang:1.26-trixie bash -c 'apt-get update -qq >/dev/null && apt-get install -y -qq libvirt-dev >/dev/null && go vet -tags libvirt_native ./internal/providers/libvirt/...'`
  - Live (optional, needs a lab host): `LIBVIRT_NATIVE_URI='qemu+ssh://user@host/system?no_tty=1' go test -tags libvirt_native -run TestNative -v ./internal/providers/libvirt/` — run inside the same container with network + creds.
  - CRD/controller tasks: `make test` (envtest), `make manifests generate && git diff --exit-code config/crd` after regeneration is committed.
- **Commits:** conventional style, one commit per task (user convention: squash feat/test/build into one `feat`; ADR/doc changes may ride the same PR as separate `docs` commit). End every commit with `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- **Branches:** M1 → `wip/libvirt-native-lifecycle` stacked on `feat/libvirt-native-transport` (rebase onto main when PR #291 merges; one PR). M2 → one branch/PR per task (`wip/vm-deletion-policy`, `wip/libvirthost-crd`, `wip/provider-hostselector`). M3/M4/M5 → one branch/PR per milestone.

## File Structure (end state)

```
api/infra.virtrigaud.io/v1beta1/
  virtualmachine_types.go        # M2: + DeletionPolicy in VirtualMachineLifecycle
  libvirthost_types.go           # M2: NEW — LibvirtHost CRD
  provider_types.go              # M2: endpoint optional + HostSelector + CEL; M4: status Hosts/Capacity
internal/controller/
  virtualmachine_controller.go   # M2: Detach path in deletion
  provider_controller.go         # M2: omit PROVIDER_ENDPOINT when unset; M3: cluster-mode env + RBAC/SA wiring
internal/providers/libvirt/
  transport.go                   # M1: controlTransport grows one verb per task (non-tagged seam)
  transport_native.go            # M1: nativeConn implementations (build-tagged)
  transport_fake_test.go         # M1: NEW — fake transport + routing tests (non-tagged)
  transport_native_test.go       # M1: live tests grow per verb (build-tagged)
  provider_virsh.go              # M1: each verb routes native-first when gate is on
  hostid.go / hostid_test.go     # M2: NEW — formatHostID/splitHostID
  cluster/                       # M3–M5: NEW package — cluster runtime
    runtime.go                   #   mode entrypoint: ctrl manager + gRPC server
    hostagents.go                #   LibvirtHost reconciler → per-host Deployment/Service
    router.go / router_test.go   #   contracts.Provider implementation routing by host ID
    scheduler.go / _test.go      #   M4: filter+score host picker
    inventory_native.go          #   M4: build-tagged capacity/pool collector (+ non-tag stub)
    drain.go / drain_test.go     #   M5: cordon→evacuate orchestration
cmd/provider-libvirt/main.go     # M3: mode switch (VIRTRIGAUD_LIBVIRT_MODE=cluster)
config/crd/bases/                # M2: + infra.virtrigaud.io_libvirthosts.yaml (generated)
config/rbac/                     # M3: + libvirt cluster-runtime Role/RoleBinding
```

---

# Milestone M1 — Finish the native control plane (virsh replacement, verb by verb)

Each task: extend the `controlTransport` interface → implement on `nativeConn` → route in `provider_virsh.go` (native-first when `p.nativeTransport != nil`) → non-tagged routing test with the fake → tagged live test → commit. The virsh implementation is **kept intact** as the default path; nothing is deleted in M1.

### Task 1: Fake transport + routing-test seam

**Files:**
- Create: `internal/providers/libvirt/transport_fake_test.go`
- Test: same file (this task IS test infrastructure)

**Interfaces:**
- Produces: `fakeTransport` implementing `controlTransport`, with `calls []string` recording, used by every later M1 routing test. Field injection works because tests live in package `libvirt` and `Provider.nativeTransport` is package-private.

- [ ] **Step 1: Write the fake and two routing tests (they pass immediately — this locks in the already-wired Validate/Describe routing as regression tests)**

```go
package libvirt

import (
	"context"
	"testing"

	"github.com/projectbeskar/virtrigaud/internal/providers/contracts"
)

// fakeTransport implements controlTransport for non-tagged routing tests.
// Every method records itself in calls and returns the canned fields.
type fakeTransport struct {
	calls    []string
	describe contracts.DescribeResponse
	err      error
}

func (f *fakeTransport) Validate(ctx context.Context) error {
	f.calls = append(f.calls, "Validate")
	return f.err
}

func (f *fakeTransport) Describe(ctx context.Context, id string) (contracts.DescribeResponse, error) {
	f.calls = append(f.calls, "Describe:"+id)
	return f.describe, f.err
}

func (f *fakeTransport) Close() {}

func newNativeRoutedProvider(ft *fakeTransport) *Provider {
	return &Provider{
		nativeTransport: ft,
		virshProvider:   NewVirshProvider(&ProviderConfig{}),
	}
}

func TestRoutingValidateGoesNative(t *testing.T) {
	ft := &fakeTransport{}
	p := newNativeRoutedProvider(ft)
	if err := p.Validate(context.Background()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(ft.calls) != 1 || ft.calls[0] != "Validate" {
		t.Fatalf("expected native Validate call, got %v", ft.calls)
	}
}

func TestRoutingDescribeGoesNative(t *testing.T) {
	ft := &fakeTransport{describe: contracts.DescribeResponse{Exists: true, PowerState: "On"}}
	p := newNativeRoutedProvider(ft)
	resp, err := p.Describe(context.Background(), "vm1")
	if err != nil || !resp.Exists {
		t.Fatalf("Describe: resp=%+v err=%v", resp, err)
	}
	if ft.calls[0] != "Describe:vm1" {
		t.Fatalf("expected native Describe call, got %v", ft.calls)
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/providers/libvirt/ -run TestRouting -v` — Expected: 2 PASS. (If `Describe` in `provider_virsh.go:765` needs `virshProvider` non-nil before routing, adjust the routing to check `nativeTransport` first — that IS the contract.)
- [ ] **Step 3: Run the full default-build verification** (Global Constraints) — Expected: clean.
- [ ] **Step 4: Commit** — `test(libvirt): fake control transport + routing regression tests`

### Task 2: `Power` goes native

**Files:**
- Modify: `internal/providers/libvirt/transport.go` (interface), `transport_native.go` (impl), `provider_virsh.go:429` (`Power`), `transport_fake_test.go`, `transport_native_test.go`

**Interfaces:**
- Produces: `Power(ctx context.Context, id string, op contracts.PowerOp) error` on `controlTransport`.

- [ ] **Step 1: Add the failing routing test** (fake gains `Power`; without the interface method this does not compile — that is the "red")

```go
func (f *fakeTransport) Power(ctx context.Context, id string, op contracts.PowerOp) error {
	f.calls = append(f.calls, "Power:"+id+":"+string(op))
	return f.err
}

func TestRoutingPowerGoesNative(t *testing.T) {
	ft := &fakeTransport{}
	p := newNativeRoutedProvider(ft)
	if _, err := p.Power(context.Background(), "vm1", contracts.PowerOpOff); err != nil {
		t.Fatalf("Power: %v", err)
	}
	if ft.calls[0] != "Power:vm1:Off" { // adjust literal to the real PowerOpOff string
		t.Fatalf("got %v", ft.calls)
	}
}
```

- [ ] **Step 2: Extend the interface** in `transport.go`:

```go
type controlTransport interface {
	Validate(ctx context.Context) error
	Describe(ctx context.Context, id string) (contracts.DescribeResponse, error)
	Power(ctx context.Context, id string, op contracts.PowerOp) error
	Close()
}
```

- [ ] **Step 3: Implement on `nativeConn`** (`transport_native.go`), mirroring the virsh semantics exactly (`stopDomain` = force `destroy`; reboot = stop+start, not guest-ACPI reboot):

```go
// Power maps the contract power ops onto typed domain calls, with the same
// semantics as the virsh path: Off is a hard stop, Reboot is stop+start.
func (n *nativeConn) Power(ctx context.Context, id string, op contracts.PowerOp) error {
	c, err := n.conn()
	if err != nil {
		return err
	}
	dom, err := c.LookupDomainByName(id)
	if err != nil {
		if isNoDomain(err) {
			return contracts.NewNotFoundError(fmt.Sprintf("domain %q not found", id), err)
		}
		return fmt.Errorf("lookup domain %q: %w", id, err)
	}
	defer func() { _ = dom.Free() }()

	switch op {
	case contracts.PowerOpOn:
		return dom.Create()
	case contracts.PowerOpOff:
		return dom.Destroy()
	case contracts.PowerOpReboot:
		if err := dom.Destroy(); err != nil && !isNotRunning(err) {
			return err
		}
		return dom.Create()
	case contracts.PowerOpShutdownGraceful:
		if err := dom.Shutdown(); err != nil {
			return dom.Destroy() // parity: virsh path falls back to force stop
		}
		return nil
	default:
		return contracts.NewInvalidSpecError(fmt.Sprintf("unsupported power operation: %s", op), nil)
	}
}

// isNotRunning matches VIR_ERR_OPERATION_INVALID ("domain is not running") so
// Reboot/Off on a stopped domain stays idempotent like the virsh path.
func isNotRunning(err error) bool {
	var le libvirtgo.Error
	return errors.As(err, &le) && le.Code == libvirtgo.ERR_OPERATION_INVALID
}
```

- [ ] **Step 4: Route in `provider_virsh.go` `Power()`** — insert at the top, after the nil-check:

```go
	if p.nativeTransport != nil {
		if err := p.nativeTransport.Power(ctx, id, op); err != nil {
			return "", err
		}
		// syncPersistentXML stays on the host path (dumpxml+define, low
		// frequency) — ADR-0007 keeps host-file helpers on SSH.
		if op == contracts.PowerOpOn || op == contracts.PowerOpReboot {
			if syncErr := p.syncPersistentXML(ctx, id); syncErr != nil {
				log.Printf("WARN Failed to sync persistent XML for %s: %v", id, syncErr)
			}
		}
		return "", nil
	}
```

- [ ] **Step 5: Live test** in `transport_native_test.go` — opt-in via a sacrificial domain (`LIBVIRT_NATIVE_TEST_DOMAIN`), skip otherwise: `PowerOpShutdownGraceful` → poll `DescribeBasic` until state leaves "running" (30s cap) → `PowerOpOn` → poll back to running.
- [ ] **Step 6: Run** `go test ./internal/providers/libvirt/ -run TestRouting -v` (PASS) + default-build verification + container native-vet (Global Constraints).
- [ ] **Step 7: Commit** — `feat(libvirt): native Power behind the control-transport gate`

### Task 3: `Reconfigure` (CPU/memory) goes native

**Files:**
- Modify: `transport.go`, `transport_native.go`, `provider_virsh.go:520` (`Reconfigure`), `transport_fake_test.go`

**Interfaces:**
- Produces on `controlTransport`:
  - `DomainInfo(ctx context.Context, id string) (state string, vcpus uint, memKiB uint64, maxMemKiB uint64, err error)`
  - `SetVCPUs(ctx context.Context, id string, count uint, live bool) error`
  - `SetMemoryKiB(ctx context.Context, id string, kib uint64, live bool) error`
- Scope guard: disk-grow (`blockresize`, `qemu-img resize`, guest-fs extension) **stays on the virsh/host path** in this task.

- [ ] **Step 1: Failing routing test** — fake records `DomainInfo`/`SetVCPUs:vm1:4:live`/`SetMemoryKiB:...`; test calls `p.Reconfigure(ctx, "vm1", desired)` with a CPU-only change and asserts the virsh exec path was not needed (fake `DomainInfo` returns `state="running", vcpus=2`).
- [ ] **Step 2: Implement on `nativeConn`:**

```go
func (n *nativeConn) DomainInfo(ctx context.Context, id string) (string, uint, uint64, uint64, error) {
	c, err := n.conn()
	if err != nil {
		return "", 0, 0, 0, err
	}
	dom, err := c.LookupDomainByName(id)
	if err != nil {
		if isNoDomain(err) {
			return "", 0, 0, 0, contracts.NewNotFoundError(fmt.Sprintf("domain %q not found", id), err)
		}
		return "", 0, 0, 0, err
	}
	defer func() { _ = dom.Free() }()
	st, _, err := dom.GetState()
	if err != nil {
		return "", 0, 0, 0, err
	}
	info, err := dom.GetInfo()
	if err != nil {
		return "", 0, 0, 0, err
	}
	return domainStateString(st), uint(info.NrVirtCpu), info.Memory, info.MaxMem, nil
}

func (n *nativeConn) SetVCPUs(ctx context.Context, id string, count uint, live bool) error {
	// flags mirror `virsh setvcpus --live` vs `--config`
	flags := libvirtgo.DOMAIN_VCPU_CONFIG
	if live {
		flags = libvirtgo.DOMAIN_VCPU_LIVE
	}
	return n.withDomain(id, func(dom *libvirtgo.Domain) error {
		return dom.SetVcpusFlags(count, flags)
	})
}

func (n *nativeConn) SetMemoryKiB(ctx context.Context, id string, kib uint64, live bool) error {
	flags := libvirtgo.DOMAIN_MEM_CONFIG
	if live {
		flags = libvirtgo.DOMAIN_MEM_LIVE
	}
	return n.withDomain(id, func(dom *libvirtgo.Domain) error {
		return dom.SetMemoryFlags(kib, flags)
	})
}

// withDomain wraps the lookup/free/NotFound boilerplate shared by the setters.
func (n *nativeConn) withDomain(id string, fn func(*libvirtgo.Domain) error) error {
	c, err := n.conn()
	if err != nil {
		return err
	}
	dom, err := c.LookupDomainByName(id)
	if err != nil {
		if isNoDomain(err) {
			return contracts.NewNotFoundError(fmt.Sprintf("domain %q not found", id), err)
		}
		return err
	}
	defer func() { _ = dom.Free() }()
	return fn(dom)
}
```

(Refactor Task 2's `Power` to use `withDomain` in the same commit — DRY.)

- [ ] **Step 3: Route in `Reconfigure`** — three surgical substitutions, keeping ALL existing decision logic (`hasChanges`, `requiresRestart`, the #203 hotplug-headroom fallback) intact:
  1. the `getDomainState` + `getDomainInfo` prefetch → one `p.nativeTransport.DomainInfo(...)` call when native (populate `isRunning`, current CPUs, current memory from its returns instead of the `currentInfo` map);
  2. every `runVirshCommand(ctx, "setvcpus", id, N, "--live"|"--config")` → `p.nativeTransport.SetVCPUs(ctx, id, N, live)`;
  3. every `runVirshCommand(ctx, "setmem"/"setmaxmem", ...)` → `p.nativeTransport.SetMemoryKiB(ctx, id, kib, live)` (for `setmaxmem` keep the virsh call — max-memory change requires the domain off and is part of the restart branch; leave that branch on virsh and note it with a `// ponytail:` comment).
  Wrap each substitution in `if p.nativeTransport != nil { ... } else { <existing virsh call> }`.
- [ ] **Step 4: Run routing tests + default build + container native-vet.** Expected: PASS.
- [ ] **Step 5: Commit** — `feat(libvirt): native Reconfigure (CPU/memory) behind the gate`

### Task 4: `ListVMs` goes native

**Files:**
- Modify: `transport.go`, `transport_native.go`, `provider_virsh.go:1992` (`ListVMs`), `transport_fake_test.go`, `transport_native_test.go`

**Interfaces:**
- Produces: `ListVMs(ctx context.Context) ([]contracts.VMInfo, error)` on `controlTransport`. Field-parity contract: same `contracts.VMInfo` fields the virsh implementation fills today (read `provider_virsh.go:1992` first; ID = domain **name**, adoption depends on it).

- [ ] **Step 1: Failing routing test** — fake returns two `VMInfo`; assert `p.ListVMs` returns them and recorded `"ListVMs"`.
- [ ] **Step 2: Implement on `nativeConn`:**

```go
func (n *nativeConn) ListVMs(ctx context.Context) ([]contracts.VMInfo, error) {
	c, err := n.conn()
	if err != nil {
		return nil, err
	}
	doms, err := c.ListAllDomains(0)
	if err != nil {
		return nil, fmt.Errorf("list domains: %w", err)
	}
	defer func() {
		for i := range doms {
			_ = doms[i].Free()
		}
	}()

	out := make([]contracts.VMInfo, 0, len(doms))
	for i := range doms {
		dom := &doms[i]
		name, err := dom.GetName()
		if err != nil {
			continue // a domain vanishing mid-list is not an error
		}
		st, _, err := dom.GetState()
		if err != nil {
			continue
		}
		vmi := contracts.VMInfo{
			ID:         name, // parity: adoption keys on the domain name
			Name:       name,
			PowerState: powerStateOnOff(st),
			ProviderRaw: map[string]string{
				"state": domainStateString(st),
			},
		}
		if uuid, err := dom.GetUUIDString(); err == nil {
			vmi.ProviderRaw["uuid"] = uuid
		}
		if info, err := dom.GetInfo(); err == nil {
			vmi.CPU = int32(info.NrVirtCpu)
			vmi.MemoryMiB = int64(info.Memory / 1024)
		}
		vmi.IPs = n.describeIPs(dom, st)
		if xml, err := dom.GetXMLDesc(0); err == nil {
			for _, tgt := range diskTargets(xml) {
				vmi.Disks = append(vmi.Disks, contracts.DiskInfo{ID: tgt})
			}
		}
		out = append(out, vmi)
	}
	return out, nil
}
```

  Then diff the produced fields against the virsh `ListVMs` (disk paths/sizes, networks): whatever the adoption flow (`internal/controller` adoption code paths — grep `ListVMs(`) actually consumes MUST be filled identically; extend from `GetXMLDesc` with the existing `xmlAttr` helper if adoption needs source paths.
- [ ] **Step 3: Route** in `provider_virsh.go` `ListVMs()`: `if p.nativeTransport != nil { return p.nativeTransport.ListVMs(ctx) }` at the top.
- [ ] **Step 4: Live test** — extend `TestNativeSmoke`: call `conn.ListVMs(ctx)`, assert count matches `ListAllDomains` and every entry has Name+PowerState.
- [ ] **Step 5: Run all verifications.** Commit — `feat(libvirt): native ListVMs behind the gate (adoption-parity fields)`

### Task 5: Snapshots go native

**Files:**
- Modify: `transport.go`, `transport_native.go`, `provider_virsh.go:1490,1556,1593`, `transport_fake_test.go`

**Interfaces:**
- Produces on `controlTransport`:
  - `SnapshotCreate(ctx context.Context, vmID, name, description string, includeMemory bool) error`
  - `SnapshotDelete(ctx context.Context, vmID, snapshotID string) error`
  - `SnapshotRevert(ctx context.Context, vmID, snapshotID string) error`
- The snapshot **name** is still generated by the existing provider code (`SnapshotCreate` at `provider_virsh.go:1490` builds it from `NameHint`); the transport takes it as input, so `SnapshotCreateResponse.SnapshotId` semantics do not change.

- [ ] **Step 1: Failing routing tests** for all three verbs (fake records; assert responses unchanged).
- [ ] **Step 2: Implement on `nativeConn`** (first read `provider_virsh.go:1490` and mirror its `virsh snapshot-create-as` flag usage — in particular whether `--disk-only` is passed when `IncludeMemory` is false):

```go
func (n *nativeConn) SnapshotCreate(ctx context.Context, vmID, name, description string, includeMemory bool) error {
	return n.withDomain(vmID, func(dom *libvirtgo.Domain) error {
		var b strings.Builder
		b.WriteString("<domainsnapshot><name>")
		xmlEscape(&b, name)
		b.WriteString("</name>")
		if description != "" {
			b.WriteString("<description>")
			xmlEscape(&b, description)
			b.WriteString("</description>")
		}
		b.WriteString("</domainsnapshot>")

		var flags libvirtgo.DomainSnapshotCreateFlags
		if !includeMemory {
			flags |= libvirtgo.DOMAIN_SNAPSHOT_CREATE_DISK_ONLY // parity with the virsh --disk-only branch
		}
		snap, err := dom.CreateSnapshotXML(b.String(), flags)
		if err != nil {
			return err
		}
		return snap.Free()
	})
}

func (n *nativeConn) SnapshotDelete(ctx context.Context, vmID, snapshotID string) error {
	return n.withSnapshot(vmID, snapshotID, func(s *libvirtgo.DomainSnapshot) error { return s.Delete(0) })
}

func (n *nativeConn) SnapshotRevert(ctx context.Context, vmID, snapshotID string) error {
	return n.withSnapshot(vmID, snapshotID, func(s *libvirtgo.DomainSnapshot) error { return s.RevertToSnapshot(0) })
}

func (n *nativeConn) withSnapshot(vmID, snapID string, fn func(*libvirtgo.DomainSnapshot) error) error {
	return n.withDomain(vmID, func(dom *libvirtgo.Domain) error {
		snap, err := dom.SnapshotLookupByName(snapID, 0)
		if err != nil {
			return contracts.NewNotFoundError(fmt.Sprintf("snapshot %q not found on %q", snapID, vmID), err)
		}
		defer func() { _ = snap.Free() }()
		return fn(snap)
	})
}

func xmlEscape(b *strings.Builder, s string) { _ = xml.EscapeText(b, []byte(s)) } // import encoding/xml
```

- [ ] **Step 3: Route** the three provider methods native-first (same `if p.nativeTransport != nil` pattern; existing name-generation/validation code above the virsh call stays).
- [ ] **Step 4: Run all verifications. Commit** — `feat(libvirt): native snapshot create/delete/revert behind the gate`

### Task 6: Domain-control halves of `Create`/`Delete` go native

**Files:**
- Modify: `transport.go`, `transport_native.go`, `provider_virsh.go` (`createDomainDefinition:1452`, `defineDomain:1470`, `Delete:211`), `transport_fake_test.go`, `transport_native_test.go`

**Interfaces:**
- Produces on `controlTransport`:
  - `DefineDomain(ctx context.Context, xmlDesc string) error`
  - `UndefineDomain(ctx context.Context, id string) error` (flags mirror `undefineDomain` at `virsh.go:785` — read it first: `--nvram`/`--managed-save`/`--snapshots-metadata` become `DOMAIN_UNDEFINE_NVRAM | DOMAIN_UNDEFINE_MANAGED_SAVE | DOMAIN_UNDEFINE_SNAPSHOTS_METADATA`)
  - `StartDomain(ctx context.Context, id string) error`, `DestroyDomain(ctx context.Context, id string) error`
- **Payoff note:** the virsh define path stages the XML as a remote temp file over SSH; `conn.DomainDefineXML(xmlDesc)` removes that file-staging round-trip entirely. Disk staging, cloud-init ISO build/copy, and disk deletion in `Delete` **stay on the host path**.

- [ ] **Step 1: Failing routing tests** — `Delete` with fake asserts `DestroyDomain:vm1` then `UndefineDomain:vm1` recorded and NO virsh needed for the domain-control half (fake `describe.Exists=true`).
- [ ] **Step 2: Implement on `nativeConn`:**

```go
func (n *nativeConn) DefineDomain(ctx context.Context, xmlDesc string) error {
	c, err := n.conn()
	if err != nil {
		return err
	}
	dom, err := c.DomainDefineXML(xmlDesc)
	if err != nil {
		return fmt.Errorf("define domain: %w", err)
	}
	return dom.Free()
}

func (n *nativeConn) StartDomain(ctx context.Context, id string) error {
	return n.withDomain(id, func(dom *libvirtgo.Domain) error { return dom.Create() })
}

func (n *nativeConn) DestroyDomain(ctx context.Context, id string) error {
	return n.withDomain(id, func(dom *libvirtgo.Domain) error {
		if err := dom.Destroy(); err != nil && !isNotRunning(err) {
			return err
		}
		return nil
	})
}

func (n *nativeConn) UndefineDomain(ctx context.Context, id string) error {
	return n.withDomain(id, func(dom *libvirtgo.Domain) error {
		return dom.UndefineFlags(libvirtgo.DOMAIN_UNDEFINE_MANAGED_SAVE |
			libvirtgo.DOMAIN_UNDEFINE_SNAPSHOTS_METADATA |
			libvirtgo.DOMAIN_UNDEFINE_NVRAM)
	})
}
```

- [ ] **Step 3: Route** — in `createDomainDefinition`/`defineDomain`: native → `DefineDomain(xml)` directly (skip temp-file staging); in `Delete`: native → `DestroyDomain` + `UndefineDomain` replace the `destroyDomain`/`undefineDomain` virsh calls; disk/ISO cleanup after stays as-is.
- [ ] **Step 4: Live lifecycle test** (`transport_native_test.go`) — fully self-contained, no sacrificial domain needed:

```go
// TestNativeDomainLifecycle defines a throwaway diskless domain, walks it
// through start/describe/destroy/undefine, and leaves nothing behind.
const testDomainXML = `<domain type='qemu'>
  <name>virtrigaud-native-test-%d</name>
  <memory unit='MiB'>32</memory><vcpu>1</vcpu>
  <os><type arch='x86_64'>hvm</type></os>
</domain>`
```
  define → `DescribeBasic` exists → `StartDomain` → running → `DestroyDomain` → `UndefineDomain` → `DescribeBasic` not-exists. Use `os.Getpid()` in the name; `defer` cleanup so a failed assert still undefines.
- [ ] **Step 5: Run all verifications + (if lab host available) the live lifecycle. Commit** — `feat(libvirt): native define/start/destroy/undefine in Create/Delete`
- [ ] **Step 6: Milestone gate** — update ADR-0007's phase table status column (Phases 3–5 → implemented); open the M1 PR titled `feat(libvirt): native control plane — lifecycle verbs (ADR-0007 Phases 3–5)`.

---

# Milestone M2 — API groundwork (no runtime behavior change)

### Task 7: `deletionPolicy: Delete | Detach` (spec §4, resolves #286)

**Files:**
- Modify: `api/infra.virtrigaud.io/v1beta1/virtualmachine_types.go:107` (VirtualMachineLifecycle), `internal/controller/virtualmachine_controller.go:413` (deletion path)
- Test: `internal/controller/virtualmachine_controller_test.go` (extend, stubProvider pattern already there)
- Generated: `make manifests generate` output under `config/crd/bases/` + `zz_generated.deepcopy.go`

**Interfaces:**
- Produces: `v1beta1.DeletionPolicy` (`"Delete" | "Detach"`), `VirtualMachineLifecycle.DeletionPolicy` field. Consumed later by M5 drain and the consolidation runbook.

- [ ] **Step 1: Failing controller test** — stubProvider records `Delete` calls; create VM with finalizer + `Lifecycle.DeletionPolicy=Detach` + non-empty `Status.ID`; drive deletion reconcile; assert stub `Delete` NOT called and finalizer removed. Companion test: policy unset → `Delete` IS called (locks current behavior).
- [ ] **Step 2: API types:**

```go
// DeletionPolicy controls what happens to the hypervisor VM when this CR is deleted.
// +kubebuilder:validation:Enum=Delete;Detach
type DeletionPolicy string

const (
	// DeletionPolicyDelete destroys the hypervisor VM (default, current behavior).
	DeletionPolicyDelete DeletionPolicy = "Delete"
	// DeletionPolicyDetach removes the CR but leaves the hypervisor VM running,
	// eligible for re-adoption. Set automatically by adoption flows.
	DeletionPolicyDetach DeletionPolicy = "Detach"
)
```
  and in `VirtualMachineLifecycle`:
```go
	// DeletionPolicy controls deletion semantics. Unset means Delete.
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
```

- [ ] **Step 3: Controller** — in the deletion branch of `virtualmachine_controller.go`, before the "Delete VM from provider" provider resolution:

```go
	if vm.Spec.Lifecycle != nil && vm.Spec.Lifecycle.DeletionPolicy == infravirtrigaudiov1beta1.DeletionPolicyDetach {
		logger.Info("deletionPolicy=Detach: leaving hypervisor VM in place", "id", vm.Status.ID)
		// fall through to finalizer removal without calling provider Delete
	} else { /* existing provider Delete block, unchanged */ }
```

- [ ] **Step 4: Wire adoption** — grep the adoption flow (`grep -rn "adopted" internal/controller/`) and set `DeletionPolicy: Detach` where adopted `VirtualMachine` CRs are constructed. Add one test asserting adoption-created CRs carry it.
- [ ] **Step 5:** `make manifests generate && make test` — Expected: PASS, CRD yaml diff shows the enum. Run `go test ./internal/controller/ -run TestVirtualMachine -count=1` explicitly.
- [ ] **Step 6: Commit** — `feat(api): VirtualMachine deletionPolicy Delete|Detach (#286)`. Separate PR; this is provider-agnostic and independently reviewable.

### Task 8: `LibvirtHost` CRD

**Files:**
- Create: `api/infra.virtrigaud.io/v1beta1/libvirthost_types.go`
- Generated: `config/crd/bases/infra.virtrigaud.io_libvirthosts.yaml`, deepcopy
- Test: envtest create/read in `internal/controller/` suite (schema smoke)

**Interfaces:**
- Produces (consumed by M3 reconciler, M4 inventory/scheduler, M5 drain):

```go
// LibvirtHostSpec defines one libvirt hypervisor host (spec §3.2).
type LibvirtHostSpec struct {
	// URI is the libvirt connection URI for this host (ssh-based; the native
	// transport rewrites it to qemu+libssh2 per ADR-0008).
	// +kubebuilder:validation:Pattern="^qemu(\\+ssh|\\+libssh2|\\+tcp|\\+tls)?://.*$"
	URI string `json:"uri"`
	// CredentialSecretRef overrides the owning Provider's default credentials.
	// +optional
	CredentialSecretRef *ObjectRef `json:"credentialSecretRef,omitempty"`
	// Unschedulable marks the host cordoned: no new placements.
	// +optional
	Unschedulable bool `json:"unschedulable,omitempty"`
	// Taints repel placements (no toleration mechanism yet: any NoSchedule
	// taint excludes the host from scheduling).
	// +optional
	Taints []corev1.Taint `json:"taints,omitempty"`
	// OvercommitCPU multiplies physical CPUs into allocatable (default 1).
	// +optional
	OvercommitCPU *int32 `json:"overcommitCPU,omitempty"`
}

// StoragePoolStatus is one observed pool WITH backend identity (spec §5.3).
type StoragePoolStatus struct {
	Name string `json:"name"`
	// Backend identifies the storage behind the pool (e.g. "nfs:10.0.0.5:/vol/vms"
	// or "dir:/var/lib/libvirt/images") so shared storage is recognizable across hosts.
	// +optional
	Backend   string             `json:"backend,omitempty"`
	Capacity  *resource.Quantity `json:"capacity,omitempty"`
	Available *resource.Quantity `json:"available,omitempty"`
}

type LibvirtHostCapabilities struct {
	LibvirtVersion string `json:"libvirtVersion,omitempty"`
	QemuVersion    string `json:"qemuVersion,omitempty"`
	KVM            bool   `json:"kvm,omitempty"`
	Arch           string `json:"arch,omitempty"`
}

type LibvirtHostStatus struct {
	// +optional
	Capacity corev1.ResourceList `json:"capacity,omitempty"`
	// +optional
	Allocatable corev1.ResourceList `json:"allocatable,omitempty"`
	// +optional
	Allocated corev1.ResourceList `json:"allocated,omitempty"`
	// +optional
	Capabilities *LibvirtHostCapabilities `json:"capabilities,omitempty"`
	// +optional
	StoragePools []StoragePoolStatus `json:"storagePools,omitempty"`
	// +optional
	PreparedImages []string `json:"preparedImages,omitempty"`
	// +optional
	LastHeartbeatTime *metav1.Time `json:"lastHeartbeatTime,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```
  Kind markers: namespaced, status subresource, printcolumns `URI`, `Ready` (from conditions), `Unschedulable`, `Age`. Add `LibvirtHost`/`LibvirtHostList` structs + `SchemeBuilder.Register` in an `init()` (copy the shape of `vmset_types.go`).

- [ ] **Step 1:** Write the types file (above, complete). **Step 2:** `make manifests generate` — new CRD yaml appears; commit generated files together.
- [ ] **Step 3:** envtest smoke: create a `LibvirtHost` with URI + status patch of capacity; read back; assert quantities survive round-trip.
- [ ] **Step 4:** `make test` PASS. **Step 5: Commit** — `feat(api): LibvirtHost CRD (host inventory for the multi-host libvirt provider)`

### Task 9: `Provider.spec.hostSelector` + CEL + controller tolerance

**Files:**
- Modify: `api/infra.virtrigaud.io/v1beta1/provider_types.go` (spec fields + CEL), `internal/controller/provider_controller.go:768` (env build), `validateRemoteRuntimeSpec:547`
- Test: envtest CEL rejection tests; controller env test

**Interfaces:**
- Produces: `ProviderSpec.HostSelector *metav1.LabelSelector`; `Endpoint` becomes optional. M3 keys "cluster mode" off `HostSelector != nil`.

- [ ] **Step 1: Failing envtest** — creating a Provider with BOTH `endpoint` and `hostSelector` must be rejected; with NEITHER must be rejected; `hostSelector` on `type: vsphere` must be rejected; libvirt+hostSelector-only must be accepted.
- [ ] **Step 2: API change** — on `ProviderSpec` struct add markers ABOVE the type:

```go
// +kubebuilder:validation:XValidation:rule="has(self.endpoint) != has(self.hostSelector)",message="exactly one of spec.endpoint or spec.hostSelector must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.hostSelector) || self.type == 'libvirt'",message="spec.hostSelector is only supported for type=libvirt"
type ProviderSpec struct {
```
  change `Endpoint` to `// +optional` + `json:"endpoint,omitempty"` (keep the existing Pattern marker — it applies only when set), and add:

```go
	// HostSelector selects the LibvirtHost objects (same namespace) forming
	// this provider's fleet. Setting it puts the provider in multi-host
	// (cluster) mode. Exactly one of endpoint/hostSelector must be set.
	// +optional
	HostSelector *metav1.LabelSelector `json:"hostSelector,omitempty"`
```

- [ ] **Step 3: Controller** — in the env-var build at `provider_controller.go:768`, emit `PROVIDER_ENDPOINT` only when `provider.Spec.Endpoint != ""`; in `validateRemoteRuntimeSpec`, accept the hostSelector form. Grep for other unconditional `Spec.Endpoint` consumers (`grep -rn "Spec.Endpoint" internal/ | grep -v _test`) and guard each.
- [ ] **Step 4:** `make manifests generate && make test` — CEL tests PASS (envtest ≥1.25 evaluates CEL server-side). **Step 5: Commit** — `feat(api): Provider.spec.hostSelector for multi-host libvirt (CEL: exactly one of endpoint/hostSelector)`

### Task 10: Host-qualified ID helpers

**Files:**
- Create: `internal/providers/libvirt/hostid.go`, `internal/providers/libvirt/hostid_test.go`

**Interfaces:**
- Produces (used by M3 router, M5 drain):

```go
// formatHostID qualifies a per-host domain ID with its LibvirtHost name.
// Cluster-mode IDs are "<host>/<domain>"; single-host IDs stay bare (spec §3.3).
func formatHostID(host, domainID string) string { return host + "/" + domainID }

// splitHostID splits a cluster-mode ID. ok=false for bare single-host IDs.
func splitHostID(id string) (host, domainID string, ok bool) {
	i := strings.IndexByte(id, '/')
	if i <= 0 || i == len(id)-1 {
		return "", id, false
	}
	return id[:i], id[i+1:], true
}
```

- [ ] **Step 1:** Table test (bare name, qualified, leading `/`, trailing `/`, empty). **Step 2:** implement. **Step 3:** `go test ./internal/providers/libvirt/ -run TestHostID -v` PASS. **Step 4: Commit** — `feat(libvirt): host-qualified ID helpers for cluster mode`

---

# Milestone M3 — Cluster runtime skeleton (multi-host create/route, naive placement)

One PR. After M3 a `Provider` with `hostSelector` over two `LibvirtHost`s can create/describe/delete VMs across both hosts in kind. Blast radius: entirely behind `hostSelector` (new mode); single-host untouched.

### Task 11: Runtime skeleton + host-agent Deployments

**Files:**
- Create: `internal/providers/libvirt/cluster/runtime.go`, `internal/providers/libvirt/cluster/hostagents.go`
- Modify: `cmd/provider-libvirt/main.go` (mode switch)
- Create: `config/rbac/libvirt_cluster_runtime_role.yaml` (+ kustomize entry)
- Test: envtest `internal/providers/libvirt/cluster/hostagents_test.go`

**Interfaces:**
- Consumes: `LibvirtHost` (Task 8), mode env. Produces: running manager with `HostAgentReconciler`; agent Service DNS convention **`<provider-name>-agent-<host-name>:<same gRPC port as single-host provider>`** — the router (Task 12) depends on exactly this naming.
- Env contract (set by provider controller in Task 13): `VIRTRIGAUD_LIBVIRT_MODE=cluster`, `VIRTRIGAUD_AGENT_IMAGE=<provider.Spec.Runtime.Image>`, plus existing `PROVIDER_NAME`/`PROVIDER_NAMESPACE`.

- [ ] **Step 1: Failing envtest** — create Provider(hostSelector matchLabels `fleet: a`) + two `LibvirtHost`s labeled `fleet: a`; run `HostAgentReconciler` against the envtest client; assert two Deployments + two Services exist, each with `PROVIDER_ENDPOINT` = the host's URI, ownerRef → its LibvirtHost, and the credentials Secret (host override or provider default) mounted the same way `provider_controller.go` mounts it today (copy that mount shape).
- [ ] **Step 2: `hostagents.go`** — a `HostAgentReconciler struct{ client.Client; ProviderName, Namespace, AgentImage string }` reconciling `LibvirtHost`: list hosts matching the Provider's selector → CreateOrUpdate Deployment+Service per host (name via `fmt.Sprintf("%s-agent-%s", providerName, host.Name)`, labels for the Service selector, env `PROVIDER_TYPE=libvirt`, `PROVIDER_ENDPOINT=host.Spec.URI`, `LIBVIRT_CONTROL_TRANSPORT=native`); delete agents whose host no longer matches. Requeue 30s.
- [ ] **Step 3: `runtime.go`** — `Run(ctx)`: controller-runtime manager (namespace-scoped cache), register reconciler, and start the provider gRPC server with the router (Task 12) as the `contracts.Provider` implementation — reuse the same server bootstrap `cmd/provider-libvirt/main.go` uses today (`server.New(config)` + `RegisterProvider`).
- [ ] **Step 4: `main.go`** — `if os.Getenv("VIRTRIGAUD_LIBVIRT_MODE") == "cluster" { cluster.Run(ctx); return }` before the existing single-host path.
- [ ] **Step 5: RBAC yaml** — Role: `libvirthosts` get/list/watch + `libvirthosts/status` patch/update; `deployments`,`services` create/get/list/watch/update/delete; `secrets` get/list/watch; `configmaps` get/create/update (M5 staging record). RoleBinding to the runtime ServiceAccount.
- [ ] **Step 6:** `make test` (envtest) PASS + default build. **Commit** — `feat(libvirt): cluster runtime skeleton — host-agent deployments per LibvirtHost`

### Task 12: The router — `contracts.Provider` over host agents

**Files:**
- Create: `internal/providers/libvirt/cluster/router.go`, `internal/providers/libvirt/cluster/router_test.go`

**Interfaces:**
- Consumes: `formatHostID`/`splitHostID` (Task 10), agent DNS convention (Task 11), `internal/transport/grpc.NewClient(ctx, endpoint, providerType, providerName, cb, tlsConfig)` (returns a `contracts.Provider`-compatible client — verify with `grep -n "func (c \*Client)" internal/transport/grpc/client.go` that all 15 verbs are present).
- Produces:

```go
// Router implements contracts.Provider for a multi-host libvirt Provider by
// routing every verb to the owning host agent via the host-qualified ID.
type Router struct {
	// ClientFor returns a per-host southbound client; injected so tests use fakes.
	ClientFor func(ctx context.Context, host string) (contracts.Provider, error)
	// PickHost chooses a host for Create/ImportDisk (Task 13/M4/M5 evolve it).
	PickHost func(ctx context.Context, req contracts.CreateRequest) (string, error)
	// Hosts lists candidate host names (Ready LibvirtHosts), for ListVMs fan-out
	// and the §3.3 fleet-wide miss fallback.
	Hosts func(ctx context.Context) ([]string, error)
}
```
  Routing rules (spec §3.3): `Create` → `PickHost` → agent `Create` → return `formatHostID(host, resp.ID)`. ID-taking verbs → `splitHostID`; `ok=false` → `InvalidSpec` error ("cluster mode requires host-qualified IDs"). On agent `NotFound` for the hinted host → bounded fleet lookup (`Describe` on every other host, first `Exists:true` wins; correct host surfaced in `ProviderRaw["host"]`); nowhere → NotFound (keeps `Delete` idempotent). `ListVMs` → fan out over `Hosts`, qualify every `VMInfo.ID`, add `ProviderRaw["host"]`. `Validate` → all hosts, error if none reachable. `ExportDisk`/`GetDiskInfo`/snapshots/power/reconfigure → pure ID-routed passthrough. `ImportDisk` → M5 (until then: route to `PickHost` result and log; no staging record yet).

- [ ] **Step 1: Failing router tests** (table-driven, fake `contracts.Provider` per host recording calls): create-routes-and-qualifies; describe-routes-by-id; describe-miss-falls-back-to-fleet-and-reports-host; delete-unknown-id-returns-notfound; listvms-fan-out-qualifies; bare-id-rejected.
- [ ] **Step 2: Implement `router.go`** (~150 lines; every verb explicit, no reflection).
- [ ] **Step 3:** `go test ./internal/providers/libvirt/cluster/ -v` PASS. **Commit** — `feat(libvirt): cluster router — provider contract over host agents by host-qualified ID`

### Task 13: Wire provider controller + first placement + kind E2E

**Files:**
- Modify: `internal/controller/provider_controller.go` (cluster-mode env + ServiceAccount), `internal/providers/libvirt/cluster/runtime.go` (real `ClientFor`/`Hosts`/`PickHost`)
- Test: envtest for the controller env; manual kind verification (documented)

- [ ] **Step 1:** Provider controller: when `Spec.HostSelector != nil` → add env `VIRTRIGAUD_LIBVIRT_MODE=cluster`, `VIRTRIGAUD_AGENT_IMAGE=<runtime image>`, attach the cluster-runtime ServiceAccount (Task 11 RBAC) to the Deployment. Envtest: Provider with hostSelector produces a Deployment with those envs; Provider with endpoint produces today's exact env set (regression).
- [ ] **Step 2:** Real wiring in `runtime.go`: `ClientFor` = `grpc.NewClient` against `fmt.Sprintf("%s-agent-%s.%s.svc:%d", providerName, host, namespace, port)` with a small LRU of clients; `Hosts` = list LibvirtHosts (Ready condition true, `!Unschedulable`); `PickHost` = **round-robin** over `Hosts` (`// ponytail: round-robin; capacity-aware scheduler lands in M4`).
- [ ] **Step 3: kind E2E (manual, recorded in the PR):** two `LibvirtHost`s pointing at lab hosts (or one host twice with different names for smoke), Provider with hostSelector, create 3 `VirtualMachine`s → assert spread across agents, `status.id` host-qualified, delete works. Use the existing kind recipe (ghcr manager + kustomize; load local provider image via `podman save | kind load image-archive /dev/stdin --name virtrigaud`).
- [ ] **Step 4: Commit** — `feat(libvirt): multi-host Provider — controller wiring + round-robin placement`. Open the M3 PR.

---

# Milestone M4 — Inventory, scheduler, capacity aggregation

### Task 14: Host inventory collector (build-tagged)

**Files:**
- Create: `internal/providers/libvirt/cluster/inventory_native.go` (`//go:build libvirt_native`), `inventory_stub.go` (non-tagged: returns "inventory requires the native build" error), `inventory_test.go` (pure transform functions, non-tagged)

**Interfaces:**
- Consumes: `dialNative` (same package tree — export a tiny constructor `libvirt.DialControl(uri string) (ControlConn, error)` if package boundaries require; keep it minimal). Produces: `CollectHostFacts(ctx, uri) (HostFacts, error)` where

```go
type HostFacts struct {
	CapacityCPU    int64  // physical cpus
	CapacityMemKiB uint64 // NodeInfo.Memory
	AllocatedCPU   int64  // sum of defined domains' vcpus
	AllocatedMemKiB uint64
	LibvirtVersion, Arch string
	KVM            bool
	Pools          []PoolFacts // name, backend (from pool XML <source>/<target>), capacity, available
	Images         []string    // volume names in the provider's default pool
}
```
  plus a pure `factsToStatus(HostFacts, overcommitCPU int32) v1beta1.LibvirtHostStatus` (unit-testable without CGO) and a poll loop in the runtime (60s, jittered) that writes `LibvirtHost.status` + `lastHeartbeatTime` and sets `Ready` condition from dial success/failure. Backend identity: `nfs:<host>:<path>` / `dir:<path>` / `logical:<source>` derived from pool XML — one `poolBackendFromXML(xml string) string` helper with table tests.

- [ ] **Step 1:** Table tests for `factsToStatus` (overcommit math: allocatable = capacity×overcommit − allocated floor at 0) and `poolBackendFromXML` (dir/nfs/logical XML fixtures).
- [ ] **Step 2:** Implement collector (`GetNodeInfo`, `ListAllDomains`+`GetInfo` for allocated, `ListAllStoragePools`+`GetXMLDesc`+`GetInfo`, `GetLibVersion`) and the poll loop.
- [ ] **Step 3:** Verifications: default build (stub path), container native-vet, live: point collector at lab host, dump status. **Commit** — `feat(libvirt): LibvirtHost inventory — capacity, pools with backend identity, images`

### Task 15: Capacity-aware scheduler with policy + cordon

**Files:**
- Create: `internal/providers/libvirt/cluster/scheduler.go`, `scheduler_test.go`
- Modify: `runtime.go` (`PickHost` swaps round-robin → scheduler)

**Interfaces:**
- Produces:

```go
type hostFacts struct {
	Name          string
	Unschedulable bool
	Taints        []corev1.Taint
	Allocatable   corev1.ResourceList
	Allocated     corev1.ResourceList
	Pools         map[string]bool // pool name → exists (default-pool check, spec §3.2)
	Images        map[string]bool // image locality soft preference (spec §5.2)
}

// pickHost filters then scores. Filters: unschedulable, any NoSchedule taint,
// policy hosts/excludedHosts (host-selection subset of VMPlacementPolicy),
// CPU+memory fit, required pool present. Score: +1 image present, tiebreak
// most free memory. Deterministic for tests.
func pickHost(req contracts.CreateRequest, policy *v1beta1.VMPlacementPolicySpec, hosts []hostFacts) (string, error)
```

- [ ] **Step 1:** Table tests: fit-excludes-full-host; cordon-excludes; taint-excludes; policy-hosts-allowlist; policy-excludedHosts; image-locality-preferred; all-filtered → typed "no schedulable host" error (surfaced as retryable).
- [ ] **Step 2:** Implement (~80 lines, pure function). Wire: runtime resolves the VM's `placementRef` → `VMPlacementPolicy` (RBAC: + `vmplacementpolicies` get/list/watch) and builds `hostFacts` from cached LibvirtHost statuses.
- [ ] **Step 3:** Tests PASS; kind spot-check (cordon a host, creates land on the other). **Commit** — `feat(libvirt): capacity-aware host scheduler (VMPlacementPolicy host subset, cordon, taints, image locality)`

### Task 16: `Provider.status` aggregation + `CapacityDegraded`

**Files:**
- Modify: `api/infra.virtrigaud.io/v1beta1/provider_types.go` (status fields), `internal/providers/libvirt/cluster/runtime.go` (aggregation loop)
- Test: envtest aggregation; `make manifests generate`

**Interfaces:**
- Produces on `ProviderStatus` (spec §3.2 capacity semantics):

```go
type ProviderHostsSummary struct {
	Total       int32 `json:"total,omitempty"`
	Ready       int32 `json:"ready,omitempty"`
	Schedulable int32 `json:"schedulable,omitempty"`
}
type ProviderCapacitySummary struct {
	Allocatable corev1.ResourceList `json:"allocatable,omitempty"`
	Allocated   corev1.ResourceList `json:"allocated,omitempty"`
}
// in ProviderStatus:
// +optional
Hosts *ProviderHostsSummary `json:"hosts,omitempty"`
// +optional
Capacity *ProviderCapacitySummary `json:"capacity,omitempty"`
```

- [ ] **Step 1:** Failing envtest: three LibvirtHosts (2 Ready, 1 heartbeat 5min stale) → aggregate → `hosts{3,2,2}`, capacity sums exclude the stale host, Provider condition `CapacityDegraded=True` with the stale host named in the message.
- [ ] **Step 2:** Pure `aggregate(hosts []v1beta1.LibvirtHost, now time.Time, staleAfter time.Duration) (ProviderHostsSummary, ProviderCapacitySummary, degraded []string)` + runtime loop patching Provider status subresource (RBAC: + `providers/status` patch). Stale = `now − lastHeartbeatTime > 3×poll` (spec §3.2).
- [ ] **Step 3:** `make manifests generate && make test` PASS. **Commit** — `feat(libvirt): Provider.status host/capacity aggregation with CapacityDegraded`. Open the M4 PR.

---

# Milestone M5 — Migration target + drain + shared storage

### Task 17: `ImportDisk` staging-pins-placement (spec §5.1)

**Files:**
- Modify: `internal/providers/libvirt/cluster/router.go` (ImportDisk + Create pinning)
- Create: `internal/providers/libvirt/cluster/staging.go`, `staging_test.go`

**Interfaces:**
- Produces: a durable staging record — ConfigMap `"<provider-name>-staging"` in the runtime namespace, `data[<TargetName>] = <host>` (`ImportDiskRequest` has **no VmId**, only `TargetName` — first VERIFY in `internal/controller/vmmigration_controller.go` how `TargetName` relates to the target `VirtualMachine` name, and key the record by whatever `Create` can later match on; record the finding as a comment in `staging.go`).

```go
type StagingStore struct{ client.Client; Namespace, Name string }
func (s *StagingStore) Record(ctx context.Context, key, host string) error   // create-or-patch
func (s *StagingStore) Lookup(ctx context.Context, key string) (string, bool, error)
func (s *StagingStore) Clear(ctx context.Context, key string) error
```

- [ ] **Step 1:** Router tests: `ImportDisk` → PickHost → routed to that agent → `Record(TargetName, host)` called; subsequent `Create` whose disk/image matches a staged key → placed on the recorded host (bypasses scheduler), `Clear` after success; `Create` without staging → normal scheduling.
- [ ] **Step 2:** Implement `staging.go` (envtest for CRUD) + router wiring: `ImportDisk` schedules (reuse `pickHost` with a pool-space-only request), records, routes; `Create` consults `Lookup` first.
- [ ] **Step 3:** kind + lab verification: run a `VMMigration` into the multi-host provider; assert disk and VM land on the same host. **Commit** — `feat(libvirt): migration-target routing — ImportDisk staging pins Create placement`

### Task 18: Drain — cordon → evacuate → verify

**Files:**
- Create: `internal/providers/libvirt/cluster/drain.go`, `drain_test.go`
- Modify: `hostagents.go` (watch for the drain annotation)

**Interfaces:**
- Trigger: annotation `infra.virtrigaud.io/drain: "true"` on a `LibvirtHost` (spec: cordon+drain are Node-like ops). Produces `drainHost(ctx, host string) error`:
  1. set `Unschedulable` (cordon) if not already;
  2. `ListVMs` on that host's agent; for each domain: pick a target host (scheduler, excluding the draining host) → **move** (Task 19 decides zero-copy vs copy: shared backend → re-define; else agent-to-agent `ExportDisk` → shared RWX PVC (`VIRTRIGAUD_DRAIN_PVC` env, documented) → `ImportDisk` → `DefineDomain` from source XML with rewritten disk paths → start if it was running → destroy+undefine source, **disks preserved on shared storage / deleted from source only after target verified running** on the copy path);
  3. condition `Drained=True` when the agent's `ListVMs` is empty; every step idempotent and resumable (re-entering drain re-lists and continues).
  Manager-visible effect: `status.hostRef`/`ProviderRaw["host"]` changes on next Describe via the §3.3 fleet-lookup — VM `status.ID`'s stale host hint is healed by the router, never rewritten by the manager.
- [ ] **Step 1:** Unit tests with fake agents + fake mover: drain-empty-host-is-noop; drain-moves-all-and-sets-condition; drain-resumes-after-partial-failure; target-selection-excludes-draining-host.
- [ ] **Step 2:** Implement orchestration (mover injected as an interface so Task 19 supplies the two strategies).
- [ ] **Step 3:** Lab verification on two hosts sharing nothing (copy path). **Commit** — `feat(libvirt): host drain — cordon and evacuate via export/import between agents`

### Task 19: Shared-storage zero-copy move + clone locality

**Files:**
- Modify: `internal/providers/libvirt/cluster/drain.go` (mover strategy selection), `router.go` (clone/snapshot routing note)
- Test: `drain_test.go` extension

**Interfaces:**
- Mover selection: if source and target host both report a pool whose `StoragePoolStatus.Backend` is **equal and non-`dir:`** for every disk of the domain → zero-copy: source agent `DestroyDomain` (if running) + `UndefineDomain` (metadata only — disks untouched), target agent `DefineDomain(sourceXML)` + `StartDomain`. Else → the Task 18 copy path.
- Clone/snapshot locality (spec §5.4): snapshots are already ID-routed (host-local — correct by construction); add a router guard comment + test asserting snapshot verbs on a moved VM follow the fleet-lookup to the new host.
- [ ] **Step 1:** Tests: shared-backend-selects-zero-copy; dir-backend-selects-copy; zero-copy-preserves-disks (fake agent asserts no ExportDisk called).
- [ ] **Step 2:** Implement strategy selection (~40 lines) using `LibvirtHost.status.storagePools`.
- [ ] **Step 3:** Lab verification with an NFS pool mounted on both hosts: drain moves domains in seconds without data copy. **Commit** — `feat(libvirt): zero-copy drain on shared storage; clone/snapshot host locality`. Open the M5 PR.
- [ ] **Step 4: Close the loop** — update `SPEC-libvirt-multihost-provider.md` phasing checkboxes; revise ADR-0007 Status to Accepted-in-part; draft the upstream summary comment for #257.

---

## Deferred (recorded, deliberately not in this plan)

- Flipping `LIBVIRT_CONTROL_TRANSPORT` default to `native` (ADR-0008 auth-parity gate: password auth unverified on libssh2).
- ADR-0007 Phase 6 (`hostrunner` extraction, deleting virsh) — only after native has soaked as default.
- Domain-**UUID**-based cluster IDs (upgrade from `<host>/<name>`; needed before fleets with colliding names across hosts are supported).
- Level-A cross-provider scheduler, `VMSet`/MachinePool mapping, live migration, HA (spec §6).
- Consolidation tooling for merging existing single-host Providers (runbook first; spec open question 5 blocks automation).

## Self-review notes

- Spec coverage: §3.1 (Tasks 9, 11, 13), §3.2 (8, 14, 16), §3.3 (10, 12), §3.4 (11–13), §4 (7), §5.1 (17), §5.2 (14, 15), §5.3 (14, 19), §5.4 (19), §7 phases 0–4 map to M1–M5. Level-A items are explicitly deferred (spec §6).
- Known verify-at-execution points (flagged inline, not placeholders): virsh `ListVMs` field parity (Task 4), snapshot flag parity (Task 5), undefine flags (Task 6), adoption construction site (Task 7), `TargetName`↔VM-name correlation in `vmmigration_controller.go` (Task 17), `internal/transport/grpc.Client` verb coverage (Task 12).
