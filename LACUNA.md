# Lacunas entre o legado e o runtime Go

O runtime Go já cobre o núcleo do Ragfy: serviço persistente, hooks de recall e digestão, embeddings locais, extração estruturada, escopo global/projeto, recuperação híbrida e reconciliação incremental. A tabela abaixo registra as capacidades relevantes do legado que ainda não foram cobertas ou que foram cobertas apenas parcialmente.

| Capacidade do legado | Situação atual | Prioridade |
| --- | --- | --- |
| Inspecionar e pesquisar diretamente a base | O legado tinha `search`, `stats` e `ask`; o CLI atual só oferece serviço, ingestão e status | Alta |
| Remover ou corrigir conhecimento | Não há comando público para esquecer uma sessão ou registro | Alta |
| Avaliar o que o recall realmente injetou | O legado registrava prompt, resultados, ranking e veredito humano; hoje há apenas logs técnicos | Alta |
| Calibrar relevância com evidência | Os limiares atuais estão fixos no código; o legado tinha probe, feedback e autotune | Alta |
| Ingestão realmente idempotente | O Go re-embeda todos os arquivos e não remove chunks antigos quando um arquivo encolhe ou desaparece | Alta antes de ampliar a ingestão |
| Recall para subagentes | O legado configurava `SubagentStart`; o Go registra somente `UserPromptSubmit` e `SessionEnd` | Média |
| PDF, Obsidian e YouTube | Não foram portados | Média, conforme o produto desejado |
| Agendamento e raízes configuradas | Não foram portados | Média/baixa |
| Reescrita ou segmentação de prompts, recência e uso | Não foram portados | Experimental; portar apenas se uma avaliação provar a necessidade |
| `remember` manual | Não foi portado | Útil, mas depois de existir remoção e auditoria |

## O que não deve ser portado agora

- A expiração automática e destrutiva por idade ou falta de uso do legado. A base ainda é pequena e a ausência de recuperação não é evidência suficiente para apagar conhecimento.
- A curadoria que removia registros antigos diretamente. A reconciliação atual preserva registros superseded para auditoria e é mais segura.
- A migração do `kb.sqlite` legado, por decisão explícita do projeto.

## Direção recomendada

O próximo incremento deve tornar a memória observável e reversível. A primeira fatia é um controle de sessões com listagem, inspeção e esquecimento transacional. Depois disso, o Ragfy deve registrar e permitir revisar os conhecimentos efetivamente injetados pelo recall antes de adotar novos heurísticos de ranking ou limpeza automática.
