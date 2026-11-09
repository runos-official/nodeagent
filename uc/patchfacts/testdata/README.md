# Where these fixtures come from

Every fixture here is real command output. A fixture somebody invents tests the
author's expectation and nothing else, so the provenance of each file is
recorded below, and so is every change made to the captured bytes.

## Captured on a running RunOS node, Ubuntu 24.04.4, a control plane

Read only, on 2026-11-08. The node held the three Kubernetes packages, had its
automatic package units masked, carried no pending reboot, and its package
metadata was about six days old.

| File | Command |
| --- | --- |
| `apt_get_s_upgrade.txt` | `apt-get -s -o Debug::NoLocking=true upgrade` |
| `apt_get_s_upgrade_ignore_hold_inst_lines.txt` | the same call with `--ignore-hold`, kept to the `Inst` lines and the two summary lines. It is here to show what an upgrade list looks like when it DOES carry a held package, which the plain call never prints. |
| `apt_cache_policy_indexes.txt` | `apt-cache policy` |
| `apt_cache_policy_packages.txt` | `apt-cache policy <the 87 waiting package names>` |
| `apt_mark_showhold_kube_trio.txt` | `apt-mark showhold` |
| `dpkg_query_held_versions_kube_trio.txt` | `dpkg-query -W -f='${Package} ${Version} ${db:Status-Status}\n' kubeadm kubectl kubelet` |
| `dpkg_query_kernels_generic.txt` | `dpkg-query -W -f='${Package} ${Version} ${db:Status-Status}\n' 'linux-image-*'` |
| `apt_config_periodic_ubuntu_default.txt` | `apt-config dump APT::Periodic` |
| `apt_history.log` | `/var/log/apt/history.log` |
| `systemctl_show_masked_units.txt` | `systemctl show --property=Id,LoadState,UnitFileState,ActiveState` for each of the five automatic package units |

## Captured on a second running RunOS node, Ubuntu 24.04.5, a control plane

Read only, on 2026-11-08. That node held six packages, not three, and its kernel
meta package is `linux-image-virtual` rather than `linux-image-generic`.

| File | Command |
| --- | --- |
| `apt_mark_showhold_six_packages.txt` | `apt-mark showhold` |
| `dpkg_query_held_versions_six_packages.txt` | `dpkg-query -W -f=...` for those six names |
| `dpkg_query_kernels_virtual.txt` | `dpkg-query -W -f=... 'linux-image-*'` |
| `systemctl_show_not_masked_units.txt` | `systemctl show ...` for three units on that node that are NOT masked. It is the captured value vocabulary of a loaded and enabled unit, which no RunOS node's automatic units show, because RunOS masks them. |

## Two changes to the captured bytes, both to keep an identifier out of a public repo

1. `apt_history.log`: the `Requested-By:` line named the operator's account. The
   name is replaced with `operator`. The line is kept, because the parser has to
   tolerate it.
2. The regional Ubuntu mirror host in the apt output is replaced with
   `archive.ubuntu.com`, the same way in every file. The describe text is an
   opaque key that the index map and the version table are joined on, so the
   rewrite changes no behaviour under test, as long as it stays consistent.

Nothing else was edited.

## Not captured

| File | What it is |
| --- | --- |
| `contractd_apt_mark_showhold.txt` | the hold list of the objective's Contract D, written in `apt-mark showhold`'s own format. No node holds that whole set yet, so it could not be captured. It is what a node will report once the install holds come back, and it is the fixture that proves a held security update drops out of the count. |
| `degraded_node_record.json` | not an input. It is the record the collector produces for a degraded node, kept as the shared fixture that the store, the carrier and the read endpoint of this objective all use. Regenerate it with `UPDATE_GOLDEN=1 go test ./uc/patchfacts/`. |
| `lock_held_slow_pass_record.json` | not an input. It is the shared record for a slow pass that skips refresh because a package lock is held. The readable metadata age and count remain values, without a field error. |
