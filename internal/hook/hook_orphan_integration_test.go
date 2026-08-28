//go:build integration

package hook

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
)

// TestIntegrationReapOrphanFreesRealPort é o gate de EFEITO REAL do reaper de
// órfão (disciplina #13: existência ≠ função). Ele não verifica que "uma função
// foi chamada" — ele:
//
//  1. sobe um container REAL publicando uma host port FIXA de CI em 127.0.0.1,
//     simulando o stack órfão que segurou a porta no incidente 2026-06-19;
//  2. prova que a porta está PRESA (um bind no mesmo endereço falha);
//  3. roda reapOrphanCIContainers com o defaultRun REAL (docker de verdade);
//  4. afirma que a porta foi LIBERADA — o container sumiu e o mesmo bind agora
//     sucede.
//
// Um mock de "docker rm foi chamado" não provaria nada disto: o ponto é que a
// alocação da porta no kernel/daemon realmente saiu. Self-skip quando docker
// está ausente, então é no-op em runner efêmero sem docker e gate real na box
// self-hosted (mesmo padrão de safedelete_integration_test.go).
func TestIntegrationReapOrphanFreesRealPort(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; cannot exercise the real port-reap effect")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skipf("docker daemon not reachable: %v", err)
	}

	// Escolhe a PRIMEIRA porta fixa de CI que está livre AGORA, para não colidir
	// com algo real rodando na box. Se nenhuma estiver livre, pula (não falha) —
	// um ambiente onde todas as portas fixas estão ocupadas não é o cenário sob
	// teste.
	port := firstFreeFixedPort(t)
	if port == 0 {
		t.Skip("no civm CI fixed host port is currently free; cannot stage the orphan")
	}

	const name = "civm-reaper-itest"
	// Garante limpeza mesmo se uma execução anterior do teste vazou o container.
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", name).Run()
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 30*time.Second)
		defer cc()
		_ = exec.CommandContext(c, "docker", "rm", "-f", name).Run()
	})

	// Sobe o "órfão": busybox dormindo, com a host port fixa publicada e o label
	// com.docker.compose.project começando com "acme" — exatamente o que um stack
	// do acme deixado por um run/runner anterior carregaria. Assim o reaper o
	// pega tanto pelo sinal primário (label) quanto pela defesa em profundidade
	// (porta fixa).
	publish := fmt.Sprintf("127.0.0.1:%d:80", port)
	label := "com.docker.compose.project=" + civm.DefaultCIOrphanProjectPrefix + "-integration-orphan"
	runOut, err := exec.CommandContext(ctx, "docker", "run", "-d",
		"--name", name, "--label", label, "-p", publish,
		"busybox:latest", "sleep", "600").CombinedOutput()
	if err != nil {
		t.Skipf("could not stage orphan container (image pull/run failed): %v: %s", err, runOut)
	}

	// O órfão deve REALMENTE estar segurando a porta: um bind direto no mesmo
	// endereço tem de falhar. Se não falhar, o setup não reproduz o incidente.
	if portIsFree(port) {
		t.Fatalf("staged orphan did not actually hold 127.0.0.1:%d — setup invalid", port)
	}

	// Executa o reaper com o docker REAL (defaultRun via applyDefaults).
	opts := Options{Execute: true}
	applyDefaults(&opts)
	actions := reapOrphanCIContainers(ctx, opts)
	if len(actions) != 1 {
		t.Fatalf("expected 1 reaper action, got %+v", actions)
	}
	if actions[0].Error != "" {
		t.Fatalf("reaper must not error against a real orphan: %+v", actions[0])
	}

	// EFEITO REAL #1: o container sumiu.
	if containerExists(ctx, name) {
		t.Fatalf("orphan container %q still exists after reap; warning=%q", name, actions[0].Warning)
	}

	// EFEITO REAL #2: a porta foi liberada. O daemon pode levar um instante para
	// soltar o bind após o rm, então faz um pequeno retry — mas a asserção é que
	// a porta FICA livre, não que uma função rodou.
	if !waitPortFree(port, 10*time.Second) {
		t.Fatalf("port 127.0.0.1:%d was NOT freed after reaping the orphan "+
			"(this is the exact failure the reaper must prevent)", port)
	}
}

// TestIntegrationReapOrphanRemovesRealLabeledContainer prova o EFEITO REAL de
// detecção+remoção do reaper contra um daemon docker REAL, sem depender de
// publicação de porta. Sobe um container com o label
// com.docker.compose.project começando com "acme" (o sinal PRIMÁRIO do reaper —
// exatamente o que um stack do acme deixado por um run/runner anterior carrega),
// roda o reaper com o docker REAL e afirma que o container REALMENTE sumiu.
//
// Por que existe separado do teste de porta: a publicação de porta exige a stack
// de iptables/bridge do host (indisponível em alguns WSL2), mas a LÓGICA central
// — listar, inspecionar pelo label e dar stop+rm de verdade — não exige rede
// nenhuma (`--network none`). Assim este gate roda na box de dev e na CI, provando
// que o reaper age sobre um container REAL e não só que "uma função foi chamada"
// (disciplina #13). O teste de porta acima cobre a liberação da porta ponta a
// ponta no runner self-hosted onde o bridge funciona.
func TestIntegrationReapOrphanRemovesRealLabeledContainer(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available; cannot exercise the real reap effect")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skipf("docker daemon not reachable: %v", err)
	}

	const name = "civm-reaper-label-itest"
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", name).Run()
	t.Cleanup(func() {
		c, cc := context.WithTimeout(context.Background(), 30*time.Second)
		defer cc()
		_ = exec.CommandContext(c, "docker", "rm", "-f", name).Run()
	})

	// --network none evita a stack de iptables/bridge (que falha em alguns WSL2),
	// isolando a asserção na detecção-por-label + remoção real.
	label := "com.docker.compose.project=" + civm.DefaultCIOrphanProjectPrefix + "-integration-label"
	runOut, err := exec.CommandContext(ctx, "docker", "run", "-d",
		"--name", name, "--network", "none", "--label", label,
		"busybox:latest", "sleep", "600").CombinedOutput()
	if err != nil {
		t.Skipf("could not stage labeled orphan container: %v: %s", err, runOut)
	}
	if !containerExists(ctx, name) {
		t.Fatalf("staged container %q not found after run — setup invalid", name)
	}

	opts := Options{Execute: true}
	applyDefaults(&opts)
	actions := reapOrphanCIContainers(ctx, opts)
	if len(actions) != 1 {
		t.Fatalf("expected 1 reaper action, got %+v", actions)
	}
	if actions[0].Error != "" {
		t.Fatalf("reaper must not error against a real labeled orphan: %+v", actions[0])
	}

	// EFEITO REAL: o container com label acme* foi de fato removido pelo reaper.
	if containerExists(ctx, name) {
		t.Fatalf("labeled orphan %q still exists after reap; warning=%q", name, actions[0].Warning)
	}
}

// firstFreeFixedPort devolve a primeira porta de DefaultCIFixedHostPorts que está
// livre em 127.0.0.1 agora, ou 0 se todas estiverem ocupadas.
func firstFreeFixedPort(t *testing.T) int {
	t.Helper()
	for _, p := range civm.DefaultCIFixedHostPorts {
		if portIsFree(p) {
			return p
		}
	}
	return 0
}

// portIsFree reporta se conseguimos fazer bind de 127.0.0.1:port agora (porta
// livre). Fecha o listener imediatamente — é só uma sonda.
func portIsFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// waitPortFree espera até deadline pela porta ficar livre (o daemon solta o bind
// de forma assíncrona após o rm). Retorna true assim que livre.
func waitPortFree(port int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if portIsFree(port) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// containerExists reporta se um container com este nome ainda existe (running ou
// não). Usado para provar que o reaper realmente removeu o órfão.
func containerExists(ctx context.Context, name string) bool {
	out, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "name=^"+name+"$").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}
