# IMPL — recuperação de runner ocioso com fila pendente

> Issue: `emersonbusson/civm#180`. Estado: implementação local, soak live pendente.

## Entrega

- Runner org `active/running` é correlacionado com API `online,busy=false`.
- Fleet vem de `CIVM_REAPER_REPOS`; cada job precisa das labels
  `self-hosted,civm`.
- Runs/jobs usam paginação integral; a busca oldest-first para no primeiro run
  elegível de cada repo e a assinatura usa o candidato global mais antigo sem
  expor seu ID no evento.
- Dwell default: `5 min`, configurável por `--queue-stall-dwell`.
- `idle.Check` cobre Worker, PluginHost, `_work` e Docker e é reexecutado antes
  da mutação.
- Marker é persistido por temp+rename antes do restart.
- Exatamente 1 restart por incidente; persistência vira
  `queue-stall-unresolved` crítico, sem reset por passagem de 1 hora.
- Runner `online,busy=true` comprova consumo e remove atomicamente o incidente;
  fila vazia produz o mesmo efeito.
- `health` verifica o resultado do service; `capacity` bloqueia em stall ou
  marker inválido; Prometheus expõe `civm_runner_queue_stalled`.
- O service lê `/etc/civm/run-reaper.env` sem persistir credenciais novas.

## Evidência local

- RED: assinatura online/idle/queued não produzia candidato.
- GREEN: primeiro sample arma; após dwell reinicia 1x; após 2h segue 1x e
  `unresolved`.
- Negativos verdes: idle unknown, Worker surgindo antes da mutação, labels/idade
  filtradas e fleet validada/deduplicada.
- Paginação verde: 2 páginas de runs, 2 páginas de jobs e 2 repos; um run mais
  novo não gera request depois que o candidato elegível do repo foi encontrado.
- Efeito verde: API `busy=true` + Worker limpa o marker após a tentativa.
- RED adversarial: primeira página apenas, marker stale após consumo e consulta
  de run mais novo após candidato elegível; todos reproduzidos antes do GREEN.
- `go test -race -count=1 -coverprofile=... ./...`: PASS.
- Cobertura: runner `81,9%`, health `80,6%`, capacity `85,7%`; todos os `35`
  packages `internal/**` ficaram em `>=80%`.
- `golangci-lint v2.12.1`, `go vet`, `govulncheck v1.1.4`, gitleaks em árvore
  e `262` commits, build, smokes read-only e validação de docs: PASS.
- Binário stripped: `8.069.385` bytes (`<10.485.760`).

## Pendente antes do merge/deploy

- medir ao menos 5 cold boots/reconexões para confirmar dwell de `5 min`;
- dry-run com fila real e runner saudável;
- 1 stall controlado, exatamente 1 restart e avanço da fila;
- 0 restarts com Worker/PluginHost/`_work`/Docker ativo;
- validar `health`, `capacity` e textfile Prometheus no guest.

## Rollback trigger

Reverter se qualquer caso negativo reiniciar, se o mesmo incidente reiniciar
2x ou se marker inválido permitir `accepting_jobs=true`.
