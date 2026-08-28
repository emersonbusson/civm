package civm

// EmergencyAdmits is the SPECv3 admission gate for the below-headroom emergency
// reclaim (docs/specs/host-volume-reclamation/SPECv3.md, DT-v3-1).
//
// Optimize-VHD -Mode Full is uninterruptible: once the native CompactVirtualDisk
// operation is in flight it cannot be safely aborted (Stop-Job abandons the
// PowerShell wrapper while the compaction keeps writing to V:). The only safe
// lever is therefore to refuse to START the operation unless the available slack
// provably covers the worst-case scratch the compaction may consume.
//
// It returns true only when the live free space, minus the absolute hard floor,
// is at least the measured scratch budget. A non-positive budget means the
// measurement campaign (DT-v3-2) has not run yet, so the emergency path stays
// disabled and the normal headroom keeps applying — no regression, and the
// deadlock is not relocated to a guessed lower floor.
//
// All arguments are in GB.
func EmergencyAdmits(liveFreeGB, hardFloorGB, scratchBudgetGB int64) bool {
	if scratchBudgetGB <= 0 {
		return false
	}
	return liveFreeGB-hardFloorGB >= scratchBudgetGB
}
