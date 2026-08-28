package jitdispatcher

import (
	"context"
	"io"
	"os/exec"
	"time"
)

type ProcessContainment interface {
	Prepare(*exec.Cmd, string) (io.Closer, error)
	Attach(int, string) (ProcessIdentity, error)
	Terminate(context.Context, ProcessIdentity, time.Duration) error
	Recover(context.Context, string, ProcessIdentity, time.Duration) error
	Alive(ProcessIdentity) (bool, error)
}
