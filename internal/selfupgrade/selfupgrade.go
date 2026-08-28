// Package selfupgrade rebuilds civmctl from its source checkout and
// atomically swaps the installed binary. Used to roll out fixes (notably
// the hook policy under internal/hook) to the runner without manual
// scp / dpkg ceremony.
//
// Safety guarantees:
//   - Build is verified (version-pins, a real deterministic subcommand) before
//     the swap — not --help, which any binary that merely links satisfies.
//   - Swap is os.Rename within the same dir → atomic per POSIX.
//   - On any failure path, the target binary is untouched.
//   - Temp build artifact is removed on error.
package selfupgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Options controla a operação de upgrade. Funções injetáveis permitem
// testar sem invocar `go build` ou tocar /usr/local/bin.
type Options struct {
	SourceDir string        // diretório com o checkout do source (default /opt/civm)
	Target    string        // caminho final do binário (default /usr/local/bin/civmctl)
	Execute   bool          // false = dry-run; true = aplica a substituição
	Timeout   time.Duration // timeout do build
	BuildFn   func(ctx context.Context, sourceDir, output string) error
	VerifyFn  func(path string) error
	RenameFn  func(oldPath, newPath string) error
	RemoveFn  func(path string) error
}

// Result captura o que aconteceu para render/auditoria.
type Result struct {
	Executed  bool   `json:"executed"`
	SourceDir string `json:"source_dir"`
	Target    string `json:"target"`
	BuiltAt   string `json:"built_at,omitempty"`
	Verified  bool   `json:"verified"`
	Swapped   bool   `json:"swapped"`
	OldSize   int64  `json:"old_size,omitempty"`
	NewSize   int64  `json:"new_size,omitempty"`
	Error     string `json:"error,omitempty"`
}

// DefaultOptions retorna padrões de produção (build via `go build`,
// verify via `civmctl-new version-pins`, rename via os.Rename).
func DefaultOptions() Options {
	return Options{
		SourceDir: "/opt/civm",
		Target:    "/usr/local/bin/civmctl",
		Timeout:   5 * time.Minute,
		BuildFn:   defaultBuild,
		VerifyFn:  defaultVerify,
		RenameFn:  os.Rename,
		RemoveFn:  os.Remove,
	}
}

// Run faz o upgrade. Em dry-run, apenas reporta o que faria. Em execute,
// builda no mesmo dir do target (necessário para rename atômico), verifica
// o binário gerado, e só então faz o swap.
func Run(ctx context.Context, opts Options) Result {
	applyDefaults(&opts)
	res := Result{Executed: opts.Execute, SourceDir: opts.SourceDir, Target: opts.Target}

	if !opts.Execute {
		// Dry-run: validar pré-condições sem tocar nada.
		if _, err := os.Stat(opts.SourceDir); err != nil {
			return failResult(res, fmt.Errorf("source_dir não existe: %w", err))
		}
		if info, err := os.Stat(opts.Target); err == nil {
			res.OldSize = info.Size()
		}
		return res
	}

	if info, err := os.Stat(opts.Target); err == nil {
		res.OldSize = info.Size()
	}

	// Build em arquivo dotted no mesmo dir do target — atomic rename exige
	// mesmo filesystem, e `go build` pode escrever para qualquer path. O
	// nome com ponto evita que aparece como binário válido caso o processo
	// crashe antes do rename.
	targetDir := filepath.Dir(opts.Target)
	tmpPath := filepath.Join(targetDir, ".civmctl.new")
	res.BuiltAt = tmpPath

	buildCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	if err := opts.BuildFn(buildCtx, opts.SourceDir, tmpPath); err != nil {
		_ = opts.RemoveFn(tmpPath)
		return failResult(res, fmt.Errorf("build: %w", err))
	}

	if err := opts.VerifyFn(tmpPath); err != nil {
		_ = opts.RemoveFn(tmpPath)
		return failResult(res, fmt.Errorf("verify built binary: %w", err))
	}
	res.Verified = true

	if info, err := os.Stat(tmpPath); err == nil {
		res.NewSize = info.Size()
	}

	if err := opts.RenameFn(tmpPath, opts.Target); err != nil {
		_ = opts.RemoveFn(tmpPath)
		return failResult(res, fmt.Errorf("rename: %w", err))
	}
	res.Swapped = true
	return res
}

func applyDefaults(opts *Options) {
	if opts.SourceDir == "" {
		opts.SourceDir = "/opt/civm"
	}
	if opts.Target == "" {
		opts.Target = "/usr/local/bin/civmctl"
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Minute
	}
	if opts.BuildFn == nil {
		opts.BuildFn = defaultBuild
	}
	if opts.VerifyFn == nil {
		opts.VerifyFn = defaultVerify
	}
	if opts.RenameFn == nil {
		opts.RenameFn = os.Rename
	}
	if opts.RemoveFn == nil {
		opts.RemoveFn = os.Remove
	}
}

func failResult(res Result, err error) Result {
	if err != nil {
		res.Error = err.Error()
	}
	return res
}

func defaultBuild(ctx context.Context, sourceDir, output string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-ldflags=-s -w", "-o", output, "./cmd/civmctl")
	cmd.Dir = sourceDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

// defaultVerify roda `<path> version-pins`, um subcomando determinístico que
// despacha lógica real (renderiza os version pins via specs.Ubuntu2404) sem
// depender de syscall de runner, root ou estado do box. É um smoke genuíno do
// binário recém-buildado: uma build que compila mas quebra no dispatch falha
// aqui — ao contrário de `--help`, que qualquer binário que apenas linka
// satisfaz (auditoria #13: existência != função). Exige exit 0 E saída não
// vazia.
func defaultVerify(path string) error {
	cmd := exec.Command(path, "version-pins") //nolint:gosec // G204: path é o binário recém-buildado, sob nosso controle
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("version-pins check: %w: %s", err, string(out))
	}
	if strings.TrimSpace(string(out)) == "" {
		return fmt.Errorf("version-pins check: empty output from %s", path)
	}
	return nil
}

// RenderJSON emite o resultado em JSON estável para integradores.
func RenderJSON(w io.Writer, r Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// RenderText emite formato humano para terminal.
func RenderText(w io.Writer, r Result) {
	mode := "DRY-RUN"
	if r.Executed {
		mode = "EXECUTE"
	}
	fmt.Fprintf(w, "civm self-upgrade: %s\n", mode)
	fmt.Fprintf(w, "  source: %s\n", r.SourceDir)
	fmt.Fprintf(w, "  target: %s (atual %d bytes)\n", r.Target, r.OldSize)
	if r.Executed {
		fmt.Fprintf(w, "  built:  %s (%d bytes)\n", r.BuiltAt, r.NewSize)
		fmt.Fprintf(w, "  verify: %v\n", r.Verified)
		fmt.Fprintf(w, "  swap:   %v\n", r.Swapped)
	}
	if r.Error != "" {
		fmt.Fprintf(w, "Error: %s\n", r.Error)
	}
}
