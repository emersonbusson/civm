package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// unitScriptRe captura o caminho absoluto de um script /usr/local/bin/*.sh
// referenciado por uma unit systemd, tanto em ExecStart= quanto em
// ConditionPathExists=. A linha ExecStart pode envolver o script (ex.:
// `/usr/bin/flock -n ... /usr/local/bin/x.sh`), então casamos o token em
// qualquer posição da linha, não só logo após o `=`.
var unitScriptRe = regexp.MustCompile(`/usr/local/bin/[A-Za-z0-9._-]+\.sh`)

// checkUnitScriptsInstalled prova que todo script /usr/local/bin/*.sh
// referenciado pelas units civmctl-*.service de deploy/systemd está de fato
// instalado no host. O porquê: as units carregam
// `ConditionPathExists=/usr/local/bin/X.sh` + `ExecStart=/usr/local/bin/X.sh`;
// se o script NÃO está instalado, o systemd PULA a unit em SILÊNCIO (a condição
// falha, não há erro no journal) — foi exatamente o que deixou o
// civmctl-buildcache-prune virar no-op e o build cache nunca ser podado
// (ADR-107). Existência da unit != função (testing.md, Kahneman #13): a unit
// existir não garante que o script-alvo exista. Crítico quando algum script
// está ausente; aponta o reparo (`civmctl bootstrap` reinstala os scripts).
//
// Só checamos scripts *.sh — units que apontam para /usr/local/bin/civmctl (o
// binário, não um .sh) já têm a cobertura do bootstrap que instala o binário e
// dos próprios checks de runner; aqui o alvo é o gap específico dos scripts de
// deploy/bin (CIVM-6).
func checkUnitScriptsInstalled(opts Options) HookCheck {
	const name = "UNIT_SCRIPTS_INSTALLED"
	pattern := filepath.Join(opts.UnitsSourceDir, "civmctl-*.service")
	units, err := opts.GlobFn(pattern)
	if err != nil {
		return HookCheck{Name: name, Severity: SeverityWarning, Detail: fmt.Sprintf("glob %s: %v", pattern, err)}
	}
	if len(units) == 0 {
		// Sem units-fonte legíveis não há o que provar; WARN para não mascarar
		// o gap como verde (fail-safe, Kahneman #16) sem inflar para crítico
		// num host onde o repo não está montado em UnitsSourceDir.
		return HookCheck{Name: name, Severity: SeverityWarning, Detail: fmt.Sprintf("nenhuma unit civmctl-*.service em %s", opts.UnitsSourceDir)}
	}
	sort.Strings(units)

	// Dedup: o mesmo script aparece em ConditionPathExists e ExecStart da mesma
	// unit; reportamos uma referência por (script, unit) usando a unit como
	// contexto da mensagem.
	type ref struct{ script, unit string }
	seen := map[ref]bool{}
	var missing []string
	checked := 0
	for _, unitPath := range units {
		data, rerr := opts.ReadFileFn(unitPath)
		if rerr != nil {
			missing = append(missing, fmt.Sprintf("%s ilegível: %v", filepath.Base(unitPath), rerr))
			continue
		}
		unitName := filepath.Base(unitPath)
		for _, script := range extractUnitScripts(string(data)) {
			r := ref{script: script, unit: unitName}
			if seen[r] {
				continue
			}
			seen[r] = true
			checked++
			if _, serr := opts.StatFn(script); serr != nil {
				if os.IsNotExist(serr) {
					missing = append(missing, fmt.Sprintf("%s (referenciado por %s) ausente — systemd PULA a unit em silêncio", script, unitName))
				} else {
					missing = append(missing, fmt.Sprintf("%s (referenciado por %s) stat falhou: %v", script, unitName, serr))
				}
			}
		}
	}
	if len(missing) > 0 {
		return HookCheck{
			Name:     name,
			Severity: SeverityCritical,
			Detail:   "scripts de unit ausentes (reinstale com civmctl bootstrap): " + strings.Join(missing, "; "),
		}
	}
	if checked == 0 {
		return HookCheck{Name: name, Severity: SeverityOK, Detail: fmt.Sprintf("%d unit(s) sem script /usr/local/bin/*.sh a verificar", len(units))}
	}
	return HookCheck{Name: name, Severity: SeverityOK, Detail: fmt.Sprintf("%d script(s) /usr/local/bin/*.sh referenciados por units instalados", checked)}
}

// extractUnitScripts varre as linhas de uma unit systemd e devolve, em ordem
// estável, os caminhos /usr/local/bin/*.sh citados em ExecStart= ou
// ConditionPathExists=. Outras diretivas são ignoradas (não nos interessa o
// /usr/bin/flock que envolve o script, só o alvo .sh).
func extractUnitScripts(content string) []string {
	var scripts []string
	seen := map[string]bool{}
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "ExecStart=") && !strings.HasPrefix(trimmed, "ConditionPathExists=") {
			continue
		}
		for _, match := range unitScriptRe.FindAllString(trimmed, -1) {
			if seen[match] {
				continue
			}
			seen[match] = true
			scripts = append(scripts, match)
		}
	}
	return scripts
}
