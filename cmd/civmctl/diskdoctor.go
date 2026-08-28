package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/emersonbusson/civm/internal/diskdoctor"
)

func runDiskDoctor(args []string) int {
	fs := flag.NewFlagSet("disk-doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "saida JSON do diagnostico de discard/TRIM")
	rootPath := fs.String("root-path", "/", "filesystem a diagnosticar")
	referenceTest := fs.Bool("reference-test", false, "delta opt-in: aloca 100MB, libera, fstrim, mede")
	timeoutSec := fs.Int("timeout", 30, "timeout em segundos")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de disk-doctor:", err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()
	opts := diskdoctor.DefaultOptions()
	opts.RootPath = *rootPath
	opts.ReferenceTest = *referenceTest
	report, err := diskdoctor.Diagnose(ctx, opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "disk-doctor:", err)
		// disk-doctor is a diagnostic command: always exit 0 even on probe
		// errors so callers never treat a diagnosis as an operational failure.
		return 0
	}
	if *jsonOut {
		if err := diskdoctor.RenderJSON(os.Stdout, report); err != nil {
			fmt.Fprintln(os.Stderr, "erro ao gerar JSON:", err)
			return 0
		}
	} else {
		diskdoctor.RenderText(os.Stdout, report)
	}
	return 0
}
