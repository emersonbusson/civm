package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/emersonbusson/civm/internal/jitdispatcher"
)

func TestJITDispatchUsageRequiresAllFlagsWithoutReadingToken(t *testing.T) {
	stdin := &trackingReader{}
	var stdout, stderr bytes.Buffer
	if code := runJITDispatchWithIO(nil, stdin, &stdout, &stderr); code != exitUsage {
		t.Fatalf("exit = %d", code)
	}
	if stdin.read {
		t.Fatal("usage failure read token stdin")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestJITDispatchExitMapping(t *testing.T) {
	tests := []struct {
		err  error
		want int
	}{
		{jitdispatcher.ErrInvalid, exitUsage},
		{jitdispatcher.ErrBusy, exitJITBusy},
		{jitdispatcher.ErrAmbiguous, exitJITAmbiguous},
		{jitdispatcher.ErrStale, exitJITAmbiguous},
		{jitdispatcher.ErrReplay, exitJITAmbiguous},
		{errors.New("failure"), exitJITFailure},
	}
	for _, test := range tests {
		if got := jitDispatchExit(test.err); got != test.want {
			t.Errorf("jitDispatchExit(%v) = %d, want %d", test.err, got, test.want)
		}
	}
}

type trackingReader struct {
	read bool
}

func (reader *trackingReader) Read([]byte) (int, error) {
	reader.read = true
	return 0, errors.New("must not read")
}
