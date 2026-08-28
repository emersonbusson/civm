package capacity

import (
	"context"
	"testing"
)

// BenchmarkCheck measures the metrics dump hot path invoked by
// civmctl-metrics.timer every minute. Mocks neutralize real syscalls and
// isolate orchestration plus gh/systemctl/pgrep output parsing.
func BenchmarkCheck(b *testing.B) {
	opts := DefaultOptions()
	opts.StatfsFn = func(string) (uint64, uint64, error) {
		return 100 << 30, 60 << 30, nil
	}
	opts.RunFn = func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name == "systemctl" {
			return []byte("actions.runner.foo.service loaded active running\nactions.runner.bar.service loaded active running\n"), nil
		}
		return []byte("2\n"), nil
	}
	ctx := context.Background()
	b.ResetTimer()
	for range b.N {
		_ = Check(ctx, opts)
	}
}
