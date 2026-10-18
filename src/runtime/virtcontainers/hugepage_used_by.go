// Copyright (c) 2026 Kata Containers community
//
// SPDX-License-Identifier: Apache-2.0
//

package virtcontainers

import (
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/agent/protocols/grpc"
	vcAnnotations "github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/annotations"
	"github.com/kata-containers/kata-containers/src/runtime/virtcontainers/utils"
)

// Who uses a container's hugepages-<size> of the VM's page size as huge pages
// inside a guest sized from the pod.
const (
	// The pages back the VM, and the container gets them as its guest memory
	// ceiling; its memory limit is not used there.
	HugePagesUsedByHost = "host"
	// The pages stay huge pages inside the guest, as on runc; the container's
	// memory limit is its guest memory ceiling.
	HugePagesUsedByGuest = "guest"
)

// Checks hugepages_used_by; empty is the default, host.
func validHugePagesUsedBy(usedBy string) bool {
	switch usedBy {
	case "", HugePagesUsedByHost, HugePagesUsedByGuest:
		return true
	}
	return false
}

// Parses io.katacontainers.container.hugepages_used_by, "name=guest,name=host".
func parseHugePagesUsedBy(value string) (map[string]string, error) {
	usedBy := make(map[string]string)
	if strings.TrimSpace(value) == "" {
		return usedBy, nil
	}
	for _, entry := range strings.Split(value, ",") {
		name, user, found := strings.Cut(strings.TrimSpace(entry), "=")
		name, user = strings.TrimSpace(name), strings.TrimSpace(user)
		if !found || name == "" {
			return nil, fmt.Errorf("annotation %s: entry %q is not name=%s or name=%s", vcAnnotations.ContainerHugePagesUsedBy, entry, HugePagesUsedByHost, HugePagesUsedByGuest)
		}
		if user != HugePagesUsedByHost && user != HugePagesUsedByGuest {
			return nil, fmt.Errorf("annotation %s: container %q has %q; use %s or %s", vcAnnotations.ContainerHugePagesUsedBy, name, user, HugePagesUsedByHost, HugePagesUsedByGuest)
		}
		if _, dup := usedBy[name]; dup {
			return nil, fmt.Errorf("annotation %s names container %q twice", vcAnnotations.ContainerHugePagesUsedBy, name)
		}
		usedBy[name] = user
	}
	return usedBy, nil
}

// The entries the sandbox was started with. The pod's annotations reach every
// container's spec too, but a restarted container carries the pod's current
// ones, so the sandbox's copy is the one that holds.
func sandboxHugePagesUsedBy(sandbox *Sandbox) (map[string]string, error) {
	spec := sandbox.GetPatchedOCISpec()
	if spec == nil {
		return map[string]string{}, nil
	}
	return parseHugePagesUsedBy(spec.Annotations[vcAnnotations.ContainerHugePagesUsedBy])
}

// Who uses a container's huge pages: the pod's entry for it, or the runtime
// class's default. own is what the container reserved of the VM's page size
// itself; one that reserved none has nothing to keep for the guest, so the
// class's default does not make it guest. A container that carries a
// different value from the one the sandbox started with is refused.
func containerHugePagesUsedBy(sandbox *Sandbox, cfg *ContainerConfig, own uint64) (string, error) {
	// The sandbox container reserves nothing and runs nothing that maps huge
	// pages.
	if ContainerType(cfg.Annotations[vcAnnotations.ContainerTypeKey]).IsSandbox() {
		return HugePagesUsedByHost, nil
	}
	usedBy, err := sandboxHugePagesUsedBy(sandbox)
	if err != nil {
		return "", err
	}
	if cfg.CustomSpec != nil {
		if current, ok := cfg.CustomSpec.Annotations[vcAnnotations.ContainerHugePagesUsedBy]; ok {
			spec := sandbox.GetPatchedOCISpec()
			if spec != nil && current != spec.Annotations[vcAnnotations.ContainerHugePagesUsedBy] {
				return "", fmt.Errorf("annotation %s changed from %q to %q after the pod started: it cannot change while the pod runs", vcAnnotations.ContainerHugePagesUsedBy, spec.Annotations[vcAnnotations.ContainerHugePagesUsedBy], current)
			}
		}
	}
	if user, ok := usedBy[containerDisplayName(cfg)]; ok {
		return user, nil
	}
	if own != 0 && sandbox.config.HypervisorConfig.HugePagesUsedBy == HugePagesUsedByGuest {
		return HugePagesUsedByGuest, nil
	}
	return HugePagesUsedByHost, nil
}

// The largest guest pool a sandbox has asked the agent for, by page size. The
// agent sets the guest's global nr_hugepages to what each storage asks for, so
// every pool storage carries the sandbox's running total and never less than
// before: a pool is never shrunk under a container that holds its pages.
func (k *kataAgent) guestPoolTotal(pageSize string, pools uint64) uint64 {
	k.poolsLock.Lock()
	defer k.poolsLock.Unlock()
	if k.poolsHighWater == nil {
		k.poolsHighWater = make(map[string]uint64)
	}
	if pools > k.poolsHighWater[pageSize] {
		k.poolsHighWater[pageSize] = pools
	}
	return k.poolsHighWater[pageSize]
}

// The guest pool held for pageSize, kept after the containers that asked stop.
func (k *kataAgent) guestPoolsHeld(pageSize string) uint64 {
	k.poolsLock.Lock()
	defer k.poolsLock.Unlock()
	return k.poolsHighWater[pageSize]
}

func maxUint64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

// A refusal for the guest's capacity, which leaves the container as it was.
type guestCapacityRefusal struct{ error }

func (e guestCapacityRefusal) Unwrap() error { return e.error }

// The guest memory ceiling and guest huge pages a container was given.
type appliedShare struct{ ceiling, pool uint64 }

// Records what a container was given.
func (k *kataAgent) recordApplied(id string, resources *grpc.LinuxResources, pool uint64) {
	k.poolsLock.Lock()
	defer k.poolsLock.Unlock()
	if k.applied == nil {
		k.applied = make(map[string]appliedShare)
	}
	share := appliedShare{pool: pool}
	if resources != nil {
		share.ceiling = uint64(memoryLimitBytes(resources.Memory))
	}
	k.applied[id] = share
}

func (k *kataAgent) appliedPool(id string) uint64 {
	k.poolsLock.Lock()
	defer k.poolsLock.Unlock()
	return k.applied[id].pool
}

func (k *kataAgent) appliedCopy() map[string]appliedShare {
	k.poolsLock.Lock()
	defer k.poolsLock.Unlock()
	out := make(map[string]appliedShare, len(k.applied))
	for id, s := range k.applied {
		out[id] = s
	}
	return out
}

// The running containers' guest huge pages, a running one counting with what it was given.
func podPoolsBytes(reservations []podReservation, copied uint64, applied map[string]appliedShare, self string) uint64 {
	var total uint64
	for _, r := range reservations {
		pool := ownPoolBytes(r, copied)
		if a, ok := applied[r.id]; ok && r.id != self {
			pool = a.pool
		}
		if pool > math.MaxUint64-total {
			return math.MaxUint64
		}
		total += pool
	}
	return total
}

// A container's memory ceiling inside the guest, or zero when it has none.
func guestCeilingBytes(r podReservation, copied uint64) uint64 {
	if !r.pool {
		if own, _ := ownHugePages(r.hugePages, copied, false); own != 0 {
			return own
		}
	}
	if r.memory > 0 && r.memory != math.MaxInt64 {
		return uint64(r.memory)
	}
	return 0
}

// Refuses a container when the pod's ceilings, as given, and guest huge pages do not fit.
func refusePodCeilingsPastGuest(reservations []podReservation, copied, running, held uint64, applied map[string]appliedShare, guest guestCapacity, self, name string) error {
	guest.pools = 0
	holdable := holdableGuestMemoryBytes(guest)
	if holdable <= 0 {
		return nil
	}
	pools := maxUint64(running, held)
	var ceilings uint64
	for _, r := range reservations {
		c := guestCeilingBytes(r, copied)
		if a, ok := applied[r.id]; ok && r.id != self {
			c = a.ceiling
		}
		if c == 0 && pools != 0 && !r.sandbox {
			return fmt.Errorf("container %s has no memory ceiling in a guest that keeps %d MiB of huge pages: give it a memory limit or huge pages of its own", r.name, pools>>utils.MibToBytesShift)
		}
		if c > math.MaxUint64-ceilings {
			ceilings = math.MaxUint64
			break
		}
		ceilings += c
	}
	need := ceilings
	if pools > math.MaxUint64-need {
		need = math.MaxUint64
	} else {
		need += pools
	}
	if need <= uint64(holdable) {
		return nil
	}
	short := (need - uint64(holdable)) >> utils.MibToBytesShift
	if short == 0 {
		short = 1
	}
	stale := ""
	if held > running {
		stale = fmt.Sprintf(" (%d MiB of them still held for containers that stopped)", (held-running)>>utils.MibToBytesShift)
	}
	return fmt.Errorf("container %s brings the memory ceilings of the pod's containers to %d MiB beside %d MiB of guest huge pages%s, but the %d MiB guest holds %d MiB for them: the pod needs %d MiB more of the VM's huge pages, on the pod or as the runtime class's pod overhead",
		name,
		ceilings>>utils.MibToBytesShift,
		pools>>utils.MibToBytesShift,
		stale,
		guest.memoryMB,
		uint64(holdable)>>utils.MibToBytesShift,
		short)
}

// Whether every container carries the same value past the VM, copy or not.
func ambiguousCopiedReservation(reservations []podReservation, vmMemoryBytes uint64) (uint64, bool) {
	if vmMemoryBytes == 0 {
		return 0, false
	}
	var value, sum uint64
	carriers := 0
	for _, r := range reservations {
		if r.hugePages == 0 {
			continue
		}
		if value != 0 && r.hugePages != value {
			return 0, false
		}
		value = r.hugePages
		if r.hugePages > math.MaxUint64-sum {
			sum = math.MaxUint64
		} else {
			sum += r.hugePages
		}
		carriers++
	}
	return value, carriers > 1 && sum > vmMemoryBytes && value <= vmMemoryBytes
}

// A hugetlbfs storage the agent mounts out of the container's sight, so that a
// guest container gets its pool whether or not it mounts a huge page volume.
func guestPoolStorage(containerID string, pageSizeBytes, total uint64) *grpc.Storage {
	return &grpc.Storage{
		Driver:     KataEphemeralDevType,
		Source:     "nodev",
		Fstype:     "hugetlbfs",
		MountPoint: filepath.Join(ephemeralPath(), "kata-hugepage-pool-"+containerID),
		Options:    []string{fmt.Sprintf("pagesize=%s,size=%s", strconv.FormatUint(pageSizeBytes, 10), strconv.FormatUint(total, 10))},
	}
}

// Zeroes a host container's limit for the VM's page size, so it cannot take
// pages from the guest containers' pool.
func stripPoolPages(limits []*grpc.LinuxHugepageLimit, pageSize string) {
	for _, l := range limits {
		if pageSize == "" || l.Pagesize == pageSize {
			l.Limit = 0
		}
	}
}

// Puts the reservation a container holds back into an update that names no
// huge pages of the VM's size, as one for the CPUs alone, so that the update
// still bounds the container by it.
func foldHeldHugePages(resources *grpc.LinuxResources, pageSize string, held uint64) {
	if held == 0 {
		return
	}
	for _, l := range resources.HugepageLimits {
		if l.Pagesize == pageSize {
			return
		}
	}
	resources.HugepageLimits = append(resources.HugepageLimits, &grpc.LinuxHugepageLimit{Pagesize: pageSize, Limit: held})
}

// The first page size other than the VM's that a container reserves. Such a
// pool is neither in the VM's size nor backed by host pages of its own size.
func otherPageSizeReserved(limits []*grpc.LinuxHugepageLimit, pageSize string) string {
	if pageSize == "" {
		return ""
	}
	for _, l := range limits {
		if l.Pagesize != pageSize && l.Limit != 0 {
			return l.Pagesize
		}
	}
	return ""
}

// Refuses a ceiling the guest cannot give, naming the shortfall, instead of
// holding the container below what it declared.
func refuseCeilingPastGuest(name string, ceiling int64, guest guestCapacity) error {
	holdable := holdableGuestMemoryBytes(guest)
	if holdable <= 0 || ceiling <= holdable {
		return nil
	}
	short := uint64(ceiling-holdable) >> utils.MibToBytesShift
	if short == 0 {
		short = 1
	}
	return fmt.Errorf("container %s declares a %d MiB memory ceiling, but the %d MiB guest holds %d MiB for it after %d MiB of huge page pools: the pod needs %d MiB more of the VM's huge pages, on the pod or as the runtime class's pod overhead",
		name,
		uint64(ceiling)>>utils.MibToBytesShift,
		guest.memoryMB,
		uint64(holdable)>>utils.MibToBytesShift,
		guest.pools>>utils.MibToBytesShift,
		short)
}

// The bytes a memory limit carries, or zero for none.
func memoryLimitBytes(memory *grpc.LinuxMemory) int64 {
	if memory == nil || memory.Limit <= 0 || memory.Limit == math.MaxInt64 {
		return 0
	}
	return memory.Limit
}
