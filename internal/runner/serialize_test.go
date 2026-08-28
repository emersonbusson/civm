package runner

import (
	"context"
	"testing"
)

// orgUnit / repoUnit constroem Status como runner.List() os produziria a partir
// dos unit names reais do box (parseRunnerUnit faz o split owner-repo.name).
func mkStatus(unit, active, sub string) Status {
	repo, name := parseRunnerUnit(unit)
	return Status{UnitName: unit, Repo: repo, Name: name, ActiveState: active, SubState: sub}
}

const (
	unitOrgAcme  = "actions.runner.acme.civm-acme-org.service"
	unitRepoAcme = "actions.runner.acme-app.civm-app.service"
	unitRepoCivm  = "actions.runner.acme-civm.civm-self.service"
	unitVitae     = "actions.runner.other-peer.civm-peer.service"
)

func TestDetectCollisions_AcmeOrgPlusRepoActive(t *testing.T) {
	t.Parallel()
	// O estado exato que quebrou o #1184: org + repo, ambos active.
	units := []Status{
		mkStatus(unitOrgAcme, "active", "running"),
		mkStatus(unitRepoAcme, "active", "running"),
		mkStatus(unitVitae, "active", "running"),
	}
	got := DetectCollisions(units)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 collision (%+v)", len(got), got)
	}
	c := got[0]
	if c.RepoUnit != unitRepoAcme {
		t.Errorf("RepoUnit = %q, want %q", c.RepoUnit, unitRepoAcme)
	}
	if c.Repo != "acme/app" {
		t.Errorf("Repo = %q, want acme/app", c.Repo)
	}
	if c.Owner != "acme" {
		t.Errorf("Owner = %q, want acme", c.Owner)
	}
	if c.OrgUnit != unitOrgAcme || c.OrgName != "civm-acme-org" {
		t.Errorf("OrgUnit/OrgName = %q/%q, want %q/civm-acme-org", c.OrgUnit, c.OrgName, unitOrgAcme)
	}
	if !c.RepoActive {
		t.Errorf("RepoActive = false, want true (runner ainda de pé)")
	}
}

func TestDetectCollisions_DisabledRepoStillCollides(t *testing.T) {
	t.Parallel()
	// O fix manual foi só `systemctl disable`: a unit fica loaded, inactive/dead.
	// Ainda É colisão — o watchdog a ressuscitaria. RepoActive deve ser false
	// para o enforcement saber que basta `config.sh remove` (já parada).
	units := []Status{
		mkStatus(unitOrgAcme, "active", "running"),
		mkStatus(unitRepoAcme, "inactive", "dead"),
	}
	got := DetectCollisions(units)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (disabled-but-loaded ainda colide)", len(got))
	}
	if got[0].RepoActive {
		t.Errorf("RepoActive = true, want false para unit inactive/dead")
	}
}

func TestDetectCollisions_OrgOnly_NoCollision(t *testing.T) {
	t.Parallel()
	// Estado durável alvo: só o runner org do acme sobrevive.
	units := []Status{
		mkStatus(unitOrgAcme, "active", "running"),
		mkStatus(unitVitae, "active", "running"),
	}
	if got := DetectCollisions(units); len(got) != 0 {
		t.Fatalf("len = %d, want 0 (só org = serializado) (%+v)", len(got), got)
	}
}

func TestDetectCollisions_NoOrgRunner_NoCollision(t *testing.T) {
	t.Parallel()
	// Sem runner org, runners por-repo são o padrão legítimo (peers pessoais).
	// Não inventar colisão onde não há org para serializar.
	units := []Status{
		mkStatus(unitRepoAcme, "active", "running"),
		mkStatus(unitVitae, "active", "running"),
	}
	if got := DetectCollisions(units); len(got) != 0 {
		t.Fatalf("len = %d, want 0 (sem org runner) (%+v)", len(got), got)
	}
}

func TestDetectCollisions_OrgServesMultipleOwnerRepos(t *testing.T) {
	t.Parallel()
	// O runner org do acme serve acme/app E acme/civm. Se ALGUÉM
	// registrasse runners por-repo para os DOIS, ambos colidem.
	units := []Status{
		mkStatus(unitOrgAcme, "active", "running"),
		mkStatus(unitRepoAcme, "active", "running"),
		mkStatus(unitRepoCivm, "active", "running"), // repo acme/civm
		mkStatus(unitVitae, "active", "running"),    // owner other: NÃO colide
	}
	got := DetectCollisions(units)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (%+v)", len(got), got)
	}
	// Ordem estável por unit: acme-acme antes de acme-civm.
	if got[0].RepoUnit != unitRepoAcme || got[1].RepoUnit != unitRepoCivm {
		t.Errorf("ordem inesperada: %q, %q", got[0].RepoUnit, got[1].RepoUnit)
	}
	for _, c := range got {
		if c.Owner != "acme" {
			t.Errorf("Owner = %q, want acme", c.Owner)
		}
	}
}

func TestDetectCollisions_DifferentOwnerNotAffected(t *testing.T) {
	t.Parallel()
	// Um runner org do acme NÃO torna o runner por-repo de outro owner
	// (other/peer) redundante. Sem falso positivo cross-owner.
	units := []Status{
		mkStatus(unitOrgAcme, "active", "running"),
		mkStatus(unitVitae, "active", "running"),
	}
	if got := DetectCollisions(units); len(got) != 0 {
		t.Fatalf("len = %d, want 0 (owner diferente) (%+v)", len(got), got)
	}
}

func TestDetectCollisions_Empty(t *testing.T) {
	t.Parallel()
	if got := DetectCollisions(nil); got != nil {
		t.Fatalf("DetectCollisions(nil) = %+v, want nil", got)
	}
	if got := DetectCollisions([]Status{}); len(got) != 0 {
		t.Fatalf("DetectCollisions([]) len = %d, want 0", len(got))
	}
}

func TestIsOrgRunner(t *testing.T) {
	t.Parallel()
	cases := []struct {
		unit string
		want bool
	}{
		{unitOrgAcme, true},   // org: nome -org + repo sem barra
		{unitRepoAcme, false}, // por-repo: repo acme/app
		{unitVitae, false},     // por-repo pessoal
		{"actions.runner.acme-org.civm-x.service", false}, // repo "acme/org" (tem barra) — não é org runner
	}
	for _, c := range cases {
		got := isOrgRunner(mkStatus(c.unit, "active", "running"))
		if got != c.want {
			repo, name := parseRunnerUnit(c.unit)
			t.Errorf("isOrgRunner(%q -> repo=%q name=%q) = %v, want %v", c.unit, repo, name, got, c.want)
		}
	}
}

// TestDetectCollisions_FromListSnapshot prova o caminho ponta-a-ponta a partir
// da saída crua do systemctl, exatamente como o doctor o consome via runner.List.
func TestDetectCollisions_FromListSnapshot(t *testing.T) {
	t.Parallel()
	out := "" +
		"  actions.runner.acme.civm-acme-org.service        loaded active running GitHub Actions Runner (acme.civm-acme-org)\n" +
		"  actions.runner.acme-app.civm-app.service      loaded active running GitHub Actions Runner (acme-acme.civm-app)\n" +
		"  actions.runner.other-peer.civm-peer.service loaded active running GitHub Actions Runner (peer)\n"
	o := DefaultListOptions()
	o.RunFn = func(context.Context, string, ...string) ([]byte, error) { return []byte(out), nil }
	items, err := List(context.Background(), o)
	if err != nil {
		t.Fatalf("List err = %v", err)
	}
	got := DetectCollisions(items)
	if len(got) != 1 || got[0].Repo != "acme/app" {
		t.Fatalf("DetectCollisions sobre snapshot real = %+v, want 1 colisão acme/app", got)
	}
}

// unitGateAcme representa um runner do pool civm-gate registrado para o acme.
// Nomeado "civm-app-gate" (convenção: civm-<owner>-gate), label `civm-gate`.
// Unit: actions.runner.acme-app.civm-app-gate.service — mesmo owner/repo
// que o runner por-repo padrão, mas sufixo "-gate" no nome.
const unitGateAcme = "actions.runner.acme-app.civm-app-gate.service"

func TestDetectCollisions_GateRunnerPlusOrg_NoCollision(t *testing.T) {
	t.Parallel()
	// Um runner civm-gate coexistindo com o runner org NÃO deve ser reportado
	// como colisão: ele atende somente jobs `[self-hosted, civm-gate]` e não
	// realiza Docker/disco, portanto não viola o invariante de serialização
	// (incidente #1184).
	units := []Status{
		mkStatus(unitOrgAcme, "active", "running"),
		mkStatus(unitGateAcme, "active", "running"),
		mkStatus(unitVitae, "active", "running"),
	}
	if got := DetectCollisions(units); len(got) != 0 {
		t.Fatalf("len = %d, want 0 (gate runner não é colisão) (%+v)", len(got), got)
	}
}

func TestDetectCollisions_GateRunnerDoesNotSuppressPlainRepoCollision(t *testing.T) {
	t.Parallel()
	// A exclusão do runner gate NÃO deve suprimir a colisão do runner por-repo
	// padrão: quando org + gate + repo-plain coexistem, só o repo-plain colide.
	units := []Status{
		mkStatus(unitOrgAcme, "active", "running"),
		mkStatus(unitGateAcme, "active", "running"),
		mkStatus(unitRepoAcme, "active", "running"),
		mkStatus(unitVitae, "active", "running"),
	}
	got := DetectCollisions(units)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (só o runner por-repo padrão colide) (%+v)", len(got), got)
	}
	c := got[0]
	if c.RepoUnit != unitRepoAcme {
		t.Errorf("RepoUnit = %q, want %q (runner gate não deve aparecer)", c.RepoUnit, unitRepoAcme)
	}
	if c.RepoName != "civm-app" {
		t.Errorf("RepoName = %q, want civm-app", c.RepoName)
	}
	if c.Owner != "acme" {
		t.Errorf("Owner = %q, want acme", c.Owner)
	}
}
