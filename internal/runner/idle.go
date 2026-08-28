package runner

import (
	"context"
	"time"

	"github.com/emersonbusson/civm/internal/idle"
)

func ensureMutationIdle(ctx context.Context, activityFn func(context.Context) ([]idle.Activity, error), probeDelay time.Duration) error {
	opts := idle.DefaultOptions()
	opts.ActivityFn = activityFn
	opts.ProbeDelay = probeDelay
	return idle.Ensure(ctx, opts, "runner mutation")
}
