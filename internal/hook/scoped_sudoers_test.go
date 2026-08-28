package hook

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emersonbusson/civm/internal/civm"
)

// recordedWrite captures one WriteFileFn call for assertions.
type recordedWrite struct {
	data []byte
	perm os.FileMode
}

// recordedRun captures one RunFn invocation.
type recordedRun struct {
	name string
	args []string
}

// sudoersHarness wires injectable seams and records every side effect so tests
// never touch real /etc, sudo, or visudo.
type sudoersHarness struct {
	writes    map[string]recordedWrite
	runs      []recordedRun
	renames   [][2]string
	removes   []string
	mkdirs    []string
	visudoErr error // when non-nil, the visudo run returns this error
}

const (
	testWrapperBody = "#!/bin/sh\nexit 0\n"
	testSudoersBody = "emdev ALL=(root) NOPASSWD: /usr/local/bin/civm-safedelete, /usr/local/bin/civm-generation-boundary\n"
)

// newSudoersOpts builds InstallOptions whose deploy sources serve the wrapper +
// sudoers fixtures and whose side-effect fns record into h.
func newSudoersOpts(h *sudoersHarness) InstallOptions {
	h.writes = map[string]recordedWrite{}
	wrapperSrc := filepath.Join(civm.DefaultDeploySourceDir, civm.DefaultSafeDeleteWrapperSource)
	boundarySrc := filepath.Join(civm.DefaultDeploySourceDir, civm.DefaultGenerationBoundaryWrapperSource)
	sudoersSrc := filepath.Join(civm.DefaultDeploySourceDir, civm.DefaultScopedSudoersSource)
	return InstallOptions{
		DeploySourceDir: civm.DefaultDeploySourceDir,
		ReadFileFn: func(path string) ([]byte, error) {
			switch path {
			case wrapperSrc:
				return []byte(testWrapperBody), nil
			case boundarySrc:
				return []byte(testWrapperBody), nil
			case sudoersSrc:
				return []byte(testSudoersBody), nil
			}
			return nil, os.ErrNotExist
		},
		WriteFileFn: func(path string, data []byte, perm os.FileMode) error {
			cp := make([]byte, len(data))
			copy(cp, data)
			h.writes[path] = recordedWrite{data: cp, perm: perm}
			return nil
		},
		MkdirAllFn: func(path string, _ os.FileMode) error { h.mkdirs = append(h.mkdirs, path); return nil },
		RemoveFn:   func(path string) error { h.removes = append(h.removes, path); return nil },
		RenameFn:   func(o, n string) error { h.renames = append(h.renames, [2]string{o, n}); return nil },
		RunFn: func(_ context.Context, name string, args ...string) ([]byte, error) {
			h.runs = append(h.runs, recordedRun{name: name, args: append([]string(nil), args...)})
			if name == "visudo" {
				return []byte("validation output"), h.visudoErr
			}
			return nil, nil
		},
	}
}

const (
	wantSudoersTmp = civm.DefaultScopedSudoersDropIn + sudoersTempSuffix
)

func TestInstallScopedSudoersWritesBothArtifacts(t *testing.T) {
	var h sudoersHarness
	opts := newSudoersOpts(&h)

	if err := installScopedSudoers(context.Background(), opts); err != nil {
		t.Fatalf("installScopedSudoers: %v", err)
	}

	// Wrapper written to the canonical path, 0755, with the deploy content.
	wrapper, ok := h.writes[civm.DefaultSafeDeleteWrapperPath]
	if !ok {
		t.Fatalf("wrapper not written to %s (writes=%v)", civm.DefaultSafeDeleteWrapperPath, h.writes)
	}
	if wrapper.perm != safeDeleteWrapperPerm {
		t.Fatalf("wrapper perm = %v, want %v", wrapper.perm, safeDeleteWrapperPerm)
	}
	if string(wrapper.data) != testWrapperBody {
		t.Fatalf("wrapper content = %q, want %q", wrapper.data, testWrapperBody)
	}
	boundary, ok := h.writes[civm.DefaultGenerationBoundaryWrapperPath]
	if !ok {
		t.Fatalf("generation-boundary wrapper not written to %s (writes=%v)",
			civm.DefaultGenerationBoundaryWrapperPath, h.writes)
	}
	if boundary.perm != safeDeleteWrapperPerm {
		t.Fatalf("generation-boundary wrapper perm = %v, want %v", boundary.perm, safeDeleteWrapperPerm)
	}

	// Sudoers staged to the temp path at 0440, validated, then renamed into place.
	sudoers, ok := h.writes[wantSudoersTmp]
	if !ok {
		t.Fatalf("sudoers temp not written to %s (writes=%v)", wantSudoersTmp, h.writes)
	}
	if sudoers.perm != scopedSudoersPerm {
		t.Fatalf("sudoers perm = %v, want %v", sudoers.perm, scopedSudoersPerm)
	}
	if string(sudoers.data) != testSudoersBody {
		t.Fatalf("sudoers content = %q, want %q", sudoers.data, testSudoersBody)
	}
	if _, written := h.writes[civm.DefaultScopedSudoersDropIn]; written {
		t.Fatalf("sudoers must NOT be written directly to the final path; only renamed")
	}

	// visudo ran against the temp before activation.
	if !ranVisudoOn(h.runs, wantSudoersTmp) {
		t.Fatalf("visudo -c -f %s not invoked (runs=%v)", wantSudoersTmp, h.runs)
	}
	// Exactly one rename: temp -> final.
	if len(h.renames) != 1 || h.renames[0] != [2]string{wantSudoersTmp, civm.DefaultScopedSudoersDropIn} {
		t.Fatalf("rename = %v, want one temp->final", h.renames)
	}
	// Clean run leaves no temp removal.
	if len(h.removes) != 0 {
		t.Fatalf("unexpected removes on success: %v", h.removes)
	}
}

func TestGenerationBoundaryDeployArtifactsAreFixedAndWhitelisted(t *testing.T) {
	repoDeploy := filepath.Join("..", "..", "deploy")
	wrapperPath := filepath.Join(repoDeploy, civm.DefaultGenerationBoundaryWrapperSource)
	sudoersPath := filepath.Join(repoDeploy, civm.DefaultScopedSudoersSource)

	wrapper, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatalf("read generation-boundary wrapper %s: %v", wrapperPath, err)
	}
	sudoers, err := os.ReadFile(sudoersPath)
	if err != nil {
		t.Fatalf("read sudoers %s: %v", sudoersPath, err)
	}
	content := string(wrapper)
	if !strings.Contains(content, "CAPABILITY=generation-clean-boundary") ||
		!strings.Contains(content, "\"$CIVMCTL\" capability \"$CAPABILITY\"") ||
		!strings.Contains(content, "maintenance enter --execute --strict --timeout=120") ||
		!strings.Contains(content, "maintenance exit --execute --timeout=120") ||
		!strings.Contains(content, "warn-clean)") ||
		!strings.Contains(content, `"$SYSTEMCTL" poweroff --no-block`) {
		t.Fatalf("generation-boundary wrapper is missing its fixed contract:\n%s", content)
	}
	if strings.Contains(content, "$@") || strings.Contains(content, "eval ") {
		t.Fatalf("generation-boundary wrapper must not forward arbitrary input:\n%s", content)
	}
	if !strings.Contains(string(sudoers), civm.DefaultGenerationBoundaryWrapperPath) {
		t.Fatalf("sudoers does not whitelist %s:\n%s", civm.DefaultGenerationBoundaryWrapperPath, sudoers)
	}
}

func TestInstallScopedSudoersFailsClosedOnVisudoReject(t *testing.T) {
	var h sudoersHarness
	h.visudoErr = errors.New("exit status 1")
	opts := newSudoersOpts(&h)

	err := installScopedSudoers(context.Background(), opts)
	if err == nil {
		t.Fatalf("expected error when visudo rejects the sudoers")
	}

	// Fail closed: the drop-in is NEVER activated.
	if len(h.renames) != 0 {
		t.Fatalf("sudoers activated despite visudo reject: renames=%v", h.renames)
	}
	if _, written := h.writes[civm.DefaultScopedSudoersDropIn]; written {
		t.Fatalf("final sudoers path must not be written on reject")
	}
	// The rejected temp is cleaned up.
	if !contains(h.removes, wantSudoersTmp) {
		t.Fatalf("rejected temp %s not removed (removes=%v)", wantSudoersTmp, h.removes)
	}
}

func TestInstallScopedSudoersFailsClosedOnRenameError(t *testing.T) {
	var h sudoersHarness
	opts := newSudoersOpts(&h)
	opts.RenameFn = func(string, string) error { return errors.New("EXDEV") }

	err := installScopedSudoers(context.Background(), opts)
	if err == nil {
		t.Fatalf("expected error when rename fails")
	}
	if !contains(h.removes, wantSudoersTmp) {
		t.Fatalf("temp not cleaned after rename failure (removes=%v)", h.removes)
	}
}

func TestInstallScopedSudoersIdempotentReRun(t *testing.T) {
	var first, second sudoersHarness
	if err := installScopedSudoers(context.Background(), newSudoersOpts(&first)); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := installScopedSudoers(context.Background(), newSudoersOpts(&second)); err != nil {
		t.Fatalf("second run: %v", err)
	}

	// Same wrapper bytes/perm and same sudoers bytes/perm on both runs.
	if first.writes[civm.DefaultSafeDeleteWrapperPath].perm != second.writes[civm.DefaultSafeDeleteWrapperPath].perm {
		t.Fatalf("wrapper perm drifted between runs")
	}
	if string(first.writes[civm.DefaultSafeDeleteWrapperPath].data) != string(second.writes[civm.DefaultSafeDeleteWrapperPath].data) {
		t.Fatalf("wrapper content drifted between runs")
	}
	if string(first.writes[wantSudoersTmp].data) != string(second.writes[wantSudoersTmp].data) {
		t.Fatalf("sudoers content drifted between runs")
	}
	if len(first.renames) != 1 || len(second.renames) != 1 {
		t.Fatalf("expected exactly one activation per run, got %d and %d", len(first.renames), len(second.renames))
	}
}

func TestInstallScopedSudoersMatchesCanonicalConstants(t *testing.T) {
	var h sudoersHarness
	if err := installScopedSudoers(context.Background(), newSudoersOpts(&h)); err != nil {
		t.Fatalf("install: %v", err)
	}
	// The wrapper destination is exactly the path the sudoers whitelists and the
	// path the Go safedelete escalation invokes.
	if civm.DefaultSafeDeleteWrapperPath != "/usr/local/bin/civm-safedelete" {
		t.Fatalf("wrapper path constant = %q", civm.DefaultSafeDeleteWrapperPath)
	}
	if civm.DefaultScopedSudoersDropIn != "/etc/sudoers.d/civm-cleanup" {
		t.Fatalf("sudoers drop-in constant = %q", civm.DefaultScopedSudoersDropIn)
	}
	if _, ok := h.writes[civm.DefaultSafeDeleteWrapperPath]; !ok {
		t.Fatalf("wrapper not written to the canonical constant path")
	}
}

func TestInstallScopedSudoersPropagatesSourceErrors(t *testing.T) {
	wrapperSrc := filepath.Join(civm.DefaultDeploySourceDir, civm.DefaultSafeDeleteWrapperSource)
	sudoersSrc := filepath.Join(civm.DefaultDeploySourceDir, civm.DefaultScopedSudoersSource)

	cases := []struct {
		name    string
		read    func(string) ([]byte, error)
		wantSub string
	}{
		{
			name:    "wrapper source missing",
			read:    func(string) ([]byte, error) { return nil, os.ErrNotExist },
			wantSub: "read safedelete wrapper source",
		},
		{
			name: "wrapper source empty",
			read: func(p string) ([]byte, error) {
				if p == wrapperSrc {
					return []byte{}, nil
				}
				return []byte(testSudoersBody), nil
			},
			wantSub: "safedelete wrapper source",
		},
		{
			name: "sudoers source missing",
			read: func(p string) ([]byte, error) {
				if p == wrapperSrc {
					return []byte(testWrapperBody), nil
				}
				return nil, os.ErrNotExist
			},
			wantSub: "read scoped sudoers source",
		},
		{
			name: "sudoers source empty",
			read: func(p string) ([]byte, error) {
				if p == wrapperSrc {
					return []byte(testWrapperBody), nil
				}
				if p == sudoersSrc {
					return []byte{}, nil
				}
				return nil, os.ErrNotExist
			},
			wantSub: "scoped sudoers source",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var h sudoersHarness
			opts := newSudoersOpts(&h)
			boundarySrc := filepath.Join(civm.DefaultDeploySourceDir, civm.DefaultGenerationBoundaryWrapperSource)
			opts.ReadFileFn = func(path string) ([]byte, error) {
				if path == boundarySrc {
					return []byte(testWrapperBody), nil
				}
				return tc.read(path)
			}

			err := installScopedSudoers(context.Background(), opts)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if got := err.Error(); !strings.Contains(got, tc.wantSub) {
				t.Fatalf("error = %q, want substring %q", got, tc.wantSub)
			}
			// No source error path may activate the sudoers.
			if len(h.renames) != 0 {
				t.Fatalf("sudoers activated on source error: %v", h.renames)
			}
		})
	}
}

func TestInstallScopedSudoersReadsFromDeploySourceDir(t *testing.T) {
	var h sudoersHarness
	opts := newSudoersOpts(&h)
	opts.DeploySourceDir = "/custom/deploy"

	var readPaths []string
	wrapperSrc := filepath.Join("/custom/deploy", civm.DefaultSafeDeleteWrapperSource)
	boundarySrc := filepath.Join("/custom/deploy", civm.DefaultGenerationBoundaryWrapperSource)
	sudoersSrc := filepath.Join("/custom/deploy", civm.DefaultScopedSudoersSource)
	opts.ReadFileFn = func(path string) ([]byte, error) {
		readPaths = append(readPaths, path)
		switch path {
		case wrapperSrc:
			return []byte(testWrapperBody), nil
		case boundarySrc:
			return []byte(testWrapperBody), nil
		case sudoersSrc:
			return []byte(testSudoersBody), nil
		}
		return nil, os.ErrNotExist
	}

	if err := installScopedSudoers(context.Background(), opts); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !contains(readPaths, wrapperSrc) || !contains(readPaths, boundarySrc) || !contains(readPaths, sudoersSrc) {
		t.Fatalf("did not read from custom deploy dir: %v", readPaths)
	}
}

func TestInstallScopedSudoersPropagatesMkdirErrors(t *testing.T) {
	cases := []struct {
		name    string
		failDir string
		wantSub string
	}{
		{"wrapper mkdir fails", filepath.Dir(civm.DefaultSafeDeleteWrapperPath), "mkdir for safedelete wrapper"},
		{"sudoers mkdir fails", filepath.Dir(civm.DefaultScopedSudoersDropIn), "mkdir for sudoers drop-in"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var h sudoersHarness
			opts := newSudoersOpts(&h)
			opts.MkdirAllFn = func(path string, _ os.FileMode) error {
				if path == tc.failDir {
					return errors.New("EACCES")
				}
				return nil
			}
			err := installScopedSudoers(context.Background(), opts)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantSub)
			}
			if len(h.renames) != 0 {
				t.Fatalf("sudoers activated despite mkdir failure: %v", h.renames)
			}
		})
	}
}

func TestInstallScopedSudoersTruncatesLongVisudoOutput(t *testing.T) {
	var h sudoersHarness
	h.visudoErr = errors.New("exit status 1")
	opts := newSudoersOpts(&h)
	long := strings.Repeat("x", 500)
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		h.runs = append(h.runs, recordedRun{name: name, args: append([]string(nil), args...)})
		if name == "visudo" {
			return []byte(long), h.visudoErr
		}
		return nil, nil
	}
	err := installScopedSudoers(context.Background(), opts)
	if err == nil {
		t.Fatalf("expected visudo reject error")
	}
	// The wrapped diagnostic is capped (200) — the full 500-char blob is not in it.
	if strings.Contains(err.Error(), long) {
		t.Fatalf("visudo output not truncated in error: %q", err.Error())
	}
}

// TestInstallScopedSudoersWithRepoDeployArtifacts feeds the REAL versioned
// deploy/ artifacts (the ones shipped to /opt/civm/deploy at provisioning)
// through installScopedSudoers to prove they are non-empty, that the sudoers
// whitelists exactly the wrapper destination, and that the install activates
// them without touching real /etc or sudo.
func TestInstallScopedSudoersWithRepoDeployArtifacts(t *testing.T) {
	// The package lives at internal/hook; the repo deploy/ is two levels up.
	repoDeploy := filepath.Join("..", "..", "deploy")
	wrapperSrc := filepath.Join(repoDeploy, civm.DefaultSafeDeleteWrapperSource)
	boundarySrc := filepath.Join(repoDeploy, civm.DefaultGenerationBoundaryWrapperSource)
	sudoersSrc := filepath.Join(repoDeploy, civm.DefaultScopedSudoersSource)

	wrapperBytes, err := os.ReadFile(wrapperSrc)
	if err != nil {
		t.Fatalf("read repo wrapper %s: %v", wrapperSrc, err)
	}
	boundaryBytes, err := os.ReadFile(boundarySrc)
	if err != nil {
		t.Fatalf("read repo generation-boundary wrapper %s: %v", boundarySrc, err)
	}
	sudoersBytes, err := os.ReadFile(sudoersSrc)
	if err != nil {
		t.Fatalf("read repo sudoers %s: %v", sudoersSrc, err)
	}
	// The sudoers must whitelist exactly the wrapper destination constant.
	for _, path := range []string{civm.DefaultSafeDeleteWrapperPath, civm.DefaultGenerationBoundaryWrapperPath} {
		if !strings.Contains(string(sudoersBytes), path) {
			t.Fatalf("repo sudoers does not whitelist %s:\n%s", path, sudoersBytes)
		}
	}
	// And it must NOT whitelist raw chown/rm or a path wildcard (DT-v2-1/3).
	for _, forbidden := range []string{"/usr/bin/chown", "/usr/bin/rm", "/bin/chown", "/bin/rm", "*"} {
		if strings.Contains(string(sudoersBytes), forbidden) {
			t.Fatalf("repo sudoers must not contain %q (DT-v2-3 scope):\n%s", forbidden, sudoersBytes)
		}
	}

	var h sudoersHarness
	opts := newSudoersOpts(&h)
	opts.DeploySourceDir = repoDeploy
	opts.ReadFileFn = func(path string) ([]byte, error) {
		switch path {
		case wrapperSrc:
			return wrapperBytes, nil
		case boundarySrc:
			return boundaryBytes, nil
		case sudoersSrc:
			return sudoersBytes, nil
		}
		return nil, os.ErrNotExist
	}

	if err := installScopedSudoers(context.Background(), opts); err != nil {
		t.Fatalf("installScopedSudoers with repo artifacts: %v", err)
	}
	if string(h.writes[civm.DefaultSafeDeleteWrapperPath].data) != string(wrapperBytes) {
		t.Fatalf("installed wrapper differs from repo artifact")
	}
	if string(h.writes[civm.DefaultGenerationBoundaryWrapperPath].data) != string(boundaryBytes) {
		t.Fatalf("installed generation-boundary wrapper differs from repo artifact")
	}
	if len(h.renames) != 1 {
		t.Fatalf("expected one activation, got %v", h.renames)
	}
}

func ranVisudoOn(runs []recordedRun, tmp string) bool {
	for _, r := range runs {
		if r.name != "visudo" {
			continue
		}
		// Expect "-c", "-f", <tmp>.
		if len(r.args) >= 3 && r.args[0] == "-c" && r.args[1] == "-f" && r.args[2] == tmp {
			return true
		}
	}
	return false
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
