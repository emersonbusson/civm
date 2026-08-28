---
name: coding-style
description: Higiene de código Go para civmctl e packages internal — comunicação intencional e limites cognitivos.
paths:
  - "**/*.go"
---

# Coding style rules

Código e testes são a fonte de verdade do design. A meta é reduzir **ruído**
(variância, inconsistência) e a carga do **Sistema 2** onde o **Sistema 1**
(leitura intuitiva) deveria bastar — o Sistema 1 escaneia, não lê. Código não
pode só "funcionar"; tem que **dizer a coisa certa**. Decisão sob incerteza
segue [../disciplines/KAHNEMAN-DISCIPLINES.md](../disciplines/KAHNEMAN-DISCIPLINES.md).

## Comunicação intencional

- Nomes revelam a intenção sem ambiguidade e contam uma história contínua.
- Booleano afirmado, não negado: `isPending`, não `isNotReady`.
- Erro embrulhado com contexto: `fmt.Errorf("ler config: %w", err)`. Nunca engolir erro em silêncio.

## Limites cognitivos

- ≤ 7 parâmetros por função; complexidade cognitiva alvo ≤ 15.
- Função < 50 linhas; arquivo < 800 linhas — extrair quando passar.
- Laço interno complexo vira **helper nomeado e testável**: o laço deixa de ser bloco anônimo e passa a ser um verbo do domínio.

## Guard clauses

- Sem deep nesting (> 4 níveis). Trate a falha no topo com retorno antecipado; o "caminho feliz" fica alinhado à esquerda. Troque `else` por guard clause.

## Modelo rico (Go)

- Lógica de domínio pertinente a um struct vive no **receiver method** (`func (s *S) Acao()`), não espalhada em helpers procedurais anêmicos na camada de chamada.
- Aceitar interfaces, retornar structs; interfaces pequenas (1–3 métodos) definidas onde são usadas.
- Injeção simples (`RunFn`, `ReadFileFn`, `NowFn`) em vez de mock pesado — é o padrão dos packages `internal/**` e o que torna a lógica hermética em teste.

## Tooling "Puro Go"

- Automação em Go (subcomando do `civmctl`, `go run`), não em shell/`.mjs` solto — evita troca de contexto mental Go↔Bash/Node.
- A camada host Hyper-V é PowerShell por necessidade: mantê-la fina e guardada por teste Go (ex.: o lint de `[math]::Max(0,…)` em `internal/hostdisk`).

## Testes como documentação

- Assertions rigorosas; nome e organização do teste documentam o design e o tratamento de erro (positivo **e** negativo). Detalhe em [testing.md](testing.md).

## Execução (micro-slicing)

- Uma fatia ortogonal por vez → commit atômico → valida e testa → próxima. O Sistema 1 é otimista demais para refator em lote.
- Ruído estrutural (refator pesado, quebra de contrato) passa pela esteira [../disciplines/SSDV3-PROMPTS.md](../disciplines/SSDV3-PROMPTS.md) antes de virar código.

## Don't

- ❌ Nome críptico ou booleano negado (`isNotReady`).
- ❌ Função > 50 linhas / > 7 params / nesting > 4 níveis.
- ❌ `else` quando uma guard clause resolve.
- ❌ Helper anêmico procedural quando a lógica pertence ao receiver do struct.
- ❌ Shell/`.mjs` solto quando uma função Go testável resolve.
- ❌ Erro engolido sem `%w` nem log de contexto.
- ❌ Refatorar vários padrões/dezenas de arquivos num commit só.

## Auditoria

Para uma auditoria sistemática de ruído arquitetural sobre estas regras, use o
superprompt [`../disciplines/SUPERPROMPT.md`](../disciplines/SUPERPROMPT.md)
(Kahneman + DDD): diagnóstico de ruído → código de higiene → esteira SSDV3 se
o ruído for estrutural.
