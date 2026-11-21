# Documentação

O inglês é o idioma canônico da documentação pública e operacional. Traduções
para português do Brasil usam o sufixo `.pt-BR.md` e apontam para sua fonte em
inglês. O [README raiz](../README.md) apresenta o projeto; o
[README em português](../README.pt-BR.md) é a tradução secundária.

## Comece pelo civm

- [Visão geral e referência de comandos](../README.pt-BR.md)
- [Como contribuir](../CONTRIBUTING.md)
- [Política de segurança](../SECURITY.md)
- [Templates de workflow](../templates/)

## Operação

Os guias operacionais existentes seguem em português e são os próximos
candidatos à tradução. Guias novos ou revisados de forma substantiva devem ser
escritos primeiro em inglês. A versão atual publicada em `main` ainda não
tem versões em inglês desses guias disponíveis.

- [Provisionamento de runner multi-projeto](../runbooks/MULTI-PROJECT-RUNNER.md)
- [Serialização do runner](../runbooks/RUNNER-SERIALIZATION.md)
- [Adoção de runner de organização](../runbooks/ORG-RUNNER-ADOPTION.md)
- [Configuração do orquestrador Windows](../runbooks/HOST-ORCHESTRATOR-SETUP.md)
- [Dispatcher JIT confiável](../runbooks/TRUSTED-JIT-DISPATCHER.md)
- [Referência de hardware (PT-BR; caminho original)](HARDWARE.md)

## Arquitetura e especificações

A árvore `docs/specs/` contém registros de design e especificações históricas
de implementação. Elas mantêm o idioma original até uma revisão substantiva;
para o comportamento atual, consulte o README e os guias operacionais.
