# Primitivas PROVADAS de reclaim, extraidas do gate de 2 fases do
# civm-vhdx-autoreclaim.ps1 (issue #106), promovidas a fonte CANONICA VIVA usada
# pelo orchestrator scale-to-zero. Funcoes PURAS -> testaveis sem Hyper-V
# (Kahneman #13: o deployado e o testado). O autoreclaim legado (desabilitado)
# mantem sua copia inline ate a remocao; esta e a fonte ativa do gate.

# Day-0 defaults = DefaultHostVolumeScratchBudgetGB (internal/civm/civm.go): o
# scratch high-water p100 do Optimize-VHD e ~10GB; +1 de margem = 11. O Optimize
# e ININTERRUPTIVEL e NAO-monotonico (pode crescer o V: no meio), entao admiti-lo
# sem folga de scratch pode empurrar o V: abaixo do piso (PausedCritical). O
# HardFloor=1 e o piso absoluto que nunca cruzamos.
$script:ReclaimHardFloorGB = 1
$script:ReclaimScratchBudgetGB = 11
# Cooldown entre panics: o Optimize recupera ~25GB e a taxa de crescimento medida
# (~22GB/h) da ~1h entre panics naturais. 15min so barra o LOOP apertado (panic a
# cada tick de 2min quando o guest reenche rapido), sem atrapalhar o ritmo real.
$script:PanicCooldownMinutes = 15
# Recover-detection: se o Optimize recuperou menos que isto, ele NAO ajudou —
# alerta em vez de fingir sucesso (#13: existe != funciona).
$script:MinRecoverGB = 3

# Test-OptimizeSlack: o gate AUTORITATIVO pos-Off (espelha autoreclaim.ps1:494).
# So admite o Optimize-VHD se a folga MEDIDA pos-Stop-VM cobre o scratch budget;
# senao o Optimize ininterruptivel poderia empurrar o V: abaixo do HardFloor.
# CRITICO: VFreeAfterOffGB DEVE ser medido DEPOIS do VM chegar a Off — o VMRS
# (~8GB de saved-state) so e liberado entao; medir antes SUBESTIMA (foi o que
# travou a espiral a 6.6GB no #106).
function Test-OptimizeSlack {
    param(
        [Parameter(Mandatory)][double]$VFreeAfterOffGB,
        [int]$HardFloorGB = $script:ReclaimHardFloorGB,
        [int]$ScratchBudgetGB = $script:ReclaimScratchBudgetGB
    )
    return (($VFreeAfterOffGB - $HardFloorGB) -ge $ScratchBudgetGB)
}

# Test-ReclaimStuck: reclaim_no_progress so e ERRO REAL quando o Optimize-VHD
# nao recuperou o minimo E o V: continua ABAIXO do piso de admissao (disco preso
# que o compact nao resolve -> precisa de humano). Se o V: ja esta >= piso,
# recuperar pouco e ESPERADO: o VHDX ja esta compacto (o footprint do guest e
# estavel ~52GB de arquivos genuinos), nao ha o que devolver. Sem este gate o
# ERROR disparava todo boundary num disco saudavel (v_free ~67) — falso-vermelho
# perpetuo (Kahneman #13: um sinal que nao bate com o estado real e bug).
function Test-ReclaimStuck {
    param(
        [Parameter(Mandatory)][int]$RecoveredGB,
        [Parameter(Mandatory)][int]$VFreeAfterGB,
        [int]$MinRecoverGB = $script:MinRecoverGB,
        [int]$AdmitFloorGB = 55
    )
    return (($RecoveredGB -lt $MinRecoverGB) -and ($VFreeAfterGB -lt $AdmitFloorGB))
}

# Test-ReclaimCooldown: $true se PODE reclamar agora (fora do cooldown). Barra o
# panic re-disparando a cada tick quando o disco reenche rapido (retry cego,
# Kahneman #14 no doc civm). Sem LastReclaimUtc (1o panic) -> pode. A medida de
# tempo e VIVA: o caller passa o NowUtc do instante da decisao, nunca um cache.
# Data ilegivel -> nao trava o reclaim (fail-safe: na duvida, deixa proteger).
function Test-ReclaimCooldown {
    param(
        [string]$LastReclaimUtc,
        [Parameter(Mandatory)][string]$NowUtc,
        [int]$CooldownMinutes = $script:PanicCooldownMinutes
    )
    if ([string]::IsNullOrWhiteSpace($LastReclaimUtc)) { return $true }
    try {
        $last = [datetime]::Parse($LastReclaimUtc).ToUniversalTime()
        $now = [datetime]::Parse($NowUtc).ToUniversalTime()
        return (($now - $last).TotalMinutes -ge $CooldownMinutes)
    }
    catch { return $true }
}
