// Package cachetrim bounds the size of regenerable build/dependency caches under
// the runner home(s) so a shared CI runner's caches cannot grow unbounded and
// fill the host VHDX volume (the 2026-06 PausedCritical incident: the acme
// workflows point GOCACHE/yarn cache-folder to named per-workflow dirs —
// ~/.cache/go-build-acme-services hit 13GB — that the old fixed-path cap never
// matched). It is the SINGLE SOURCE of the cache-cap policy, consumed by both
// the job hooks (internal/hook, runs as the runner user) and the disk-pressure
// cleanup (internal/cleanup, runs as root over all /home/* runner homes).
package cachetrim

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
)

// Cap is a per-directory size budget: trim Path to at most MaxBytes by removing
// the oldest units, preserving anything modified within MinProtect.
//
// PackageDepth controla a granularidade do trim:
//
//   - 0 (default) — por arquivo. Vale para todo cache cujo arquivo já é uma
//     unidade completa e independente: Go build, golangci, e os caches
//     content-addressed (npm `_cacache`, pnpm store, zips do yarn berry). Nesses,
//     remover um arquivo remove uma unidade inteira e a ferramenta re-busca o que
//     faltar — nunca há estado parcial.
//   - N > 0 — por diretório de pacote. Agrupa os arquivos pelos N primeiros
//     segmentos de path (o diretório de pacote) e remove o pacote INTEIRO. É
//     necessário quando o pacote é um diretório multi-arquivo cuja remoção parcial
//     corrompe. Hoje o único caso é o yarn v1 (`<root>/v6/<pkg>/...`,
//     PackageDepth=2): tirar um arquivo do meio deixa o pacote parcial e o
//     `yarn install --frozen-lockfile` quebra com ENOENT em vez de re-buscar.
//
// Um manager futuro com pacote em diretório (ex.: outra estrutura de profundidade
// fixa) entra setando a sua PackageDepth.
//
// WipeWhole é a terceira via, para caches cuja entrada tem referências CRUZADAS
// opacas que o trim externo não consegue agrupar: o Go build cache guarda cada
// entrada como par `<actionID>-a` + `<outputID>-d`, e o `-a` aponta o `-d` por um
// hash de conteúdo que pode estar em OUTRO diretório de prefixo — remover qualquer
// um dos dois orfana a entrada e o `go vet` quebra com "can't import facts ... no
// such file or directory" (golangci é igual). Como nem por arquivo nem por
// diretório é seguro, acima do cap só o wipe do diretório inteiro é atômico. É um
// backstop: o go/golangci auto-trimam o working-set normal (entradas > 5 dias), e
// o cap fica generoso para o wipe só disparar em crescimento descontrolado.
// Ver docs/specs/cachetrim-yarn-atomic.
type Cap struct {
	Path         string
	MaxBytes     int64
	MinProtect   time.Duration
	PackageDepth int
	WipeWhole    bool
}

// Deps injects filesystem access so callers and tests stay hermetic.
type Deps struct {
	GlobFn func(pattern string) ([]string, error)
	StatFn func(path string) (os.FileInfo, error)
}

func (d Deps) withDefaults() Deps {
	if d.GlobFn == nil {
		d.GlobFn = filepath.Glob
	}
	if d.StatFn == nil {
		d.StatFn = os.Stat
	}
	return d
}

func (d Deps) glob(pattern string) []string {
	m, _ := d.GlobFn(pattern)
	return m
}

// existingDirs keeps only existing directories, deduplicated by cleaned path.
// Glob already returns existing entries; the fixed extras (e.g. ~/.yarn/cache)
// pass through the same filter so a missing dir does not skew the family budget
// division.
func (d Deps) existingDirs(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		clean := filepath.Clean(p)
		if _, ok := seen[clean]; ok {
			continue
		}
		fi, err := d.StatFn(clean)
		if err != nil || !fi.IsDir() {
			continue
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}
	return out
}

// Caps returns the family-capped cache dirs across the given home roots. Each
// family budget (civm.DefaultCache*MaxGB) is divided among the named variants
// found by glob, so the family total stays bounded regardless of how many
// per-workflow dirs exist. The hook passes a single home (the runner user); the
// root-run cleanup passes every /home/* runner home so caches are bounded even
// when no job is starting (closes the disk-watchdog gap).
func Caps(homes []string, deps Deps) []Cap {
	deps = deps.withDefaults()
	const giB = int64(1) << 30
	protect := time.Duration(civm.DefaultCacheTrimMinProtectHours) * time.Hour
	var caps []Cap
	// family expands a glob (plus optional fixed sub-paths) across every home into
	// one Cap per existing dir, splitting the family budget evenly so the family
	// total is bounded no matter how many named variants exist.
	family := func(familyMaxGB, pkgDepth int, wipeWhole bool, glob string, extraSubs ...string) {
		var dirs []string
		for _, home := range homes {
			if home == "" {
				continue
			}
			dirs = append(dirs, deps.glob(filepath.Join(home, glob))...)
			for _, sub := range extraSubs {
				p := filepath.Join(home, sub)
				if strings.ContainsAny(sub, "*?[") {
					dirs = append(dirs, deps.glob(p)...)
					continue
				}
				dirs = append(dirs, p)
			}
		}
		dirs = deps.existingDirs(dirs)
		if len(dirs) == 0 {
			return
		}
		per := int64(familyMaxGB) * giB / int64(len(dirs))
		if per < 1 {
			per = 1
		}
		for _, d := range dirs {
			caps = append(caps, Cap{Path: d, MaxBytes: per, MinProtect: protect, PackageDepth: pkgDepth, WipeWhole: wipeWhole})
		}
	}
	// yarn v1: pacote = diretório multi-arquivo em `<root>/v6/<pkg>` → atômico por
	// pacote (PackageDepth 2). Yarn Berry pode aparecer como `.yarn/cache` no repo
	// ou como cache externo nomeado (`~/.yarn-berry-<org>`); os zips rasos caem no
	// modo por arquivo sozinho. go-build/golangci: entrada com refs cruzadas opacas
	// (o `-a` aponta um `-d` em outro prefixo) → não dá para sub-trimar, só
	// WipeWhole acima do cap (backstop; ambos auto-trimam o working-set normal).
	// npm/pnpm (abaixo) são content-addressed → seguros por arquivo.
	family(civm.DefaultCacheGoBuildMaxGB, 0, true, ".cache/go-build*")
	family(civm.DefaultCacheYarnMaxGB, 2, false, ".cache/yarn*", ".yarn-berry-*", ".yarn/cache")
	family(civm.DefaultCacheGolangciLintMaxGB, 0, true, ".cache/golangci-lint*")
	family(civm.DefaultCachePlaywrightMaxGB, 0, true, ".cache/ms-playwright")
	// npm/pnpm use a single well-known dir per home (no named variants) and the
	// budget is not divided, so no division skew — they enter per home
	// unconditionally (trim/wipe on an absent dir is a no-op).
	for _, home := range homes {
		if home == "" {
			continue
		}
		caps = append(caps,
			Cap{Path: filepath.Join(home, ".npm", "_cacache"), MaxBytes: int64(civm.DefaultCacheNPMMaxGB) * giB, MinProtect: protect},
			Cap{Path: filepath.Join(home, ".pnpm-store"), MaxBytes: int64(civm.DefaultCachePNPMMaxGB) * giB, MinProtect: protect},
		)
	}
	return caps
}

// Paths returns just the paths of caps — the wipe-mode set (disk-pressure purge).
func Paths(caps []Cap) []string {
	if len(caps) == 0 {
		return nil
	}
	paths := make([]string, len(caps))
	for i, c := range caps {
		paths[i] = c.Path
	}
	return paths
}

// Options control one trim run.
type Options struct {
	Execute     bool
	Now         time.Time
	WalkDirFn   func(root string, fn fs.WalkDirFunc) error
	RemoveAllFn func(path string) error
	// InFlightFloor, quando > 0, faz o trim PULAR qualquer dir de cache que tenha
	// um arquivo escrito dentro desse intervalo — um dir com escrita fresca é um
	// install vivo. Setado só no emergency path (≥75% de disco, onde o idle guard
	// é ignorado); no path normal fica 0 e o teto-hard de sempre vale. Sem ele, o
	// Pass 2 (que ignora MinProtect) deletaria o pacote que o yarn/go está
	// escrevendo → parcial → ENOENT → job morto mid-install. Kahneman #16: o
	// fail-safe é o disco, mas nunca às custas de matar o job que ele preserva.
	InFlightFloor time.Duration
}

func (o Options) withDefaults() Options {
	if o.WalkDirFn == nil {
		o.WalkDirFn = filepath.WalkDir
	}
	if o.RemoveAllFn == nil {
		o.RemoveAllFn = os.RemoveAll
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	return o
}

// Result is one cache dir's trim outcome.
type Result struct {
	Path       string
	BytesFound int64
	BytesFreed int64
	Executed   bool
	Err        error
	// SkippedInFlight indica que o dir tinha escrita fresca (< InFlightFloor) e
	// foi pulado inteiro para não matar um install vivo. Observável para o teste
	// e para o log de decisão do emergency reclaim.
	SkippedInFlight bool
}

type cacheEntry struct {
	path  string
	size  int64
	mtime time.Time
}

// collectUnits reúne as unidades de trim de um cache e o total de bytes. Com
// PackageDepth 0, cada arquivo é uma unidade — Go build / golangci e os caches
// content-addressed, onde cada arquivo já é uma entrada completa. Com PackageDepth
// > 0 (yarn v1), os arquivos sob um diretório de pacote são agregados em UMA
// unidade identificada por esse diretório (size = soma, mtime = o mais novo, para
// o MinProtect proteger pacote recém-escrito); arquivos rasos (zips do yarn berry,
// locks, `.tmp`) entram como unidades de arquivo. Assim o RemoveAll remove um
// pacote inteiro, nunca parcial.
func collectUnits(c Cap, walkDir func(string, fs.WalkDirFunc) error) ([]cacheEntry, int64, error) {
	type agg struct {
		size  int64
		mtime time.Time
	}
	pkgs := make(map[string]*agg)
	var files []cacheEntry
	var total int64
	err := walkDir(c.Path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		total += info.Size()
		if root := packageRoot(c, p); root != "" {
			a := pkgs[root]
			if a == nil {
				a = &agg{}
				pkgs[root] = a
			}
			a.size += info.Size()
			if info.ModTime().After(a.mtime) {
				a.mtime = info.ModTime()
			}
			return nil
		}
		files = append(files, cacheEntry{path: p, size: info.Size(), mtime: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, total, err
	}
	units := files
	for root, a := range pkgs {
		units = append(units, cacheEntry{path: root, size: a.size, mtime: a.mtime})
	}
	return units, total, nil
}

// packageRoot devolve o diretório de pacote atômico (os PackageDepth primeiros
// segmentos do path de p) que contém o arquivo p, ou "" se p deve ser tratado como
// arquivo solto. Só age com PackageDepth > 0. Para o yarn v1 (PackageDepth=2) o
// pacote fica em `<root>/<ver>/<pkg>` e seus arquivos estão mais fundo; arquivos
// rasos (no próprio dir de pacote ou acima — ex.: zips do berry, locks, `.tmp`)
// retornam "" e caem no modo por arquivo.
func packageRoot(c Cap, p string) string {
	if c.PackageDepth <= 0 {
		return ""
	}
	rel, err := filepath.Rel(c.Path, p)
	if err != nil {
		return ""
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) <= c.PackageDepth {
		return ""
	}
	return filepath.Join(append([]string{c.Path}, parts[:c.PackageDepth]...)...)
}

// TrimByAge walks c.Path, sorts files by mtime ascending, and removes the oldest
// until total <= c.MaxBytes. Files newer than Now-c.MinProtect are preserved —
// protects the hot cache of a job that just wrote. No-op if the cache is absent
// or already under the cap.
//
// É só o orquestrador: a sequência de guards de short-circuit (unsafe-path,
// walk-error, under-cap, in-flight floor, WipeWhole) vive em trimGuards, e o
// trim em si, por idade e em dois passes, em trimEntries.
func TrimByAge(opts Options, c Cap) Result {
	opts = opts.withDefaults()
	r := Result{Path: c.Path, Executed: opts.Execute}

	// trimGuards resolve os casos de short-circuit. Quando done=true, r já carrega
	// o resultado final (no-op, erro, skip ou wipe) e não há nada a trimar.
	entries, total, done := trimGuards(opts, c, &r)
	if done {
		return r
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].mtime.Before(entries[j].mtime) })
	target := total - c.MaxBytes
	removed := make([]bool, len(entries))

	// MaxBytes e um TETO HARD (a garantia anti-enchimento). trimEntries percorre do
	// mais antigo ao mais novo. Pass 1 (allowProtected=false) preserva os arquivos
	// quentes (acessados dentro de MinProtect). Se isso NAO alcanca o cap — o caso
	// do cache de CI sob carga continua, onde TODO arquivo e recente e o cap nunca
	// se aplicava (yarn-acme-* cresceu a 18GB, incidente 2026-06-15) — Pass 2 trima
	// os protegidos tambem, do mais antigo ao mais novo, ate o cap. A protecao de
	// disco vence a temperatura do cache (Kahneman #16: o fail-safe e o disco).
	freed, err := trimEntries(entries, removed, target, 0, false, opts, c)
	if err != nil {
		r.Err = err
		r.BytesFreed = freed
		return r
	}
	if freed < target {
		freed, err = trimEntries(entries, removed, target, freed, true, opts, c)
		if err != nil {
			r.Err = err
		}
	}
	r.BytesFreed = freed
	return r
}

// trimGuards roda o preâmbulo de short-circuits do TrimByAge e devolve as
// unidades coletadas, o total de bytes e done=true quando o caller deve retornar
// imediatamente com r já preenchido. Cobre, nesta ordem: path inseguro (vazio,
// "/" ou o próprio HOME), erro de walk (dir ausente = no-op), dir já abaixo do
// cap, o floor in-flight (pula o install vivo) e o WipeWhole (backstop atômico do
// go-build/golangci). Quando done=false, há trabalho de trim real a fazer.
func trimGuards(opts Options, c Cap, r *Result) (entries []cacheEntry, total int64, done bool) {
	if strings.TrimSpace(c.Path) == "" || c.Path == "/" || c.Path == os.Getenv("HOME") {
		r.Err = errors.New("unsafe cache path")
		return nil, 0, true
	}
	entries, total, walkErr := collectUnits(c, opts.WalkDirFn)
	if walkErr != nil {
		if errors.Is(walkErr, fs.ErrNotExist) {
			return nil, 0, true
		}
		r.Err = walkErr
		return nil, 0, true
	}
	r.BytesFound = total
	if total <= c.MaxBytes {
		return nil, total, true
	}
	// In-flight floor (só no emergency path): um dir com escrita muito recente é
	// um install vivo. Sob o bypass de disco (sem idle guard) trimá-lo mataria o
	// job — parcial → ENOENT. Pula o dir inteiro; o alívio de disco vem de
	// docker/tmp e da recusa de job no piso, nunca de um cache em uso. Vale para
	// WipeWhole (go-build/golangci) e para o trim por pacote (yarn).
	if opts.InFlightFloor > 0 {
		floor := opts.Now.Add(-opts.InFlightFloor)
		for i := range entries {
			if entries[i].mtime.After(floor) {
				r.SkippedInFlight = true
				return nil, total, true
			}
		}
	}
	if c.WipeWhole {
		// Cache com refs cruzadas opacas (go-build/golangci): sub-trimar orfana uma
		// entrada. Acima do cap, só o wipe do dir inteiro é atômico — backstop raro.
		if opts.Execute {
			if err := opts.RemoveAllFn(c.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				r.Err = err
				return nil, total, true
			}
		}
		r.BytesFreed = total
		return nil, total, true
	}
	return entries, total, false
}

// trimEntries remove as unidades mais antigas até freed alcançar target, contando
// a partir do freed já liberado num pass anterior. Com allowProtected=false (Pass
// 1) pula as unidades quentes (mtime depois do cutoff de proteção, sob MinProtect
// > 0); com true (Pass 2) trima até os protegidos — o TETO HARD do cap vence a
// temperatura do cache. O slice removed é compartilhado entre os passes (mutado in
// place) para um pass não reprocessar o que o outro já tirou. Devolve o freed
// acumulado e o primeiro erro de remoção (ErrNotExist conta como já removido).
func trimEntries(entries []cacheEntry, removed []bool, target, freed int64, allowProtected bool, opts Options, c Cap) (int64, error) {
	// O cutoff de proteção é Now − MinProtect: tudo escrito depois dele é quente.
	protectCutoff := opts.Now.Add(-c.MinProtect)
	for i := range entries {
		if freed >= target {
			return freed, nil
		}
		if removed[i] {
			continue
		}
		if !allowProtected && c.MinProtect > 0 && entries[i].mtime.After(protectCutoff) {
			continue
		}
		if opts.Execute {
			if err := opts.RemoveAllFn(entries[i].path); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					removed[i] = true
					continue
				}
				return freed, err
			}
		}
		removed[i] = true
		freed += entries[i].size
	}
	return freed, nil
}
