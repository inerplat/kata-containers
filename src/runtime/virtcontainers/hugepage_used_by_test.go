// Copyright (c) 2026 Kata Containers community
//
// SPDX-License-Identifier: Apache-2.0
//

package virtcontainers

import (
	"path/filepath"
	"testing"

	ctrAnnotations "github.com/containerd/containerd/pkg/cri/annotations"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"

	pb "github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/agent/protocols/grpc"
	vcAnnotations "github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/annotations"
)

func TestParseHugePagesUsedBy(t *testing.T) {
	assert := assert.New(t)

	usedBy, err := parseHugePagesUsedBy(" nvmf=guest , trainer=host ")
	assert.NoError(err)
	assert.Equal(map[string]string{"nvmf": HugePagesUsedByGuest, "trainer": HugePagesUsedByHost}, usedBy)

	usedBy, err = parseHugePagesUsedBy("")
	assert.NoError(err)
	assert.Empty(usedBy)

	for _, bad := range []string{"nvmf", "=guest", "nvmf=pool", "nvmf=guest,nvmf=host"} {
		_, err := parseHugePagesUsedBy(bad)
		assert.Error(err, bad)
	}

	assert.True(validHugePagesUsedBy(""))
	assert.True(validHugePagesUsedBy(HugePagesUsedByGuest))
	assert.False(validHugePagesUsedBy("pool"))
}

// A sandbox whose pause container carries the pod's annotations.
func usedBySandbox(classUsedBy, annotation string) *Sandbox {
	annotations := map[string]string{}
	if annotation != "" {
		annotations[vcAnnotations.ContainerHugePagesUsedBy] = annotation
	}
	return &Sandbox{config: &SandboxConfig{
		HypervisorConfig: HypervisorConfig{
			HugePages:          true,
			HugePagesPodSizing: true,
			HugePagesUsedBy:    classUsedBy,
		},
		Containers: []ContainerConfig{{
			ID:          "pause",
			Annotations: map[string]string{vcAnnotations.ContainerTypeKey: string(PodSandbox)},
			CustomSpec:  &specs.Spec{Annotations: annotations},
		}},
	}}
}

func usedByContainer(name, annotation string) *ContainerConfig {
	annotations := map[string]string{}
	if annotation != "" {
		annotations[vcAnnotations.ContainerHugePagesUsedBy] = annotation
	}
	return &ContainerConfig{
		ID:          name + "-id",
		Annotations: map[string]string{ctrAnnotations.ContainerName: name},
		CustomSpec:  &specs.Spec{Annotations: annotations},
	}
}

func TestContainerHugePagesUsedBy(t *testing.T) {
	const own = uint64(2) << 30
	assert := assert.New(t)

	for _, tt := range []struct {
		description string
		class       string
		annotation  string
		container   string
		own         uint64
		expected    string
	}{
		{"the default class is host", "", "", "app", own, HugePagesUsedByHost},
		{"a guest class gives every container that reserves pages to the guest", HugePagesUsedByGuest, "", "dpdk", own, HugePagesUsedByGuest},
		{"a guest class leaves a container that reserves none to the host", HugePagesUsedByGuest, "", "log", 0, HugePagesUsedByHost},
		{"a host class with a guest entry", HugePagesUsedByHost, "nvmf=guest", "nvmf", own, HugePagesUsedByGuest},
		{"a host class leaves unnamed containers alone", HugePagesUsedByHost, "nvmf=guest", "trainer", own, HugePagesUsedByHost},
		{"a guest class with a host entry", HugePagesUsedByGuest, "accel=host", "accel", own, HugePagesUsedByHost},
		{"an entry holds even for a container that reserves none", HugePagesUsedByHost, "nvmf=guest", "nvmf", 0, HugePagesUsedByGuest},
	} {
		sandbox := usedBySandbox(tt.class, tt.annotation)
		usedBy, err := containerHugePagesUsedBy(sandbox, usedByContainer(tt.container, tt.annotation), tt.own)
		assert.NoError(err, tt.description)
		assert.Equal(tt.expected, usedBy, tt.description)
	}

	pause := usedByContainer("pause", "")
	pause.Annotations[vcAnnotations.ContainerTypeKey] = string(PodSandbox)
	usedBy, err := containerHugePagesUsedBy(usedBySandbox(HugePagesUsedByGuest, ""), pause, own)
	assert.NoError(err)
	assert.Equal(HugePagesUsedByHost, usedBy, "a guest class does not give the sandbox container's pages to the guest")

	// The kubelet rebuilds a restarted container from the pod's current
	// annotations; the sandbox keeps the ones it started with.
	sandbox := usedBySandbox(HugePagesUsedByHost, "nvmf=guest")
	_, err = containerHugePagesUsedBy(sandbox, usedByContainer("nvmf", "nvmf=host"), own)
	assert.Error(err)
	assert.Contains(err.Error(), "cannot change while the pod runs")

	_, err = containerHugePagesUsedBy(usedBySandbox("", "nvmf=pool"), usedByContainer("nvmf", "nvmf=pool"), own)
	assert.Error(err)

	// In a guest sized from the pod the entry, not a mount, decides the pool.
	assert.True(containerHasGuestHugePagePool(sandbox, usedByContainer("nvmf", "nvmf=guest"), own))
	assert.False(containerHasGuestHugePagePool(sandbox, usedByContainer("trainer", "nvmf=guest"), own))
}

func TestGuestPoolTotalNeverShrinks(t *testing.T) {
	assert := assert.New(t)
	k := &kataAgent{}

	assert.Equal(uint64(4<<30), k.guestPoolTotal("1GB", 4<<30))
	assert.Equal(uint64(12<<30), k.guestPoolTotal("1GB", 12<<30))
	// A pool container that stopped no longer counts, but its pages may still be
	// held; the pool is not asked to shrink.
	assert.Equal(uint64(12<<30), k.guestPoolTotal("1GB", 8<<30))
	assert.Equal(uint64(2<<20), k.guestPoolTotal("2MB", 2<<20))
}

func TestGuestPoolStorage(t *testing.T) {
	assert := assert.New(t)

	s := guestPoolStorage("abc", 1<<30, 12<<30)
	assert.Equal("hugetlbfs", s.Fstype)
	assert.Equal(KataEphemeralDevType, s.Driver)
	assert.Equal(filepath.Join(ephemeralPath(), "kata-hugepage-pool-abc"), s.MountPoint)
	assert.Equal([]string{"pagesize=1073741824,size=12884901888"}, s.Options)
}

func TestStripPoolPagesAndOtherPageSize(t *testing.T) {
	assert := assert.New(t)

	limits := []*pb.LinuxHugepageLimit{
		{Pagesize: "2MB", Limit: 0},
		{Pagesize: "1GB", Limit: 8 << 30},
	}
	assert.Equal("", otherPageSizeReserved(limits, "1GB"))
	stripPoolPages(limits, "1GB")
	assert.Equal(uint64(0), limits[1].Limit)

	limits = append(limits, &pb.LinuxHugepageLimit{Pagesize: "2MB", Limit: 2 << 30})
	assert.Equal("2MB", otherPageSizeReserved(limits, "1GB"))
}

func TestBoundContainerInGuest(t *testing.T) {
	const gib = int64(1) << 30
	saved := readVMHugepageSize
	t.Cleanup(func() { readVMHugepageSize = saved })
	readVMHugepageSize = func() (uint64, error) { return 1 << 30, nil }

	guest := guestCapacity{memoryMB: 16 * 1024, vCPUs: 4}
	k := &kataAgent{}

	for _, tt := range []struct {
		description   string
		usedBy        string
		copied        uint64
		guest         guestCapacity
		resources     *pb.LinuxResources
		refused       string
		expectedLimit int64
		expectedHuge  uint64
	}{
		{
			description: "a host container is bounded by its reservation and takes no pool pages",
			usedBy:      HugePagesUsedByHost,
			guest:       guest,
			resources: &pb.LinuxResources{
				Memory:         &pb.LinuxMemory{Limit: 1 << 20},
				HugepageLimits: []*pb.LinuxHugepageLimit{{Pagesize: "1GB", Limit: 8 << 30}},
			},
			expectedLimit: 8 * gib,
			expectedHuge:  0,
		},
		{
			description: "a guest container keeps its reservation as its guest hugetlb limit",
			usedBy:      HugePagesUsedByGuest,
			guest:       guest,
			resources: &pb.LinuxResources{
				Memory:         &pb.LinuxMemory{Limit: 2 * gib},
				HugepageLimits: []*pb.LinuxHugepageLimit{{Pagesize: "1GB", Limit: 4 << 30}},
			},
			expectedLimit: 2 * gib,
			expectedHuge:  4 << 30,
		},
		{
			description: "a guest container's memory ceiling must fit after the pools",
			usedBy:      HugePagesUsedByGuest,
			guest:       guestCapacity{memoryMB: 16 * 1024, vCPUs: 4, pools: 12 << 30},
			resources: &pb.LinuxResources{
				Memory:         &pb.LinuxMemory{Limit: 4 * gib},
				HugepageLimits: []*pb.LinuxHugepageLimit{{Pagesize: "1GB", Limit: 12 << 30}},
			},
			refused: "the pod needs",
		},
		{
			description: "a copied reservation leaves the memory limit as the ceiling",
			usedBy:      HugePagesUsedByHost,
			copied:      16 << 30,
			guest:       guest,
			resources: &pb.LinuxResources{
				Memory:         &pb.LinuxMemory{Limit: 512 << 20},
				HugepageLimits: []*pb.LinuxHugepageLimit{{Pagesize: "1GB", Limit: 16 << 30}},
			},
			expectedLimit: 512 << 20,
			expectedHuge:  0,
		},
		{
			description: "a reservation of another page size is refused",
			usedBy:      HugePagesUsedByGuest,
			guest:       guest,
			resources: &pb.LinuxResources{
				Memory:         &pb.LinuxMemory{Limit: gib},
				HugepageLimits: []*pb.LinuxHugepageLimit{{Pagesize: "2MB", Limit: 2 << 30}},
			},
			refused: "only 1GB huge pages",
		},
	} {
		assert := assert.New(t)
		err := k.boundContainerInGuest(tt.resources, tt.guest, tt.copied, tt.usedBy, "app")
		if tt.refused != "" {
			assert.Error(err, tt.description)
			assert.Contains(err.Error(), tt.refused, tt.description)
			continue
		}
		assert.NoError(err, tt.description)
		assert.Equal(tt.expectedLimit, tt.resources.Memory.Limit, tt.description)
		assert.Equal(tt.expectedHuge, hugePagesTotal(tt.resources.HugepageLimits, "1GB"), tt.description)
	}
}

func TestFoldHeldHugePages(t *testing.T) {
	const gib = int64(1) << 30
	assert := assert.New(t)
	saved := readVMHugepageSize
	t.Cleanup(func() { readVMHugepageSize = saved })
	readVMHugepageSize = func() (uint64, error) { return 1 << 30, nil }

	// An update for the memory alone keeps a host container at its reservation
	// rather than at its nominal memory limit.
	resources := &pb.LinuxResources{Memory: &pb.LinuxMemory{Limit: 16 << 20}}
	foldHeldHugePages(resources, "1GB", 8<<30)
	k := &kataAgent{}
	assert.NoError(k.boundContainerInGuest(resources, guestCapacity{memoryMB: 16 * 1024, vCPUs: 4}, 0, HugePagesUsedByHost, "main"))
	assert.Equal(8*gib, resources.Memory.Limit)

	// An update that names the page size, zero or not, is left as it is.
	resources = &pb.LinuxResources{HugepageLimits: []*pb.LinuxHugepageLimit{{Pagesize: "1GB", Limit: 4 << 30}}}
	foldHeldHugePages(resources, "1GB", 8<<30)
	assert.Len(resources.HugepageLimits, 1)
	assert.Equal(uint64(4<<30), resources.HugepageLimits[0].Limit)

	resources = &pb.LinuxResources{}
	foldHeldHugePages(resources, "1GB", 0)
	assert.Empty(resources.HugepageLimits)
}

func TestGuestCeilingBytes(t *testing.T) {
	const gib = uint64(1) << 30
	assert := assert.New(t)

	assert.Equal(16*gib, guestCeilingBytes(podReservation{hugePages: 16 * gib, memory: 1 << 20}, 0), "a host container is bounded by its pages")
	assert.Equal(2*gib, guestCeilingBytes(podReservation{memory: int64(2 * gib)}, 0), "a container without pages is bounded by its memory limit")
	assert.Equal(512*uint64(1<<20), guestCeilingBytes(podReservation{hugePages: 2 * gib, memory: 512 << 20, pool: true}, 0), "a guest container is bounded by its memory limit")
	assert.Equal(256*uint64(1<<20), guestCeilingBytes(podReservation{hugePages: 16 * gib, memory: 256 << 20}, 16*gib), "a copy is not a ceiling")
	assert.Equal(uint64(0), guestCeilingBytes(podReservation{}, 0), "no limit, no ceiling")
}

func TestRefusePodCeilingsPastGuest(t *testing.T) {
	const gib = uint64(1) << 30
	const mib = int64(1) << 20
	assert := assert.New(t)

	for _, tt := range []struct {
		description  string
		reservations []podReservation
		copied       uint64
		running      uint64
		held         uint64
		applied      map[string]appliedShare
		guest        guestCapacity
		refused      string
	}{
		{
			description:  "every container declares its pages and the pod leaves room for the guest",
			reservations: []podReservation{{name: "app", hugePages: 16 * gib, memory: mib}, {name: "logging", hugePages: 2 * gib, memory: mib}, {name: "metrics", hugePages: 2 * gib, memory: mib}},
			guest:        guestCapacity{memoryMB: 22 * 1024, vCPUs: 6},
		},
		{
			description:  "a sidecar's memory is not in the VM's size",
			reservations: []podReservation{{name: "app", hugePages: 16 * gib, memory: 2048 * mib}, {name: "logging", memory: 2048 * mib}},
			guest:        guestCapacity{memoryMB: 18 * 1024, vCPUs: 5},
			refused:      "the pod needs",
		},
		{
			description:  "a host container and a guest container's pages",
			reservations: []podReservation{{name: "c1", hugePages: 14 * gib, memory: mib}, {name: "c2", hugePages: 2 * gib, memory: 512 * mib, pool: true}},
			running:      2 * gib,
			guest:        guestCapacity{memoryMB: 16 * 1024, vCPUs: 3},
			refused:      "the pod needs",
		},
		{
			description:  "the same pod with room in the overhead",
			reservations: []podReservation{{name: "c1", hugePages: 14 * gib, memory: mib}, {name: "c2", hugePages: 2 * gib, memory: 512 * mib, pool: true}},
			running:      2 * gib,
			guest:        guestCapacity{memoryMB: 18 * 1024, vCPUs: 3},
		},
		{
			description:  "pages the guest still holds from an init container",
			reservations: []podReservation{{name: "main", hugePages: 8 * gib, memory: mib}},
			held:         2 * gib,
			guest:        guestCapacity{memoryMB: 10 * 1024, vCPUs: 3},
			refused:      "still held for containers that stopped",
		},
		{
			// a was created alone and given the pod level copy as its ceiling;
			// once b arrives the values reveal the copy, but a keeps what it got.
			description:  "a running container counts with the ceiling it was given",
			reservations: []podReservation{{id: "a", name: "a", hugePages: 16 * gib, memory: 512 * mib}, {id: "self", name: "b", hugePages: 10 * gib, memory: mib}},
			copied:       16 * gib,
			applied:      map[string]appliedShare{"a": {ceiling: 16 * gib}},
			guest:        guestCapacity{memoryMB: 24 * 1024, vCPUs: 3},
			refused:      "the pod needs",
		},
		{
			description:  "the container being checked counts with its new ceiling",
			reservations: []podReservation{{id: "self", name: "app", hugePages: 4 * gib, memory: mib}},
			applied:      map[string]appliedShare{"self": {ceiling: 64 * gib}},
			guest:        guestCapacity{memoryMB: 6 * 1024, vCPUs: 2},
		},
		{
			description:  "a guest of unknown size settles nothing",
			reservations: []podReservation{{name: "app", hugePages: 64 * gib}},
		},
		{
			description:  "a container without a ceiling beside guest huge pages",
			reservations: []podReservation{{name: "pause", sandbox: true}, {name: "pool", hugePages: 4 * gib, memory: 256 * mib, pool: true}, {name: "hog"}},
			running:      4 * gib,
			guest:        guestCapacity{memoryMB: 8 * 1024, vCPUs: 2},
			refused:      "container hog has no memory ceiling",
		},
		{
			description:  "the sandbox container and an unbounded container without guest huge pages",
			reservations: []podReservation{{name: "pause", sandbox: true}, {name: "app", hugePages: 4 * gib}, {name: "side"}},
			guest:        guestCapacity{memoryMB: 6 * 1024, vCPUs: 2},
		},
		{
			// A pod level 8Gi copied into b reads as b's own while the sum fits.
			description:  "a copy the values cannot reveal is refused as a declaration",
			reservations: []podReservation{{name: "a", hugePages: 1 * gib, memory: 512 * mib}, {name: "b", hugePages: 8 * gib, memory: 512 * mib}},
			guest:        guestCapacity{memoryMB: 10 * 1024, vCPUs: 3},
			refused:      "the pod needs 123 MiB more",
		},
	} {
		err := refusePodCeilingsPastGuest(tt.reservations, tt.copied, tt.running, tt.held, tt.applied, tt.guest, "self", "x")
		if tt.refused != "" {
			assert.Error(err, tt.description)
			assert.Contains(err.Error(), tt.refused, tt.description)
			continue
		}
		assert.NoError(err, tt.description)
	}
}

func TestGuestPoolsHeld(t *testing.T) {
	const gib = uint64(1) << 30
	assert := assert.New(t)
	k := &kataAgent{}

	assert.Equal(uint64(0), k.guestPoolsHeld("1GB"))
	k.guestPoolTotal("1GB", 6*gib)
	assert.Equal(6*gib, k.guestPoolsHeld("1GB"), "pages the guest still holds")
	assert.Equal(6*gib, k.guestPoolsHeld("1GB"), "reading does not raise it")
	assert.Equal(uint64(0), k.guestPoolsHeld("2MB"))
}

func TestRelinkContainerConfigs(t *testing.T) {
	assert := assert.New(t)
	s := &Sandbox{config: &SandboxConfig{HypervisorConfig: HypervisorConfig{HugePages: true, HugePagesPodSizing: true}}, containers: map[string]*Container{}}
	for _, id := range []string{"a", "b", "c"} {
		s.config.Containers = append(s.config.Containers, ContainerConfig{ID: id})
		s.relinkContainerConfigs()
		s.containers[id] = &Container{id: id, config: &s.config.Containers[len(s.config.Containers)-1]}
	}
	s.config.Containers = append(s.config.Containers[:0], s.config.Containers[1:]...)
	delete(s.containers, "a")
	s.relinkContainerConfigs()
	for _, id := range []string{"b", "c"} {
		assert.Equal(id, s.containers[id].config.ID, "a delete shifts the entries after it")
	}
}

func TestAmbiguousCopiedReservation(t *testing.T) {
	const gib = uint64(1) << 30
	assert := assert.New(t)

	value, ambiguous := ambiguousCopiedReservation([]podReservation{{hugePages: 16 * gib}, {hugePages: 16 * gib}}, 18*gib)
	assert.True(ambiguous, "the same value twice past the VM")
	assert.Equal(16*gib, value)

	_, ambiguous = ambiguousCopiedReservation([]podReservation{{hugePages: 10 * gib}, {hugePages: 16 * gib}, {hugePages: 16 * gib}}, 18*gib)
	assert.False(ambiguous, "a smaller value tells the copies apart")
	_, ambiguous = ambiguousCopiedReservation([]podReservation{{hugePages: 16 * gib}}, 18*gib)
	assert.False(ambiguous, "a lone container")
	_, ambiguous = ambiguousCopiedReservation([]podReservation{{hugePages: 8 * gib}, {hugePages: 8 * gib}, {}}, 18*gib)
	assert.False(ambiguous, "values that fit the VM are declarations")
	_, ambiguous = ambiguousCopiedReservation([]podReservation{{hugePages: 8 * gib}, {hugePages: 8 * gib}}, 0)
	assert.False(ambiguous, "a guest of unknown size settles nothing")
	_, ambiguous = ambiguousCopiedReservation([]podReservation{{hugePages: 4 * gib}, {hugePages: 4 * gib}}, 2*gib)
	assert.False(ambiguous, "a VM smaller than the value was not sized from the pod")
}

func TestRecordApplied(t *testing.T) {
	const gib = uint64(1) << 30
	assert := assert.New(t)
	k := &kataAgent{}
	k.recordApplied("a", &pb.LinuxResources{Memory: &pb.LinuxMemory{Limit: int64(2 * gib)}}, 4*gib)
	assert.Equal(appliedShare{ceiling: 2 * gib, pool: 4 * gib}, k.appliedCopy()["a"])
	k.recordApplied("a", &pb.LinuxResources{Memory: &pb.LinuxMemory{Limit: int64(8 * gib)}}, k.appliedPool("a"))
	assert.Equal(appliedShare{ceiling: 8 * gib, pool: 4 * gib}, k.appliedCopy()["a"], "an update keeps the pool")
	k.recordApplied("u", nil, 0)
	assert.Equal(appliedShare{}, k.appliedCopy()["u"], "no limit, no ceiling")
}

func TestPodPoolsBytesKeepsWhatRunningContainersGot(t *testing.T) {
	const gib = uint64(1) << 30
	assert := assert.New(t)
	// a was given a 16Gi pool before b revealed its value as the pod level copy.
	reservations := []podReservation{{id: "a", name: "a", hugePages: 16 * gib}, {id: "b", name: "b", hugePages: 10 * gib, pool: true}}
	applied := map[string]appliedShare{"a": {ceiling: 512 << 20, pool: 16 * gib}}
	assert.Equal(26*gib, podPoolsBytes(reservations, 16*gib, applied, "b"))
	assert.Equal(10*gib, podPoolsBytes(reservations, 16*gib, nil, "b"))
}

func TestRelinkAfterRefusedDuplicate(t *testing.T) {
	assert := assert.New(t)
	s := &Sandbox{config: &SandboxConfig{HypervisorConfig: HypervisorConfig{HugePages: true, HugePagesPodSizing: true}}, containers: map[string]*Container{}}
	s.config.Containers = append(s.config.Containers, ContainerConfig{ID: "a", Annotations: map[string]string{"v": "kept"}})
	s.containers["a"] = &Container{id: "a"}
	s.relinkContainerConfigs()
	// A second create with the same ID appends, relinks, then rolls back.
	s.config.Containers = append(s.config.Containers, ContainerConfig{ID: "a", Annotations: map[string]string{"v": "refused"}})
	s.relinkContainerConfigs()
	s.config.Containers = s.config.Containers[:len(s.config.Containers)-1]
	s.relinkContainerConfigs()
	assert.Equal("kept", s.containers["a"].config.Annotations["v"])
}

func TestRelinkLeavesASandboxNotSizedFromThePod(t *testing.T) {
	assert := assert.New(t)
	s := &Sandbox{config: &SandboxConfig{}, containers: map[string]*Container{}}
	s.config.Containers = []ContainerConfig{{ID: "a"}}
	held := &ContainerConfig{ID: "a"}
	s.containers["a"] = &Container{id: "a", config: held}
	s.relinkContainerConfigs()
	assert.Same(held, s.containers["a"].config, "main behaviour is kept with the option off")
}
