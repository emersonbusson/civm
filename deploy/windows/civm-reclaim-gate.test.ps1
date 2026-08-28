# Teste das primitivas puras de reclaim (Test-OptimizeSlack + Test-ReclaimCooldown).
# Sem Hyper-V -> testavel em qualquer pwsh (Kahneman #13: deployado == testado).
. "$PSScriptRoot\civm-reclaim-gate.ps1"
$pass = 0; $fail = 0
function Check($name, $got, $exp) {
    if ($got -eq $exp) { $script:pass++; "PASS  $name" }
    else { $script:fail++; "FAIL  $name (esperado=$exp got=$got)" }
}

# Test-OptimizeSlack: admite o Optimize so se (VFreeAfterOff - HardFloor 1) >=
# ScratchBudget 11 — senao o Optimize ininterruptivel poderia estourar o V:.
Check 'slack 12GB pos-Off -> admite (12-1=11 >= 11, boundary)' (Test-OptimizeSlack -VFreeAfterOffGB 12) $true
Check 'slack 25GB pos-Off -> admite (folga sobrando)' (Test-OptimizeSlack -VFreeAfterOffGB 25) $true
Check 'slack 11GB pos-Off -> PULA (10 < 11, nao estoura o V:)' (Test-OptimizeSlack -VFreeAfterOffGB 11) $false
Check 'slack 5GB pos-Off -> PULA (critico)' (Test-OptimizeSlack -VFreeAfterOffGB 5) $false
Check 'slack budget custom 30: 25GB -> PULA (24 < 30)' (Test-OptimizeSlack -VFreeAfterOffGB 25 -ScratchBudgetGB 30) $false

# Test-ReclaimCooldown: $true se PODE reclamar (fora do cooldown de 15min). Barra
# o loop de panic re-disparando a cada tick. Data ilegivel -> fail-safe (nao trava).
$now = '2026-06-17T22:00:00.0000000Z'
Check 'cooldown sem lastUtc (1o panic) -> pode' (Test-ReclaimCooldown -LastReclaimUtc '' -NowUtc $now) $true
Check 'cooldown 20min depois -> pode (>15)' (Test-ReclaimCooldown -LastReclaimUtc '2026-06-17T21:40:00Z' -NowUtc $now) $true
Check 'cooldown 5min depois -> NAO pode (loop barrado)' (Test-ReclaimCooldown -LastReclaimUtc '2026-06-17T21:55:00Z' -NowUtc $now) $false
Check 'cooldown exatamente 15min -> pode (boundary >=)' (Test-ReclaimCooldown -LastReclaimUtc '2026-06-17T21:45:00Z' -NowUtc $now) $true
Check 'cooldown data ilegivel -> pode (fail-safe)' (Test-ReclaimCooldown -LastReclaimUtc 'lixo' -NowUtc $now) $true

# Test-ReclaimStuck: reclaim_no_progress so e ERRO quando recuperou < min (3) E
# o V: continua < piso (55). V: >= piso -> recuperar pouco e esperado (compacto).
Check 'stuck: recuperou 1, V=40 (<55) -> ERRO real (preso)' (Test-ReclaimStuck -RecoveredGB 1 -VFreeAfterGB 40) $true
Check 'stuck: recuperou 2, V=67 (>=55) -> NAO erro (VHDX ja compacto)' (Test-ReclaimStuck -RecoveredGB 2 -VFreeAfterGB 67) $false
Check 'stuck: recuperou 10, V=40 (<55) -> NAO erro (recuperou bem)' (Test-ReclaimStuck -RecoveredGB 10 -VFreeAfterGB 40) $false
Check 'stuck: recuperou 0, V=55 (==piso) -> NAO erro (boundary >=)' (Test-ReclaimStuck -RecoveredGB 0 -VFreeAfterGB 55) $false
Check 'stuck: recuperou 2, V=54 (<55) -> ERRO (preso na borda)' (Test-ReclaimStuck -RecoveredGB 2 -VFreeAfterGB 54) $true

''; "RESULTADO: $pass PASS / $fail FAIL"
if ($fail -gt 0) { exit 1 }
