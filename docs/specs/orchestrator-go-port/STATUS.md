# STATUS — orchestrator-go-port

| Field | Value |
| --- | --- |
| Status | **Superseded / deferred** |
| Date | 2026-07-14 |
| Superseded by | Sibling project **civm-host** (C# on Windows host) |
| Behavior contract remains | [`../orchestrator-scale-to-zero/SPEC.md`](../orchestrator-scale-to-zero/SPEC.md) |

## Decision (discipline)

Do **not** implement a second host-side brain in Go (`civmctl orchestrate tick` on Windows).

- **Guest** stays Go (`civmctl`) forever — cleanup, runners, reaper, doctor.
- **Host** actuation is Windows/Hyper-V (`Start/Stop-VM`, `Optimize-VHD`, WMI). The day-0 path for replacing PowerShell is **civm-host** (F1–F2 done, F3 wiring, F4 cutover), not a Go host agent.
- Keeping this PRD/SPEC as **historical design notes** avoids re-litigating Model A/B; new work must not open a dual-owner race with `civm-vm-orchestrator.ps1` or `civm-host.exe`.

## Kahneman notes

- **#15 (retry only with signature):** dual orchestrators on the same VHDX without a single owner is a hang class — forbidden until F4 single-owner.
- **#18 (fix in owning layer):** disk-safety decision/actuation ownership is the host process that holds `V:\civm-reclaim.lock`.
- **WYSIATI:** PS remains production until F4 Tier III evidence; shadow C# ticks are observation only.

## What to do instead

1. Operate the lab with host-local lab wrapper (`Repos`/`TokenPaths`/`GuestSshTarget`) — never commit lab fleet into public defaults.
2. Advance **civm-host** F3 (state write + compact parity) → F4 (`--active` + disable PS tasks) with Tier II/III gates.
3. Reopen a Go host port only if F4 is rejected with written evidence that C# cannot meet the contract.
