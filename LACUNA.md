# Lacunas entre o legado e o runtime Go

O runtime Go já cobre o núcleo do Ragfy: serviço persistente, hooks de recall e digestão, embeddings locais, extração estruturada, escopo global/projeto, recuperação híbrida e reconciliação incremental. A tabela abaixo registra as capacidades relevantes do legado e foi revisada contra a implementação atual. Itens concluídos permanecem nela para documentar o fechamento da lacuna.

| Estado | Capacidade do legado | Situação atual | Prioridade |
| --- | --- | --- | --- |
| Parcial | Inspecionar e pesquisar diretamente a base | `ragfy search` usa o mesmo retriever do hook e `status` expõe estatísticas agregadas; ainda faltam listagem e inspeção de fontes, sessões e registros | Alta |
| Pendente | Remover ou corrigir conhecimento | Não há comando público para esquecer uma sessão, fonte ou registro | Alta |
| Parcial | Avaliar o que o recall realmente injetou | Os logs mostram candidatos, cortes, componentes da pontuação e seleção; ainda não há histórico associado ao prompt e à sessão, nem veredito humano | Alta |
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

O próximo incremento deve tornar a memória observável e reversível. A primeira fatia é somente leitura: `ragfy sessions list` e `ragfy sessions show <session-id>`, apoiados nas fontes e registros que agora são preservados. A fatia seguinte é `sessions forget`, com remoção transacional e reativação segura dos registros que dependiam dos conhecimentos removidos. Depois disso, o Ragfy deve persistir e permitir revisar os resultados efetivamente injetados pelo recall antes de adotar novos heurísticos de ranking ou limpeza automática.
