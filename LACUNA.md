# Lacunas entre o legado e o runtime Go

O runtime Go já cobre o núcleo do Ragfy: serviço persistente, hooks de recall e digestão, embeddings locais, extração estruturada, escopo global/projeto, recuperação híbrida e reconciliação incremental. A tabela abaixo registra as capacidades relevantes do legado e foi revisada contra a implementação atual. Itens concluídos permanecem nela para documentar o fechamento da lacuna.

| Estado | Capacidade do legado | Situação atual | Prioridade |
| --- | --- | --- | --- |
| Parcial | Inspecionar e pesquisar diretamente a base | `ragfy search` usa o mesmo retriever do hook, `status` expõe estatísticas agregadas e `sessions list|show` inspeciona sessões, fontes e registros; ainda falta uma visão geral de fontes não ligadas a sessões | Média |
| Parcial | Remover ou corrigir conhecimento | `sessions forget` remove uma sessão transacionalmente e reativa dependentes; ainda não há correção ou remoção seletiva de uma fonte ou registro | Média |
| Parcial | Avaliar o que o recall realmente injetou | `recalls list|show` preserva por prompt e sessão os resultados injetados e suas pontuações; ainda faltam veredito humano e persistência dos candidatos cortados | Alta |
| Pendente | Calibrar relevância com evidência | Os limiares atuais estão fixos no código; o legado tinha probe, feedback e autotune | Alta |
| Parcial | Ingestão realmente idempotente | Conteúdo inalterado não é extraído nem re-embeddado; uma versão alterada preserva a fonte anterior e aposenta seus registros. Ainda falta detectar arquivos que desapareceram de uma raiz ingerida e testar o ciclo completo de arquivos | Alta antes de ampliar a ingestão |
| Pendente | Recall para subagentes | O legado configurava `SubagentStart`; o Go registra somente `UserPromptSubmit` e `SessionEnd` | Média |
| Pendente | PDF, Obsidian e YouTube | Não foram portados | Média, conforme o produto desejado |
| Pendente | Agendamento e raízes configuradas | Não foram portados | Média/baixa |
| Pendente | Reescrita ou segmentação de prompts, recência e uso | Não foram portados | Experimental; portar apenas se uma avaliação provar a necessidade |
| Concluído | `remember` manual | `ragfy ingest --text`, com `--title`, `--project` e `--provider`, cobre a inclusão manual usando o pipeline normal de extração e reconciliação | Fechada |

## O que não deve ser portado agora

- A expiração automática e destrutiva por idade ou falta de uso do legado. A base ainda é pequena e a ausência de recuperação não é evidência suficiente para apagar conhecimento.
- A curadoria que removia registros antigos diretamente. A reconciliação atual preserva registros superseded para auditoria e é mais segura.
- A migração do `kb.sqlite` legado, por decisão explícita do projeto.

## Direção recomendada

A memória de sessões já é observável e reversível por `sessions list|show|forget`, e os resultados efetivamente injetados já podem ser revisados por `recalls list|show`. O próximo incremento deve fechar o ciclo de avaliação: marcar resultados como úteis ou irrelevantes e persistir também candidatos e motivos de corte. Só então esses dados devem orientar calibração de limiares ou novos heurísticos de ranking. Em paralelo, a principal pendência de integridade é detectar arquivos removidos de uma raiz ingerida.
