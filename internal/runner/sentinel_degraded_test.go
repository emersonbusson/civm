package runner

import "testing"

// TestDegradedDecisionStillBrokenSentinel guards the hook.DecisionCleanupDegraded
// contract: job-completed cleanup failures now exit 0 (a green job must not be
// failed by post-job hygiene), so the broken-runner sentinel MUST keep matching
// on the work_root action error alone — independent of decision or exit code.
func TestDegradedDecisionStillBrokenSentinel(t *testing.T) {
	t.Parallel()
	rec := hookLogRecord{Decision: "cleanup-degraded"}
	rec.Actions = []struct {
		Name  string `json:"name"`
		Error string `json:"error"`
	}{{Name: "work_root", Error: "unlinkat leftover: directory not empty"}}

	if !hookRecordIsBrokenSentinel(rec) {
		t.Fatal("cleanup-degraded with a work_root error must stay a broken-runner sentinel")
	}

	rec.Actions[0].Error = ""
	if hookRecordIsBrokenSentinel(rec) {
		t.Fatal("clean work_root action must not be a sentinel")
	}
}
