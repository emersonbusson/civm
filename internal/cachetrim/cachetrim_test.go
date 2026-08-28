package cachetrim

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
)

// TestCapsGlobsNamedDirsAcrossHomes is the regression of the 2026-06 incident:
// the named per-workflow cache dirs (go-build-acme-services, yarn-acme-web)
// must be capped, and the family budget divided among matched dirs.
func TestCapsGlobsNamedDirsAcrossHomes(t *testing.T) {
	root := t.TempDir()
	mk := func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(p, 0o750); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// two runner homes (cleanup runs as root over /home/*), each with named caches.
	home1 := filepath.Join(root, "home", "emdev")
	home2 := filepath.Join(root, "home", "runner")
	gbA := mk("home", "emdev", ".cache", "go-build-acme-services")
	gbB := mk("home", "emdev", ".cache", "go-build-acme-devctl")
	gbC := mk("home", "runner", ".cache", "go-build-acme-web")
	yarn1 := mk("home", "emdev", ".cache", "yarn-acme-web")
	yarnBerry := mk("home", "emdev", ".yarn-berry-acme-org")
	lint := mk("home", "emdev", ".cache", "golangci-lint")
	npm := mk("home", "emdev", ".npm", "_cacache")

	caps := Caps([]string{home1, home2}, Deps{})
	byPath := make(map[string]Cap, len(caps))
	for _, c := range caps {
		byPath[c.Path] = c
	}
	for _, p := range []string{gbA, gbB, gbC, yarn1, yarnBerry, lint, npm} {
		if _, ok := byPath[p]; !ok {
			t.Errorf("Caps() missing named dir %s — would grow unbounded", p)
		}
	}
	// 3 go-build dirs across both homes share the family budget (familyGB/3).
	const giB = int64(1) << 30
	wantPer := int64(civm.DefaultCacheGoBuildMaxGB) * giB / 3
	if got := byPath[gbA].MaxBytes; got != wantPer {
		t.Errorf("go-build per-dir cap=%d, want family/3=%d", got, wantPer)
	}
	wantYarnPer := int64(civm.DefaultCacheYarnMaxGB) * giB / 2
	if got := byPath[yarnBerry].MaxBytes; got != wantYarnPer {
		t.Errorf("yarn berry per-dir cap=%d, want family/2=%d", got, wantYarnPer)
	}
	// Paths derives 1:1.
	if len(Paths(caps)) != len(caps) {
		t.Errorf("Paths len mismatch")
	}
}

// TestCapsEmptyHomeNoPanic: a non-existent / empty home yields no globbed caps
// (npm/pnpm still added per home but absent → harmless no-op downstream).
func TestCapsEmptyHomeNoPanic(t *testing.T) {
	caps := Caps([]string{filepath.Join(t.TempDir(), "nope")}, Deps{})
	for _, c := range caps {
		if c.MaxBytes <= 0 {
			t.Errorf("cap %s non-positive budget", c.Path)
		}
	}
}

// TestTrimByAgeRemovesOldestPreservesHot proves the core trim: over-cap dir gets
// its oldest files removed down to the cap, but files newer than MinProtect stay.
func TestTrimByAgeRemovesOldestPreservesHot(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	mkfile := func(name string, size int64, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := mkfile("old.o", 8<<20, 72*time.Hour) // 8MB, 3 days old → trimmable
	hot := mkfile("hot.o", 8<<20, 1*time.Hour)  // 8MB, 1h old → protected
	mid := mkfile("mid.o", 8<<20, 48*time.Hour) // 8MB, 2 days old → trimmable

	var removed []string
	opts := Options{
		Execute:     true,
		Now:         now,
		RemoveAllFn: func(p string) error { removed = append(removed, p); return nil },
	}
	// cap 12MB: total 24MB → must free ~12MB from oldest (old + mid), never hot.
	c := Cap{Path: dir, MaxBytes: 12 << 20, MinProtect: 24 * time.Hour}
	r := TrimByAge(opts, c)
	if r.Err != nil {
		t.Fatalf("unexpected err: %v", r.Err)
	}
	if r.BytesFound != 24<<20 {
		t.Errorf("BytesFound=%d, want 24MB", r.BytesFound)
	}
	hotRemoved := false
	for _, p := range removed {
		if p == hot {
			hotRemoved = true
		}
	}
	if hotRemoved {
		t.Errorf("trim removed the hot (<MinProtect) file %s", hot)
	}
	if len(removed) == 0 {
		t.Errorf("over-cap dir should have trimmed oldest; removed nothing (old=%s mid=%s)", old, mid)
	}
}

// TestTrimByAgeHardCeilingTrimsRecentWhenAllProtected prova o TETO HARD do
// incidente 2026-06-15: quando TODO arquivo esta dentro do MinProtect — o caso do
// cache de CI sob carga continua, onde o yarn-acme-* cresceu a 18GB porque o cap
// nunca aplicava — o MaxBytes AINDA e imposto (Pass 2 trima os protegidos, do mais
// antigo ao mais novo). Antes do fix isto FALHAVA (freed=0, dir crescia sem limite).
func TestTrimByAgeHardCeilingTrimsRecentWhenAllProtected(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	mkfile := func(name string, size int64, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// 3 arquivos de 8MB (24MB total), TODOS dentro do MinProtect de 24h.
	oldest := mkfile("a.o", 8<<20, 3*time.Hour)
	mkfile("b.o", 8<<20, 2*time.Hour)
	newest := mkfile("c.o", 8<<20, 1*time.Hour)

	var removed []string
	opts := Options{
		Execute:     true,
		Now:         now,
		RemoveAllFn: func(p string) error { removed = append(removed, p); return nil },
	}
	// cap 10MB, total 24MB → DEVE liberar >=14MB mesmo com tudo protegido.
	c := Cap{Path: dir, MaxBytes: 10 << 20, MinProtect: 24 * time.Hour}
	r := TrimByAge(opts, c)
	if r.Err != nil {
		t.Fatalf("unexpected err: %v", r.Err)
	}
	if r.BytesFreed < (24<<20)-(10<<20) {
		t.Errorf("hard ceiling NAO imposto: freed=%d, want >= %d (todos os arquivos recentes)", r.BytesFreed, (24<<20)-(10<<20))
	}
	removedOldest, removedNewest := false, false
	for _, p := range removed {
		if p == oldest {
			removedOldest = true
		}
		if p == newest {
			removedNewest = true
		}
	}
	if !removedOldest {
		t.Error("hard ceiling deve trimar o arquivo protegido MAIS ANTIGO primeiro")
	}
	if removedNewest {
		t.Errorf("hard ceiling over-trimou: removeu o mais novo %s quando os mais antigos bastavam", newest)
	}
}

// TestTrimByAgeDirAtomicRemovesWholePackages prova o fix do incidente 2026-06-15:
// num cache yarn-shape (pacote = diretório multi-arquivo), o trim DirAtomic remove
// o diretório de pacote INTEIRO do mais antigo, e nunca deixa um pacote parcial —
// que quebraria o `yarn install --frozen-lockfile` com ENOENT no arquivo sumido.
func TestTrimByAgeDirAtomicRemovesWholePackages(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	// mkpkg cria um pacote yarn-shape: v6/<pkg>/{node_modules/<pkg>/index.js,
	// .yarn-metadata.json} — múltiplos arquivos sob um dir de pacote.
	mkpkg := func(pkg string, sizeEach int64, age time.Duration) string {
		root := filepath.Join(dir, "v6", pkg)
		inner := filepath.Join(root, "node_modules", pkg)
		if err := os.MkdirAll(inner, 0o750); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{filepath.Join(inner, "index.js"), filepath.Join(root, ".yarn-metadata.json")} {
			if err := os.WriteFile(f, make([]byte, sizeEach), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(f, now.Add(-age), now.Add(-age)); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	oldPkg := mkpkg("npm-old-1.0.0", 5<<20, 72*time.Hour) // 10MB, 3 dias → trimável
	hotPkg := mkpkg("npm-hot-2.0.0", 5<<20, 1*time.Hour)  // 10MB, 1h → protegido

	var removed []string
	opts := Options{
		Execute:     true,
		Now:         now,
		RemoveAllFn: func(p string) error { removed = append(removed, p); return nil },
	}
	// cap 12MB, total 20MB → libera ~8MB removendo o pacote mais antigo INTEIRO.
	// PackageDepth 2 = pacote yarn v1 em <root>/v6/<pkg>.
	c := Cap{Path: dir, MaxBytes: 12 << 20, MinProtect: 24 * time.Hour, PackageDepth: 2}
	r := TrimByAge(opts, c)
	if r.Err != nil {
		t.Fatalf("unexpected err: %v", r.Err)
	}
	if r.BytesFound != 20<<20 {
		t.Errorf("BytesFound=%d, want 20MB", r.BytesFound)
	}
	// Uma única RemoveAll, no dir do pacote velho — atômico, nunca arquivo solto.
	if len(removed) != 1 || removed[0] != oldPkg {
		t.Errorf("esperava 1 RemoveAll do dir do pacote velho %s, removed=%v", oldPkg, removed)
	}
	if removed[0] == hotPkg {
		t.Errorf("removeu o pacote quente %s", hotPkg)
	}
}

// TestTrimByAgePackageDepthShallowFilesAreFileUnits prova que um cache de unidade
// de arquivo único (yarn berry `<pkg>.zip`, ou um blob content-addressed) sob
// PackageDepth > 0 cai sozinho no modo por arquivo: os arquivos rasos não casam a
// profundidade de pacote, então o trim remove o arquivo inteiro mais antigo sem
// tocar no quente — nada de remoção parcial. É a garantia para npm/pnpm/berry.
func TestTrimByAgePackageDepthShallowFilesAreFileUnits(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	mkfile := func(name string, size int64, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := mkfile("pkg-old-1.0.0.zip", 8<<20, 72*time.Hour) // raso → unidade de arquivo
	hot := mkfile("pkg-hot-2.0.0.zip", 8<<20, 1*time.Hour)

	var removed []string
	opts := Options{Execute: true, Now: now, RemoveAllFn: func(p string) error { removed = append(removed, p); return nil }}
	// PackageDepth 2, mas os arquivos estão em profundidade 1 → modo por arquivo.
	c := Cap{Path: dir, MaxBytes: 10 << 20, MinProtect: 24 * time.Hour, PackageDepth: 2}
	r := TrimByAge(opts, c)
	if r.Err != nil {
		t.Fatalf("unexpected err: %v", r.Err)
	}
	if len(removed) != 1 || removed[0] != old {
		t.Errorf("esperava remover o arquivo velho %s, removed=%v", old, removed)
	}
	if removed[0] == hot {
		t.Errorf("removeu o arquivo quente %s", hot)
	}
}

// TestTrimByAgeWipeWholeRemovesEntireDir prova o backstop go-build/golangci: um
// cache com refs cruzadas (WipeWhole) acima do cap é removido por INTEIRO num único
// RemoveAll do dir — nunca arquivo a arquivo, que orfanaria uma entrada (`-a` sem
// o `-d` em outro prefixo) e quebraria o `go vet` com "can't import facts".
func TestTrimByAgeWipeWholeRemovesEntireDir(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	// go-build-shape: `-a` (ação) e `-d` (dados) em diretórios de prefixo distintos.
	for _, f := range []string{"d4/aaa-a", "42/bbb-d", "66/ccc-d"} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, 5<<20), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var removed []string
	opts := Options{Execute: true, Now: now, RemoveAllFn: func(p string) error { removed = append(removed, p); return nil }}
	// total 15MB > cap 10MB → wipe do dir inteiro (atômico).
	c := Cap{Path: dir, MaxBytes: 10 << 20, MinProtect: 24 * time.Hour, WipeWhole: true}
	r := TrimByAge(opts, c)
	if r.Err != nil {
		t.Fatalf("unexpected err: %v", r.Err)
	}
	if r.BytesFreed != 15<<20 {
		t.Errorf("BytesFreed=%d, want 15MB (wipe inteiro)", r.BytesFreed)
	}
	if len(removed) != 1 || removed[0] != dir {
		t.Errorf("esperava 1 RemoveAll do dir inteiro %s, removed=%v", dir, removed)
	}
}

func TestTrimByAgeNoOpUnderCap(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	r := TrimByAge(Options{Execute: true, Now: time.Now()}, Cap{Path: dir, MaxBytes: 5 << 20, MinProtect: time.Hour})
	if r.BytesFreed != 0 {
		t.Errorf("under-cap dir must not trim; freed=%d", r.BytesFreed)
	}
}

func TestTrimByAgeUnsafePath(t *testing.T) {
	for _, p := range []string{"", "/"} {
		r := TrimByAge(Options{Execute: true, Now: time.Now()}, Cap{Path: p, MaxBytes: 1, MinProtect: time.Hour})
		if r.Err == nil {
			t.Errorf("path %q must be rejected as unsafe", p)
		}
	}
}

func TestTrimByAgeMissingDirIsNoError(t *testing.T) {
	r := TrimByAge(Options{Execute: true, Now: time.Now()}, Cap{Path: filepath.Join(t.TempDir(), "absent"), MaxBytes: 1, MinProtect: time.Hour})
	if r.Err != nil {
		t.Errorf("absent cache dir must be a no-op, got err %v", r.Err)
	}
}

// TestTrimByAgeInFlightFloorSkipsFreshDirReclaimsStale prova o floor in-flight do
// emergency path. O emergency_test antigo injetava cache VAZIO (GlobFn->nil) e
// nunca exercitava este caminho — o buraco Kahneman #13 que escondia o job-kill.
// Com InFlightFloor setado: um dir com escrita fresca (install vivo) é PULADO
// inteiro mesmo acima do cap (par com o positivo: um dir só com arquivos velhos,
// runaway stale, continua sendo reclamado).
func TestTrimByAgeInFlightFloorSkipsFreshDirReclaimsStale(t *testing.T) {
	now := time.Now()
	// Cria um dir com 3 arquivos de 8MB (24MB total) na idade dada.
	mkdir := func(age time.Duration) string {
		dir := t.TempDir()
		for _, n := range []string{"a.o", "b.o", "c.o"} {
			p := filepath.Join(dir, n)
			if err := os.WriteFile(p, make([]byte, 8<<20), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	const floor = 15 * time.Minute
	// cap 10MB num total de 24MB → sem o floor, trimaria (>=14MB) mesmo protegido.
	cap10 := func(dir string) Cap { return Cap{Path: dir, MaxBytes: 10 << 20, MinProtect: 24 * time.Hour} }

	// (1) Escrita fresca (1min < floor) = install vivo → PULA o dir inteiro.
	freshDir := mkdir(1 * time.Minute)
	var removedFresh []string
	rFresh := TrimByAge(Options{
		Execute: true, Now: now, InFlightFloor: floor,
		RemoveAllFn: func(p string) error { removedFresh = append(removedFresh, p); return nil },
	}, cap10(freshDir))
	if rFresh.Err != nil {
		t.Fatalf("fresh dir err: %v", rFresh.Err)
	}
	if !rFresh.SkippedInFlight {
		t.Error("dir com escrita fresca DEVE ser pulado (SkippedInFlight) sob o floor")
	}
	if rFresh.BytesFreed != 0 || len(removedFresh) != 0 {
		t.Errorf("install vivo NAO pode ser trimado: freed=%d removed=%d", rFresh.BytesFreed, len(removedFresh))
	}

	// (2) Tudo velho (2h > floor) = runaway stale → reclama normalmente.
	staleDir := mkdir(2 * time.Hour)
	var removedStale []string
	rStale := TrimByAge(Options{
		Execute: true, Now: now, InFlightFloor: floor,
		RemoveAllFn: func(p string) error { removedStale = append(removedStale, p); return nil },
	}, cap10(staleDir))
	if rStale.Err != nil {
		t.Fatalf("stale dir err: %v", rStale.Err)
	}
	if rStale.SkippedInFlight {
		t.Error("dir stale NAO deve ser pulado pelo floor")
	}
	if rStale.BytesFreed < (24<<20)-(10<<20) {
		t.Errorf("runaway stale deve ser reclamado: freed=%d, want >= %d", rStale.BytesFreed, (24<<20)-(10<<20))
	}
}
