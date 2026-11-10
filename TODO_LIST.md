# Próximos passos

## Validação da versão atual

- [x] Compilar e reinstalar o binário a partir do código atual.
- [x] Confirmar que a migração adicionou os campos de reconciliação ao banco.
- [x] Levantar todas as sessões já registradas em `digested_sessions` e localizar seus transcripts.
- [x] Reprocessar as sessões recuperáveis com o runtime atual. Foram reprocessadas 6 de 7; a sessão apagada `01a19ccd-3478-7892-b386-fc434354ba6f` não possui mais transcript e já havia armazenado zero registros.
- [x] Confirmar nos logs a extração, a seleção de candidatos e as decisões de reconciliação.
- [x] Confirmar no banco os estados `active` e `superseded`, os vencedores e os motivos.

## Controle e auditoria da memória

- [x] Implementar `ragfy sessions list`.
- [x] Implementar `ragfy sessions show <session-id>` com conhecimentos, origem e estado.
- [x] Implementar `ragfy sessions forget <session-id>` de forma transacional.
- [x] Ao esquecer uma sessão, reativar com segurança registros que tenham sido superseded por conhecimentos removidos.
- [x] Implementar busca direta pelo CLI usando o mesmo caminho de recuperação do hook (`ragfy search`).
- [x] Registrar os resultados efetivamente injetados por recall, associados à sessão e ao prompt.
- [x] Permitir revisar o histórico e as pontuações com `ragfy recalls list|show`.
- [ ] Criar uma forma de marcar resultados como úteis ou irrelevantes.
- [ ] Criar um comando de explicação do recall com candidatos, cortes e pontuações.

## Integridade da ingestão

- [x] Evitar extração e re-embedding de fontes cujo conteúdo não mudou.
- [x] Preservar versões de uma fonte alterada e aposentar os conhecimentos derivados da versão anterior, inclusive quando o arquivo encolher.
- [x] Preservar no documento a data do arquivo em vez da data da execução da ingestão.
- [x] Testar idempotência, versionamento da fonte e aposentadoria transacional dos conhecimentos anteriores.

## Cobertura adicional

- [ ] Carregar um contexto-base limitado do projeto no `SessionStart`, mantendo o recall orientado ao prompt em `UserPromptSubmit`.

## Itens deliberadamente adiados

- [ ] Não implementar expiração automática por idade ou ausência de uso sem evidência de avaliação.
- [ ] Não implementar exclusão destrutiva automática de registros superseded.
- [ ] Não migrar o `kb.sqlite` legado.
- [ ] Não implementar raízes gerenciadas ou atualização agendada na primeira versão; a ingestão de arquivos permanece sob demanda.
