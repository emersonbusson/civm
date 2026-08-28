# SPEC — Cache trim atômico por pacote (yarn)

> SSDV3 PASSO 2. Design. Implementa o PRD.

## Visão

`TrimByAge` passa a operar sobre **unidades**, não arquivos. Uma unidade é:

- modo arquivo (default): um arquivo (Go build, golangci-lint — RF-3).
- modo `DirAtomic` (yarn): um **diretório de pacote** inteiro.

A lógica de dois passes (hard-ceiling do #124) é reaproveitada **sem mudança** —
ela já remove "entradas" do mais antigo ao mais novo; só a coleta das entradas
muda. `os.RemoveAll` já remove arquivo OU diretório, então o trim atômico cai
naturalmente.

## Mudanças

### 1. `Cap.DirAtomic bool`

Novo campo. `Caps()` o liga apenas para a família yarn:

```go
family(civm.DefaultCacheYarnMaxGB, true,  ".cache/yarn*", ".yarn/cache") // DirAtomic
family(civm.DefaultCacheGoBuildMaxGB, false, ".cache/go-build*")         // por arquivo
```

(`family` ganha o parâmetro `dirAtomic` que entra em cada `Cap` gerado.)

### 2. `collectUnits(c, opts) ([]cacheEntry, total)`

Substitui a coleta inline em `TrimByAge`:

- `DirAtomic == false`: walk recursivo, cada arquivo vira uma `cacheEntry`
  (comportamento atual).
- `DirAtomic == true`: os dirs de pacote são `filepath.Glob(<path>/*/*)`
  filtrado a diretórios — para o yarn v1 isso casa `<root>/v6/<pkg-integrity>`
  em qualquer versão de cache (`v1..vN`). Cada dir de pacote vira UMA
  `cacheEntry` com `size` = soma dos arquivos e `mtime` = mtime do arquivo mais
  novo do dir (o mais novo, para o MinProtect proteger pacote recém-escrito).
  Arquivos soltos em profundidade < 2 (`.tmp`, locks) entram como unidades de
  arquivo no mesmo conjunto, para o total bater e poderem ser trimados se
  sobrarem acima do cap.

### 3. `TrimByAge` opera sobre as unidades

`total`, `target = total - MaxBytes`, sort por mtime, e o dois-passes
(`trimPass(false)` preserva quentes, `trimPass(true)` impõe o teto) ficam
idênticos — só percorrem `units` em vez de `entries`. `RemoveAll(unit.path)`
remove o pacote inteiro (dir) atomicamente.

## Invariante garantido

Em modo `DirAtomic`, nenhum pacote yarn fica parcial: ou o dir inteiro existe,
ou foi removido por completo. O `yarn install` re-fetcha um pacote ausente
(rede) — nunca encontra um pacote pela metade (`ENOENT`).

## Validação (critérios)

- Unit: novo teste prova que, sobre um cache yarn-shape acima do cap, o trim
  remove **dirs de pacote inteiros** (oldest-first), nunca deixa arquivo órfão.
- Unit: o teste de regressão de arquivo (go-build) continua passando (RF-3).
- Efeito (#13): rebuild civmctl → deploy no guest → limpar caches corrompidos →
  re-run do `web` do #1155 → **verde**.

## Alternativas descartadas

- **Subir o cap yarn (3→6GB)**: shim — não corrige o trim parcial inseguro, só
  adia (corrompe quando o cache passa do cap maior). Viola Day-0 (sem shim).
- **Wipe do dir inteiro acima do cap**: atômico, mas re-download total por 90MB
  de excesso — economia de CI ruim.
