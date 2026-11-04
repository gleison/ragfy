# Próximos passos

## Validação da versão atual

- [x] Compilar e reinstalar o binário a partir do código atual.
- [x] Confirmar que a migração adicionou os campos de reconciliação ao banco.
- [x] Levantar todas as sessões já registradas em `digested_sessions` e localizar seus transcripts.
- [x] Reprocessar as sessões recuperáveis com o runtime atual. Foram reprocessadas 6 de 7; a sessão apagada `01a19ccd-3478-7892-b386-fc434354ba6f` não possui mais transcript e já havia armazenado zero registros.
- [x] Confirmar nos logs a extração, a seleção de candidatos e as decisões de reconciliação.
- [x] Confirmar no banco os estados `active` e `superseded`, os vencedores e os motivos.

## Controle e auditoria da memória

- [ ] Implementar `ragfy sessions list`.
- [ ] Implementar `ragfy sessions show <session-id>` com conhecimentos, origem e estado.
- [ ] Implementar `ragfy sessions forget <session-id>` de forma transacional.
- [ ] Ao esquecer uma sessão, reativar com segurança registros que tenham sido superseded por conhecimentos removidos.
- [ ] Implementar busca direta pelo CLI usando o mesmo caminho de recuperação do hook.
- [ ] Registrar os resultados efetivamente injetados por recall, associados à sessão e ao prompt.
- [ ] Criar uma forma de marcar resultados como úteis ou irrelevantes.
- [ ] Criar um comando de explicação do recall com candidatos, cortes e pontuações.

## Integridade da ingestão

- [ ] Evitar re-embedding de arquivos e chunks cujo conteúdo não mudou.
- [ ] Remover chunks antigos quando um arquivo encolher, for alterado ou desaparecer da raiz ingerida.
- [ ] Preservar no documento a data do arquivo em vez da data da execução da ingestão.
- [ ] Testar atualização, remoção e re-chunking de documentos.

## Cobertura adicional

- [ ] Avaliar e implementar recall no `SubagentStart`.
- [ ] Avaliar `ragfy remember` para inclusão manual de conhecimento.
- [ ] Avaliar conectores para PDF e Obsidian antes de YouTube.
- [ ] Avaliar configuração de raízes e atualização agendada.
- [ ] Medir prompts longos antes de portar segmentação ou reescrita de consultas.
- [ ] Medir precisão, recall e silêncio em consultas sem resposta antes de alterar limiares.

## Itens deliberadamente adiados

- [ ] Não implementar expiração automática por idade ou ausência de uso sem evidência de avaliação.
- [ ] Não implementar exclusão destrutiva automática de registros superseded.
- [ ] Não migrar o `kb.sqlite` legado.
