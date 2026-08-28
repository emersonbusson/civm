package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/emersonbusson/civm/internal/hostdisk"
)

func runHostDisk(args []string) int {
	fs := flag.NewFlagSet("host-disk", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "saida JSON para guards/integradores")
	path := fs.String("path", "", "caminho do JSON do host entregue ao guest")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "erro nos args de host-disk:", err)
		return exitUsage
	}
	opts := hostdisk.DefaultOptions()
	if *path != "" {
		opts.Path = *path
	}
	r, err := hostdisk.Check(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aviso ao ler o JSON do host:", err)
	}
	if *jsonOut {
		if jerr := hostdisk.RenderJSON(os.Stdout, r); jerr != nil {
			fmt.Fprintln(os.Stderr, "erro ao gerar JSON:", jerr)
			return 2
		}
	} else {
		hostdisk.RenderText(os.Stdout, r)
	}
	// Exit 1 em crit OU qualquer violacao de headroom OU stale/delivery-failed
	// (DT-v2-13): serve de guard para a task de compactacao no host.
	if r.Level == "crit" || r.FreeHeadroomViolation || r.AllocationHeadroomViolation {
		return 1
	}
	return 0
}
