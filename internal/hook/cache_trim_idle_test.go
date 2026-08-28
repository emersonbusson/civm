package hook

import (
	"context"
	"errors"
	"testing"

	"github.com/emersonbusson/civm/internal/idle"
)

// TestCacheTrimIsIdle prova o gate que impede o hook de trimar o cache
// compartilhado enquanto OUTRO runner tem build ativo (a corrida que dava
// ENOENT em gates/yarn-audit). O caso "sibling acme-org" guarda o trap de
// prefixo: o runner dir do próprio runner termina com separador, então
// actions-runner-acme/ não casa dentro de actions-runner-acme-org/.
func TestCacheTrimIsIdle(t *testing.T) {
	ownDirs := []string{"/home/emdev/actions-runner-acme/"}
	act := func(cmds ...string) func(context.Context) ([]idle.Activity, error) {
		return func(context.Context) ([]idle.Activity, error) {
			as := make([]idle.Activity, len(cmds))
			for i, c := range cmds {
				as[i] = idle.Activity{PID: 100 + i, Command: c}
			}
			return as, nil
		}
	}
	tests := []struct {
		name    string
		ownDirs []string
		fn      func(context.Context) ([]idle.Activity, error)
		want    bool
	}{
		{"so o proprio Worker -> idle", ownDirs, act("/home/emdev/actions-runner-acme/bin/Runner.Worker run"), true},
		{"proprio build + Worker -> idle", ownDirs, act(
			"/home/emdev/actions-runner-acme/bin/Runner.Worker run",
			"yarn /home/emdev/actions-runner-acme/_work/acme/app",
		), true},
		{"sibling acme-org build ativo -> NAO idle (trap de prefixo)", ownDirs, act(
			"/home/emdev/actions-runner-acme/bin/Runner.Worker run",
			"node /home/emdev/actions-runner-acme-org/_work/acme/app next build",
		), false},
		{"sem atividade -> idle", ownDirs, act(), true},
		{"ownDirs vazio (env degradado) -> fail-safe NAO idle", nil, act(), false},
		{"probe error -> fail-safe NAO idle", ownDirs, func(context.Context) ([]idle.Activity, error) {
			return nil, errors.New("ps fail")
		}, false},
		{"ActivityFn nil -> fail-safe NAO idle", ownDirs, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := Options{ActivityFn: tt.fn}
			if got := cacheTrimIsIdle(context.Background(), opts, tt.ownDirs); got != tt.want {
				t.Errorf("cacheTrimIsIdle = %v, want %v", got, tt.want)
			}
		})
	}
}
