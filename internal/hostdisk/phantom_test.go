package hostdisk

import "testing"

// SPEC: docs/specs/host-volume-reclaim-liveness/SPECv2.md §RF-1 (validacao 1).
func TestIsPhantomReclaim(t *testing.T) {
	tests := []struct {
		name               string
		taskRunning        bool
		scriptProcessAlive bool
		reclaimLockHeld    bool
		want               bool
	}{
		{
			// O incidente 2026-06-15: task presa em Running, processo morto,
			// lock orfao. ESTE e o fantasma que bloqueou ~30h.
			name: "running + dead process + orphan lock => phantom",
			taskRunning: true, scriptProcessAlive: false, reclaimLockHeld: false,
			want: true,
		},
		{
			// Par #13: um reclaim LEGITIMO em execucao tem processo vivo. NUNCA
			// pode ser limpo (mataria o reclaim de verdade).
			name: "running + live process => NOT phantom (real reclaim)",
			taskRunning: true, scriptProcessAlive: true, reclaimLockHeld: true,
			want: false,
		},
		{
			// Janela de corrida: recem-disparado, processo ainda subindo, mas o
			// lock ja foi segurado. Nao e fantasma.
			name: "running + held lock (process not yet scanned) => NOT phantom",
			taskRunning: true, scriptProcessAlive: false, reclaimLockHeld: true,
			want: false,
		},
		{
			// Recem-disparado: processo ja vivo, lock ainda nao materializado.
			name: "running + live process + no lock yet => NOT phantom",
			taskRunning: true, scriptProcessAlive: true, reclaimLockHeld: false,
			want: false,
		},
		{
			// Task ociosa (Ready). Nada a limpar.
			name: "not running => NOT phantom",
			taskRunning: false, scriptProcessAlive: false, reclaimLockHeld: false,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsPhantomReclaim(tt.taskRunning, tt.scriptProcessAlive, tt.reclaimLockHeld)
			if got != tt.want {
				t.Errorf("IsPhantomReclaim(running=%v, proc=%v, lock=%v) = %v, want %v",
					tt.taskRunning, tt.scriptProcessAlive, tt.reclaimLockHeld, got, tt.want)
			}
		})
	}
}
