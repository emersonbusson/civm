package hostdisk

// SPEC: docs/specs/host-volume-reclaim-liveness/SPECv2.md §RF-1
//
// A liveness do reclaim do V: depende de a Scheduled Task civm-vhdx-autoreclaim
// nunca ficar presa num estado "fantasma": o Task Scheduler do Windows a marca em
// execucao, mas o processo real do script ja morreu. Como a task usa
// MultipleInstances=IgnoreNew, uma fantasma faz TODO tick subsequente ser
// ignorado (LastTaskResult=0x80070420) ate o ExecutionTimeLimit expirar —
// no incidente 2026-06-15 isso foi ~30h e o V: bateu no piso critico.
//
// Kahneman #13 (existencia != funcao): "a task esta Running" nao prova que o
// reclaim esta funcionando. Kahneman #16: a cura (reclaim) nao pode morrer e
// bloquear a propria ressurreicao.

// IsPhantomReclaim reporta se a task de reclaim esta num estado fantasma e deve
// ser limpa (Stop-ScheduledTask) por um watchdog externo.
//
// Os tres sinais sao colhidos pelo watchdog no host:
//   - taskRunning: (Get-ScheduledTask).State == 'Running'.
//   - scriptProcessAlive: existe um processo powershell/pwsh executando o
//     civm-vhdx-autoreclaim.ps1 (a prova viva de que o reclaim de fato roda).
//   - reclaimLockHeld: V:\civm-autoreclaim.lock esta SEGURADO (FileShare::None
//     lanca ao abrir). Um lock orfao (processo morto soltou o handle) conta como
//     nao-segurado.
//
// Fantasma == marcado em execucao, SEM processo vivo E com o lock nao-segurado.
// Exigir AMBOS (!scriptProcessAlive && !reclaimLockHeld) evita matar uma
// instancia recem-iniciada que ainda nao materializou o processo/lock na janela
// de corrida (ela tera processo vivo OU o lock segurado).
func IsPhantomReclaim(taskRunning, scriptProcessAlive, reclaimLockHeld bool) bool {
	return taskRunning && !scriptProcessAlive && !reclaimLockHeld
}
