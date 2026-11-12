#!/usr/bin/env bats
#
# Copyright (c) 2026 Kata Containers community
#
# SPDX-License-Identifier: Apache-2.0
#
# A huge page backed guest sized from its pod (enable_hugepages_pod_sizing)

load "${BATS_TEST_DIRNAME}/lib.sh"
load "${BATS_TEST_DIRNAME}/../../common.bash"
load "${BATS_TEST_DIRNAME}/tests_common.sh"

# The container's reservation and the guest's own share, in MiB. Their sum is
# the VM, and it differs from default_memory so the two cannot be mistaken.
container_mib=768
overhead_mib=768
vm_mib=$((container_mib + overhead_mib))

hugepage_pod_sizing_supported() {
	[[ "${KATA_HYPERVISOR}" == "qemu" || "${KATA_HYPERVISOR}" == "clh" || "${KATA_HYPERVISOR}" == "dragonball" ]] || return 1
	[[ "$(uname -m)" == "x86_64" ]] || return 1
	[[ "${KUBERNETES:-}" == "vanilla" ]] || return 1
	return 0
}

host_cmd() {
	exec_host "$node" "nsenter --mount=/proc/1/ns/mnt $*"
}

allocatable_hugepages() {
	kubectl get node "$node" -o jsonpath='{.status.allocatable.hugepages-2Mi}'
}

# The kubelet reads the size of the huge page pool when it starts. Only a pod
# in the host's PID namespace can reach systemd to restart it.
restart_kubelet() {
	local pod="restart-kubelet-${RANDOM}"

	kubectl apply -n kube-system -f - <<POD
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
spec:
  hostPID: true
  restartPolicy: Never
  nodeName: ${node}
  tolerations:
  - operator: Exists
  containers:
  - name: restart
    image: quay.io/bedrock/ubuntu:latest
    command: ["nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--pid", "systemctl", "restart", "kubelet"]
    securityContext:
      privileged: true
POD
	kubectl wait -n kube-system --for=jsonpath='{.status.phase}'=Succeeded --timeout=120s "pod/${pod}" || {
		kubectl describe -n kube-system "pod/${pod}"
		kubectl logs -n kube-system "pod/${pod}"
		kubectl delete -n kube-system "pod/${pod}" --ignore-not-found
		return 1
	}
	kubectl delete -n kube-system "pod/${pod}" --ignore-not-found
}

set_host_hugepages() {
	local pages="$1"
	local expected="$2"

	host_cmd "sysctl -w vm.nr_hugepages=${pages}"
	restart_kubelet || return 1

	local cmd="[[ \"\$(kubectl get node $node -o jsonpath='{.status.allocatable.hugepages-2Mi}')\" == \"${expected}\" ]]"
	waitForProcess 120 5 "$cmd" || return 1
	kubectl wait --for=condition=Ready --timeout=120s "node/${node}" || return 1
}

setup() {
	hugepage_pod_sizing_supported || skip "needs qemu, clh or dragonball on a x86_64 kubeadm node (KATA_HYPERVISOR=${KATA_HYPERVISOR})"

	setup_common || die "setup_common failed"

	[[ "$(host_cmd "grep Hugepagesize /proc/meminfo" | awk '{print $2}')" == "2048" ]] \
		|| skip "the node's default huge page size is not 2 MiB"

	original_pages="$(host_cmd "cat /proc/sys/vm/nr_hugepages")"
	original_allocatable="$(allocatable_hugepages)"
	set_host_hugepages "$((original_pages + vm_mib / 2))" "$((original_pages * 2 + vm_mib))Mi" \
		|| die "Failed to reserve ${vm_mib} MiB of huge pages on node $node"

	local hypervisor_name="${KATA_HYPERVISOR}"
	local dropin_file="${BATS_TEST_TMPDIR}/99-k8s-hugepage-pod-sizing.toml"
	cat > "${dropin_file}" <<DROPIN
[hypervisor.${hypervisor_name}]
enable_hugepages = true
enable_hugepages_pod_sizing = true

[runtime]
static_sandbox_resource_mgmt = true
sandbox_cgroup_only = true
DROPIN
	dropin_path="$(set_kata_runtime_config_dropin_file "$node" "${dropin_file}")" \
		|| die "Failed to install Kata runtime config drop-in on node $node"

	# The guest's own share comes from the runtime class's pod overhead.
	plain_class="$(get_test_runtime_class)"
	sized_class="${plain_class}-hugepage-sized"
	kubectl get runtimeclass "${plain_class}" -o json \
		| jq --arg name "${sized_class}" --arg pages "${overhead_mib}Mi" \
			'{apiVersion, kind, handler, scheduling, overhead} | .metadata.name = $name | .overhead.podFixed["hugepages-2Mi"] = $pages | del(..|nulls)' \
		| kubectl apply -f -
}

@test "A huge page backed guest is sized from its pod" {
	pod_name="hugepage-sized"
	pod_config="${BATS_TEST_TMPDIR}/${pod_name}.yaml"
	cat > "${pod_config}" <<POD
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_name}
spec:
  runtimeClassName: ${sized_class}
  terminationGracePeriodSeconds: 0
  nodeSelector:
    kubernetes.io/hostname: ${node}
  containers:
  - name: app
    image: quay.io/prometheus/busybox:latest
    command: ["tail", "-f", "/dev/null"]
    resources:
      limits:
        hugepages-2Mi: ${container_mib}Mi
        memory: 128Mi
POD

	kubectl create -f "${pod_config}"
	kubectl wait --for=condition=Ready --timeout="$timeout" "pod/${pod_name}"

	# The guest has the pod's reservation, less what its kernel keeps.
	local guest_kb
	guest_kb="$(kubectl exec "${pod_name}" -- awk '/MemTotal/ {print $2}' /proc/meminfo)"
	echo "guest MemTotal: ${guest_kb} kB"
	[[ "${guest_kb}" -le $((vm_mib * 1024)) ]]
	[[ "${guest_kb}" -gt $(((vm_mib - 256) * 1024)) ]]

	# The container's ceiling is its own reservation, not its memory limit.
	local ceiling
	ceiling="$(kubectl exec "${pod_name}" -- cat /sys/fs/cgroup/memory.max)"
	echo "container memory.max: ${ceiling}"
	[[ "${ceiling}" == "$((container_mib * 1024 * 1024))" ]]
}

@test "A pod that reserved no huge pages is refused" {
	pod_name="hugepage-unsized"
	pod_config="${BATS_TEST_TMPDIR}/${pod_name}.yaml"
	cat > "${pod_config}" <<POD
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_name}
spec:
  runtimeClassName: ${plain_class}
  terminationGracePeriodSeconds: 0
  nodeSelector:
    kubernetes.io/hostname: ${node}
  containers:
  - name: app
    image: quay.io/prometheus/busybox:latest
    command: ["tail", "-f", "/dev/null"]
POD

	kubectl create -f "${pod_config}"

	local cmd="kubectl describe pod ${pod_name} | grep -q 'its pod reserved none'"
	waitForProcess 120 5 "$cmd"
	[[ "$(kubectl get pod "${pod_name}" -o jsonpath='{.status.phase}')" == "Pending" ]]
}

teardown() {
	hugepage_pod_sizing_supported || return 0
	[[ -n "${original_pages:-}" ]] || return 0

	kubectl describe pods || true
	k8s_delete_all_pods_if_any_exists || true
	[[ -z "${sized_class:-}" ]] || kubectl delete runtimeclass "${sized_class}" --ignore-not-found || true

	remove_kata_runtime_config_dropin_file "$node" "${dropin_path:-}" \
		|| echo "Warning: Failed to remove Kata runtime config drop-in on node $node"

	set_host_hugepages "${original_pages}" "${original_allocatable}" \
		|| echo "Warning: Failed to give back the huge pages on node $node"

	teardown_common "${node:-}" "${node_start_time:-}"
}
