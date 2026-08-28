package main

import (
	"fmt"
	"os"

	"github.com/emersonbusson/civm/internal/civm"
)

const (
	civmGenerationCleanBoundaryCapability = civm.GenerationCleanBoundaryCapability
	civmGenerationCleanBoundaryMarker     = civm.GenerationCleanBoundaryMarker
)

// runCapability exposes an intentionally tiny compatibility surface for
// root-owned deploy wrappers. It never inspects runner state, credentials or
// user input beyond the fixed capability name.
func runCapability(args []string) int {
	if len(args) != 1 || args[0] != civmGenerationCleanBoundaryCapability {
		fmt.Fprintln(os.Stderr, "uso: civmctl capability generation-clean-boundary")
		return exitUsage
	}
	fmt.Println(civmGenerationCleanBoundaryMarker)
	return 0
}
