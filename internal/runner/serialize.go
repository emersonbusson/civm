package runner

import (
	"sort"
	"strings"
)

// orgRunnerNameSuffix marca um runner registrado no nível da ORGANIZAÇÃO.
// Convenção do box (PRD civm-runner-reliability): o runner org do acme se
// chama "civm-acme-org", contra gitHubUrl https://github.com/acme, e atende
// TODOS os repos da org (acme/app + acme/civm) num único processo. Os
// runners por-repo seguem "civm-<repo>" sem o sufixo.
const orgRunnerNameSuffix = "-org"

// gateRunnerNameSuffix identifica runners do pool "civm-gate" — jobs leves de
// polling que aguardam a vez de um PR na fila FIFO sem jamais executar
// Docker/disco. Runners desse pool são registrados com `--labels civm-gate` e
// nomeados com o sufixo "-gate" (ex.: "civm-app-gate"). Não são candidatos
// à colisão porque:
//   - Atendem somente a jobs `runs-on: [self-hosted, civm-gate]`; nenhum job
//     `[self-hosted, civm]` pode aterrissar num runner gate.
//   - Não realizam operações de Docker/disco, portanto não provocam o
//     "concurrent prune on shared civm runner" que o invariante de serialização
//     guarda (incidente #1184).
//
// Convenção de nomeação: `--name civm-<owner>-gate`, logo `Status.Name` termina
// em "-gate". Como `Status` não carrega o campo Labels da API do GitHub
// (parseSystemctlList não tem acesso a ele), o sufixo do nome é o único
// discriminador confiável disponível em DetectCollisions.
const gateRunnerNameSuffix = "-gate"

// Collision descreve um runner por-repo que é REDUNDANTE porque um runner
// org da mesma organização já atende os jobs daquele repo com o mesmo label
// civm. Dois runners elegíveis para o mesmo repo = dois jobs concorrentes no
// mesmo disco/daemon Docker → "concurrent prune on shared civm runner" mata o
// docker pull de um deles (incidente #1184, validation.md 2026-06-18).
//
// O invariante de serialização: para cada org com runner org presente no box,
// NENHUM runner por-repo daquela org pode coexistir. O runner org é o
// sobrevivente canônico (1 processo serializa a org inteira em fila FIFO).
type Collision struct {
	// RepoUnit é a unit systemd do runner por-repo redundante a ser removida
	// (ex.: "actions.runner.acme-app.civm-app.service").
	RepoUnit string
	// RepoName é o nome do runner por-repo (ex.: "civm-app").
	RepoName string
	// Repo é o owner/repo servido pelo runner redundante (ex.: "acme/app").
	Repo string
	// Owner é a organização dona, derivada do runner org (ex.: "acme").
	Owner string
	// OrgUnit é a unit do runner org que torna o por-repo redundante
	// (ex.: "actions.runner.acme.civm-acme-org.service").
	OrgUnit string
	// OrgName é o nome do runner org sobrevivente (ex.: "civm-acme-org").
	OrgName string
	// RepoActive indica se o runner redundante ainda está active/running.
	// Um por-repo só DISABLED (mas loaded) continua sendo colisão: o
	// runner-watchdog o ressuscita no próximo tick (ver §nota em DetectCollisions).
	RepoActive bool
}

// isOrgRunner decide se um runner systemd é org-level. O discriminador
// confiável é o sufixo "-org" no nome do runner (Status.Name vem do unit name,
// não da config remota), combinado com o segmento de repo sem barra — um runner
// org tem gitHubUrl https://github.com/<org>, então parseRunnerUnit extrai o
// login da org puro (ex.: "acme"), nunca "owner/repo".
func isOrgRunner(s Status) bool {
	if !strings.HasSuffix(s.Name, orgRunnerNameSuffix) {
		return false
	}
	// O segmento de repo de um runner org é o login da org puro (sem "/").
	// Um runner por-repo cujo nome por acaso termine em "-org" ainda teria
	// repo "owner/repo" com barra, então não é confundido aqui.
	return s.Repo != "" && !strings.Contains(s.Repo, "/")
}

// isGateRunner decide se um runner é do pool "civm-gate". O pool carrega o
// label `civm-gate` (configurado via --labels) e adota o sufixo "-gate" no
// nome (convenção operacional). Como Status não expõe o campo Labels do GitHub,
// o sufixo exato "-gate" em Status.Name é o único discriminador disponível
// em DetectCollisions — o match é por pertencimento exato de sufixo, não por
// substring, para evitar falsos positivos com nomes que contenham "-gate-" no meio.
func isGateRunner(s Status) bool {
	return strings.HasSuffix(s.Name, gateRunnerNameSuffix)
}

// orgOwner devolve o login da org de um runner org. Para o runner org o
// segmento de repo JÁ é o login puro (sem barra), então é só o próprio Repo.
func orgOwner(s Status) string {
	return s.Repo
}

// repoOwner extrai o owner ("acme") de um runner por-repo ("acme/app").
// Devolve "" se o Status não tiver a forma owner/repo.
func repoOwner(s Status) string {
	idx := strings.Index(s.Repo, "/")
	if idx <= 0 {
		return ""
	}
	return s.Repo[:idx]
}

// DetectCollisions percorre os runners systemd do box e devolve, em ordem
// estável por unit, cada runner por-repo que colide com um runner org da mesma
// organização. Lista vazia = box já serializado (invariante satisfeito).
//
// Função PURA (sem I/O): recebe o snapshot de runner.List() e é exercida por
// unit tests herméticos. O enforcement (remover a unit redundante) e o guard
// (doctor reportar crítico) consomem este resultado.
//
// NOTA sobre o watchdog: NÃO basta `systemctl disable` o runner por-repo. Um
// runner só desativado continua loaded; runner.List() ainda o devolve
// inactive/dead, e restartCandidates() (runner-watchdog, tick de ~2min) o trata
// como "runner caído" e dá `systemctl restart`, RESSUSCITANDO a colisão. Por
// isso o estado durável correto é a REMOÇÃO completa do runner por-repo
// (svc.sh stop+uninstall + config.sh remove + rm -rf) — assim List() nunca mais
// o vê. RepoActive=true sinaliza que ele ainda está de pé e precisa de remoção.
//
// Runners do pool "civm-gate" (sufixo "-gate" no nome, label `civm-gate`) são
// EXCLUÍDOS da detecção de colisão: um runner gate nunca recebe jobs
// `[self-hosted, civm]` e não realiza Docker/disco, portanto não viola o
// invariante de serialização.
func DetectCollisions(units []Status) []Collision {
	// 1) Indexa os runners org por login de org. Runners do pool civm-gate são
	//    pulados: um runner "-gate" nunca é o runner org canônico, e incluí-lo
	//    aqui faria um gate runner ser incorretamente tratado como org runner caso
	//    o nome termine em "-org" por coincidência futura. A verificação via
	//    isGateRunner tem precedência: o sufixo "-gate" exclui antes de qualquer
	//    comparação de "-org".
	//    Mais de um runner org da mesma org é improvável, mas se houver, o
	//    primeiro em ordem estável é o "sobrevivente" referenciado nos diagnósticos.
	orgByOwner := map[string]Status{}
	for _, u := range units {
		if isGateRunner(u) {
			continue
		}
		if !isOrgRunner(u) {
			continue
		}
		owner := orgOwner(u)
		if owner == "" {
			continue
		}
		if _, seen := orgByOwner[owner]; !seen {
			orgByOwner[owner] = u
		}
	}
	if len(orgByOwner) == 0 {
		return nil
	}

	// 2) Para cada runner por-repo cujo owner tem runner org, registra colisão.
	//    Runners do pool civm-gate são excluídos: um runner gate atende somente
	//    jobs com label `civm-gate` e não concorre com o runner org pelo mesmo job.
	var collisions []Collision
	for _, u := range units {
		if isGateRunner(u) {
			continue
		}
		if isOrgRunner(u) {
			continue
		}
		owner := repoOwner(u)
		if owner == "" {
			continue
		}
		org, ok := orgByOwner[owner]
		if !ok {
			continue
		}
		collisions = append(collisions, Collision{
			RepoUnit:   u.UnitName,
			RepoName:   u.Name,
			Repo:       u.Repo,
			Owner:      owner,
			OrgUnit:    org.UnitName,
			OrgName:    org.Name,
			RepoActive: u.ActiveState == "active" && u.SubState == "running",
		})
	}
	sort.Slice(collisions, func(i, j int) bool {
		return collisions[i].RepoUnit < collisions[j].RepoUnit
	})
	return collisions
}
