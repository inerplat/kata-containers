// Copyright (c) 2026 Kata Containers community
//
// SPDX-License-Identifier: Apache-2.0
//

use std::collections::HashMap;

use anyhow::{anyhow, Result};
use oci_spec::runtime::{LinuxHugepageLimit, LinuxResources};
use resource::cpu_mem::hugepage_sizing::holdable_guest_bytes;

pub(crate) const USED_BY_HOST: &str = "host";
pub(crate) const USED_BY_GUEST: &str = "guest";
pub(crate) const ANNOTATION: &str = "io.katacontainers.container.hugepages_used_by";
const MIB: u64 = 1 << 20;

/// What one running container of the pod carries.
#[derive(Clone, Debug, Default)]
pub(crate) struct Reservation {
    pub id: String,
    pub name: String,
    pub huge_pages: u64,
    pub memory: i64,
    pub sandbox: bool,
}

/// What a container was given in the guest.
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub(crate) struct Applied {
    pub ceiling: u64,
    pub pool: u64,
}

/// The pod's huge page state kept across its containers.
#[derive(Debug, Default)]
pub(crate) struct HugePageBook {
    pub annotation: Option<String>,
    pub pool_high_water: u64,
    pub applied: HashMap<String, Applied>,
}

/// The guest a pod sized VM boots, zero when not static.
#[derive(Clone, Copy, Debug, Default)]
pub(crate) struct Guest {
    pub memory_mib: u64,
    pub vcpus: u64,
}

impl Guest {
    fn holdable(&self, pools: u64) -> u64 {
        holdable_guest_bytes(self.memory_mib, self.vcpus).saturating_sub(pools)
    }
}

/// Parses the annotation, "name=guest,name=host".
pub(crate) fn parse_used_by(value: &str) -> Result<HashMap<String, String>> {
    let mut out = HashMap::new();
    for entry in value.split(',').map(str::trim).filter(|e| !e.is_empty()) {
        let (name, user) = entry
            .split_once('=')
            .map(|(n, u)| (n.trim(), u.trim()))
            .filter(|(n, _)| !n.is_empty())
            .ok_or_else(|| {
                anyhow!("annotation {ANNOTATION}: entry {entry:?} is not name=host or name=guest")
            })?;
        if user != USED_BY_HOST && user != USED_BY_GUEST {
            return Err(anyhow!(
                "annotation {ANNOTATION}: container {name:?} has {user:?}; use host or guest"
            ));
        }
        if out.insert(name.to_string(), user.to_string()).is_some() {
            return Err(anyhow!(
                "annotation {ANNOTATION} names container {name:?} twice"
            ));
        }
    }
    Ok(out)
}

/// Who uses a container's huge pages: its pod entry, else the class's; host if it has none.
pub(crate) fn used_by(
    class: &str,
    entries: &HashMap<String, String>,
    r: &Reservation,
    own: u64,
) -> &'static str {
    if r.sandbox {
        return USED_BY_HOST;
    }
    match entries.get(&r.name).map(String::as_str) {
        Some(USED_BY_GUEST) => USED_BY_GUEST,
        Some(_) => USED_BY_HOST,
        None if own != 0 && class == USED_BY_GUEST => USED_BY_GUEST,
        None => USED_BY_HOST,
    }
}

/// The bytes of pages of page_size a container reserved.
pub(crate) fn huge_pages_of(resources: Option<&LinuxResources>, page_size: &str) -> u64 {
    resources
        .and_then(|r| r.hugepage_limits().as_ref())
        .map(|limits| {
            limits
                .iter()
                .filter(|l| l.page_size() == page_size)
                .map(|l| l.limit().max(0) as u64)
                .sum()
        })
        .unwrap_or(0)
}

/// The bytes a memory limit carries, zero for none.
pub(crate) fn memory_limit(resources: Option<&LinuxResources>) -> i64 {
    resources
        .and_then(|r| r.memory().as_ref())
        .and_then(|m| m.limit())
        .filter(|l| *l > 0 && *l != i64::MAX)
        .unwrap_or(0)
}

/// The pod level value the kubelet copied: the largest value when the sum is past the VM.
pub(crate) fn copied_reservation(rs: &[Reservation], vm_bytes: u64) -> u64 {
    if vm_bytes == 0 {
        return 0;
    }
    let largest = rs.iter().map(|r| r.huge_pages).max().unwrap_or(0);
    let sum = rs.iter().fold(0u64, |s, r| s.saturating_add(r.huge_pages));
    let below = rs
        .iter()
        .any(|r| r.huge_pages != 0 && r.huge_pages < largest);
    if sum <= vm_bytes || !below || largest > vm_bytes {
        return 0;
    }
    largest
}

/// The value every container carries past the VM, copy or not.
pub(crate) fn ambiguous_copy(rs: &[Reservation], vm_bytes: u64) -> Option<u64> {
    if vm_bytes == 0 {
        return None;
    }
    let carriers: Vec<u64> = rs
        .iter()
        .map(|r| r.huge_pages)
        .filter(|h| *h != 0)
        .collect();
    let value = *carriers.first()?;
    let sum = carriers.iter().fold(0u64, |s, h| s.saturating_add(*h));
    (carriers.len() > 1
        && carriers.iter().all(|h| *h == value)
        && sum > vm_bytes
        && value <= vm_bytes)
        .then_some(value)
}

pub(crate) fn own(huge_pages: u64, copied: u64) -> u64 {
    if copied != 0 && huge_pages == copied {
        0
    } else {
        huge_pages
    }
}

fn ceiling(r: &Reservation, copied: u64, guest_pool: bool) -> u64 {
    let own = own(r.huge_pages, copied);
    if !guest_pool && own != 0 {
        return own;
    }
    r.memory.max(0) as u64
}

/// The running containers' guest huge pages, a running one counting with what it was given.
pub(crate) fn pod_pools(
    rs: &[Reservation],
    copied: u64,
    pools: &dyn Fn(&Reservation) -> bool,
    applied: &HashMap<String, Applied>,
    this: &str,
) -> u64 {
    rs.iter().fold(0u64, |total, r| {
        let pool = match applied.get(&r.id) {
            Some(a) if r.id != this => a.pool,
            _ if pools(r) => own(r.huge_pages, copied),
            _ => 0,
        };
        total.saturating_add(pool)
    })
}

/// Refuses when the pod's ceilings, as given, and guest huge pages do not fit.
#[allow(clippy::too_many_arguments)]
pub(crate) fn refuse_pod_ceilings(
    rs: &[Reservation],
    copied: u64,
    pools: &dyn Fn(&Reservation) -> bool,
    running: u64,
    held: u64,
    applied: &HashMap<String, Applied>,
    guest: Guest,
    this: &str,
    name: &str,
) -> Result<()> {
    let holdable = guest.holdable(0);
    if holdable == 0 {
        return Ok(());
    }
    let pool_bytes = running.max(held);
    let mut ceilings = 0u64;
    for r in rs {
        let c = match applied.get(&r.id) {
            Some(a) if r.id != this => a.ceiling,
            _ => ceiling(r, copied, pools(r)),
        };
        if c == 0 && pool_bytes != 0 && !r.sandbox {
            return Err(anyhow!(
                "container {} has no memory ceiling in a guest that keeps {} MiB of huge pages: give it a memory limit or huge pages of its own",
                r.name,
                pool_bytes / MIB
            ));
        }
        ceilings = ceilings.saturating_add(c);
    }
    let need = ceilings.saturating_add(pool_bytes);
    if need <= holdable {
        return Ok(());
    }
    let stale = if held > running {
        format!(
            " ({} MiB of them still held for containers that stopped)",
            (held - running) / MIB
        )
    } else {
        String::new()
    };
    Err(anyhow!(
        "container {name} brings the memory ceilings of the pod's containers to {} MiB beside {} MiB of guest huge pages{stale}, but the {} MiB guest holds {} MiB for them: the pod needs {} MiB more of the VM's huge pages, on the pod or as the runtime class's pod overhead",
        ceilings / MIB,
        pool_bytes / MIB,
        guest.memory_mib,
        holdable / MIB,
        ((need - holdable) / MIB).max(1)
    ))
}

fn refuse_ceiling_past_guest(name: &str, ceiling: u64, guest: Guest, pools: u64) -> Result<()> {
    let holdable = guest.holdable(pools);
    if guest.memory_mib == 0 || ceiling <= holdable {
        return Ok(());
    }
    Err(anyhow!(
        "container {name} declares a {} MiB memory ceiling, but the {} MiB guest holds {} MiB for it after {} MiB of huge page pools: the pod needs {} MiB more of the VM's huge pages, on the pod or as the runtime class's pod overhead",
        ceiling / MIB,
        guest.memory_mib,
        holdable / MIB,
        pools / MIB,
        ((ceiling - holdable) / MIB).max(1)
    ))
}

/// Bounds a container in a guest sized from the pod, as host or guest.
pub(crate) fn bound_container(
    resources: &mut LinuxResources,
    page_size: &str,
    guest: Guest,
    pools: u64,
    copied: u64,
    used_by: &str,
    name: &str,
) -> Result<()> {
    if let Some(other) = resources.hugepage_limits().as_ref().and_then(|limits| {
        limits
            .iter()
            .find(|l| l.page_size() != page_size && l.limit() != 0)
            .map(|l| l.page_size().clone())
    }) {
        return Err(anyhow!(
            "container {name} reserves huge pages of {other}, but the VM's memory is on {page_size} pages: only {page_size} huge pages can be used in this runtime class"
        ));
    }
    if used_by == USED_BY_GUEST {
        return refuse_ceiling_past_guest(name, memory_limit(Some(resources)) as u64, guest, pools);
    }
    let own = own(huge_pages_of(Some(resources), page_size), copied);
    if let Some(limits) = resources.hugepage_limits().clone() {
        let stripped: Vec<LinuxHugepageLimit> = limits
            .into_iter()
            .map(|mut l| {
                if l.page_size() == page_size {
                    l.set_limit(0);
                }
                l
            })
            .collect();
        resources.set_hugepage_limits(Some(stripped));
    }
    if own == 0 {
        return refuse_ceiling_past_guest(name, memory_limit(Some(resources)) as u64, guest, pools);
    }
    let mut memory = resources.memory().unwrap_or_default();
    let limit = own.min(i64::MAX as u64) as i64;
    memory.set_limit(Some(limit));
    if memory.reservation().is_some_and(|r| r > limit) {
        memory.set_reservation(Some(limit));
    }
    if memory.swap().is_some_and(|s| s != 0 && s != limit) {
        memory.set_swap(Some(limit));
    }
    resources.set_memory(Some(memory));
    refuse_ceiling_past_guest(name, own, guest, pools)
}

/// Keeps one entry per page size, the last, and the held one where a resize says zero.
pub(crate) fn updated_limits(
    held: Option<&Vec<LinuxHugepageLimit>>,
    update: Option<&Vec<LinuxHugepageLimit>>,
) -> Option<Vec<LinuxHugepageLimit>> {
    let update = match update {
        Some(u) if !u.is_empty() => u,
        _ => return held.cloned(),
    };
    let mut sizes: Vec<String> = Vec::new();
    let mut held_by: HashMap<String, i64> = HashMap::new();
    let mut update_by: HashMap<String, i64> = HashMap::new();
    for l in held.into_iter().flatten() {
        if !sizes.contains(l.page_size()) {
            sizes.push(l.page_size().clone());
        }
        held_by.insert(l.page_size().clone(), l.limit());
    }
    for l in update {
        if !sizes.contains(l.page_size()) {
            sizes.push(l.page_size().clone());
        }
        update_by.insert(l.page_size().clone(), l.limit());
    }
    Some(
        sizes
            .into_iter()
            .map(|size| {
                let limit = match update_by.get(&size) {
                    Some(&v) if v != 0 => v,
                    _ => held_by.get(&size).copied().unwrap_or(0),
                };
                let mut l = LinuxHugepageLimit::default();
                l.set_page_size(size);
                l.set_limit(limit);
                l
            })
            .collect(),
    )
}

/// Puts the held reservation back into an update that names none of the VM's page size.
pub(crate) fn fold_held(resources: &mut LinuxResources, page_size: &str, held: u64) {
    if held == 0 {
        return;
    }
    let mut limits = resources.hugepage_limits().clone().unwrap_or_default();
    if limits.iter().any(|l| l.page_size() == page_size) {
        return;
    }
    let mut l = LinuxHugepageLimit::default();
    l.set_page_size(page_size.to_string());
    l.set_limit(held.min(i64::MAX as u64) as i64);
    limits.push(l);
    resources.set_hugepage_limits(Some(limits));
}

#[cfg(test)]
mod tests {
    use super::*;
    use oci_spec::runtime::LinuxMemory;

    const GIB: u64 = 1 << 30;

    fn r(id: &str, huge: u64, mem: u64) -> Reservation {
        Reservation {
            id: id.to_string(),
            name: id.to_string(),
            huge_pages: huge,
            memory: mem as i64,
            sandbox: false,
        }
    }

    fn res(huge: u64, mem: u64) -> LinuxResources {
        let mut l = LinuxHugepageLimit::default();
        l.set_page_size("1GB".to_string());
        l.set_limit(huge as i64);
        let mut m = LinuxMemory::default();
        m.set_limit(Some(mem as i64));
        let mut out = LinuxResources::default();
        out.set_hugepage_limits(Some(vec![l]));
        out.set_memory(Some(m));
        out
    }

    #[test]
    fn test_parse_used_by() {
        let m = parse_used_by(" dpdk=guest , app=host ").unwrap();
        assert_eq!(m.get("dpdk").unwrap(), USED_BY_GUEST);
        assert_eq!(m.get("app").unwrap(), USED_BY_HOST);
        assert!(parse_used_by("").unwrap().is_empty());
        for bad in ["dpdk", "=guest", "dpdk=pool", "a=guest,a=host"] {
            assert!(parse_used_by(bad).is_err(), "{}", bad);
        }
    }

    #[test]
    fn test_used_by() {
        let e = parse_used_by("dpdk=guest").unwrap();
        assert_eq!(
            used_by("", &e, &r("dpdk", 2 * GIB, 0), 2 * GIB),
            USED_BY_GUEST
        );
        assert_eq!(
            used_by("", &e, &r("app", 8 * GIB, 0), 8 * GIB),
            USED_BY_HOST
        );
        assert_eq!(
            used_by(USED_BY_GUEST, &HashMap::new(), &r("log", 0, 0), 0),
            USED_BY_HOST,
            "nothing of its own"
        );
        assert_eq!(
            used_by(USED_BY_GUEST, &HashMap::new(), &r("c", 2 * GIB, 0), 2 * GIB),
            USED_BY_GUEST
        );
        let mut pause = r("pause", 0, 0);
        pause.sandbox = true;
        assert_eq!(
            used_by(USED_BY_GUEST, &HashMap::new(), &pause, 0),
            USED_BY_HOST
        );
    }

    #[test]
    fn test_copied_reservation() {
        let rs = [
            r("main", 10 * GIB, 0),
            r("s1", 16 * GIB, 0),
            r("s2", 16 * GIB, 0),
        ];
        assert_eq!(copied_reservation(&rs, 18 * GIB), 16 * GIB);
        assert_eq!(
            copied_reservation(&[r("a", 12 * GIB, 0)], 12 * GIB),
            0,
            "lone container"
        );
        assert_eq!(
            copied_reservation(&[r("a", 8 * GIB, 0), r("b", GIB, 0)], 13 * GIB),
            0,
            "declarations"
        );
        assert_eq!(
            copied_reservation(&[r("a", 16 * GIB, 0), r("b", 16 * GIB, 0)], 18 * GIB),
            0,
            "cannot tell"
        );
        assert_eq!(
            ambiguous_copy(&[r("a", 16 * GIB, 0), r("b", 16 * GIB, 0)], 18 * GIB),
            Some(16 * GIB)
        );
        assert_eq!(
            ambiguous_copy(&[r("a", 4 * GIB, 0), r("b", 4 * GIB, 0)], 2 * GIB),
            None,
            "not sized from the pod"
        );
        assert_eq!(
            ambiguous_copy(&[r("a", 8 * GIB, 0), r("b", 8 * GIB, 0)], 18 * GIB),
            None,
            "fits"
        );
    }

    #[test]
    fn test_refuse_pod_ceilings() {
        let none = HashMap::new();
        let no_pool = |_: &Reservation| false;
        let g = |mib, vcpus| Guest {
            memory_mib: mib,
            vcpus,
        };
        let rs = [r("app", 16 * GIB, 1 << 20), r("logging", 2 * GIB, 1 << 20)];
        assert!(refuse_pod_ceilings(
            &rs,
            0,
            &no_pool,
            0,
            0,
            &none,
            g(20480, 6),
            "logging",
            "logging"
        )
        .is_ok());
        let rs = [r("app", 16 * GIB, 2 * GIB), r("logging", 0, 2 * GIB)];
        let err = refuse_pod_ceilings(
            &rs,
            0,
            &no_pool,
            0,
            0,
            &none,
            g(18432, 5),
            "logging",
            "logging",
        )
        .unwrap_err();
        assert!(err.to_string().contains("the pod needs"));
        let rs = [r("main", 8 * GIB, 1 << 20)];
        let err = refuse_pod_ceilings(
            &rs,
            0,
            &no_pool,
            0,
            2 * GIB,
            &none,
            g(10240, 3),
            "main",
            "main",
        )
        .unwrap_err();
        assert!(err
            .to_string()
            .contains("still held for containers that stopped"));
        let rs = [r("a", 16 * GIB, 512 << 20), r("b", 10 * GIB, 1 << 20)];
        let applied = HashMap::from([(
            "a".to_string(),
            Applied {
                ceiling: 16 * GIB,
                pool: 0,
            },
        )]);
        assert!(refuse_pod_ceilings(
            &rs,
            16 * GIB,
            &no_pool,
            0,
            0,
            &applied,
            g(24576, 3),
            "b",
            "b"
        )
        .is_err());
        assert!(
            refuse_pod_ceilings(&rs, 16 * GIB, &no_pool, 0, 0, &none, g(24576, 3), "b", "b")
                .is_ok(),
            "without the record the copy reads as 512Mi"
        );
        let rs = [r("pool", 2 * GIB, 256 << 20), r("hog", 0, 0)];
        let err = refuse_pod_ceilings(
            &rs,
            0,
            &|r: &Reservation| r.id == "pool",
            2 * GIB,
            0,
            &none,
            g(12288, 4),
            "hog",
            "hog",
        )
        .unwrap_err();
        assert!(err.to_string().contains("has no memory ceiling"));
        assert!(refuse_pod_ceilings(
            &[r("a", 64 * GIB, 0)],
            0,
            &no_pool,
            0,
            0,
            &none,
            Guest::default(),
            "a",
            "a"
        )
        .is_ok());
    }

    #[test]
    fn test_pod_pools() {
        let rs = [r("a", 16 * GIB, 0), r("b", 10 * GIB, 0)];
        let applied = HashMap::from([(
            "a".to_string(),
            Applied {
                ceiling: 512 << 20,
                pool: 16 * GIB,
            },
        )]);
        let b_pool = |r: &Reservation| r.id == "b";
        assert_eq!(pod_pools(&rs, 16 * GIB, &b_pool, &applied, "b"), 26 * GIB);
        assert_eq!(
            pod_pools(&rs, 16 * GIB, &b_pool, &HashMap::new(), "b"),
            10 * GIB
        );
    }

    #[test]
    fn test_bound_container() {
        let g = Guest {
            memory_mib: 16384,
            vcpus: 4,
        };
        let mut host = res(8 * GIB, 1 << 20);
        bound_container(&mut host, "1GB", g, 0, 0, USED_BY_HOST, "main").unwrap();
        assert_eq!(memory_limit(Some(&host)), (8 * GIB) as i64);
        assert_eq!(
            huge_pages_of(Some(&host), "1GB"),
            0,
            "a host container takes no pool pages"
        );

        let mut guest = res(4 * GIB, 2 * GIB);
        bound_container(&mut guest, "1GB", g, 4 * GIB, 0, USED_BY_GUEST, "dpdk").unwrap();
        assert_eq!(memory_limit(Some(&guest)), (2 * GIB) as i64);
        assert_eq!(huge_pages_of(Some(&guest), "1GB"), 4 * GIB);

        let mut copy = res(16 * GIB, 512 << 20);
        bound_container(&mut copy, "1GB", g, 0, 16 * GIB, USED_BY_HOST, "s1").unwrap();
        assert_eq!(
            memory_limit(Some(&copy)),
            512 << 20,
            "a copy is not a ceiling"
        );

        let mut too_big = res(16 * GIB, 1 << 20);
        let err = bound_container(&mut too_big, "1GB", g, 0, 0, USED_BY_HOST, "app").unwrap_err();
        assert!(err.to_string().contains("1279 MiB more"), "{}", err);

        let mut other = res(0, 1 << 30);
        let mut l = LinuxHugepageLimit::default();
        l.set_page_size("2MB".to_string());
        l.set_limit(2 * GIB as i64);
        other.set_hugepage_limits(Some(vec![l]));
        assert!(bound_container(&mut other, "1GB", g, 0, 0, USED_BY_HOST, "x").is_err());
    }

    #[test]
    fn test_updated_limits() {
        let held = res(8 * GIB, 0).hugepage_limits().clone().unwrap();
        let mut zero = LinuxHugepageLimit::default();
        zero.set_page_size("1GB".to_string());
        zero.set_limit(0);
        let update = vec![held[0].clone(), zero];
        let out = updated_limits(Some(&held), Some(&update)).unwrap();
        assert_eq!(out.len(), 1);
        assert_eq!(
            out[0].limit(),
            (8 * GIB) as i64,
            "a resize keeps the held pages"
        );
        assert_eq!(
            updated_limits(Some(&held), None).unwrap()[0].limit(),
            (8 * GIB) as i64
        );
    }

    #[test]
    fn test_fold_held() {
        let mut update = LinuxResources::default();
        let mut m = LinuxMemory::default();
        m.set_limit(Some(16 << 20));
        update.set_memory(Some(m));
        fold_held(&mut update, "1GB", 8 * GIB);
        assert_eq!(huge_pages_of(Some(&update), "1GB"), 8 * GIB);
        let mut named = res(4 * GIB, 0);
        fold_held(&mut named, "1GB", 8 * GIB);
        assert_eq!(huge_pages_of(Some(&named), "1GB"), 4 * GIB);
    }
}
