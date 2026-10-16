# Arquitetura

## Organização e ciclo de vida

O Uber Fx monta configuração, PostgreSQL, SQS, autenticação, serviço financeiro, workers de referências, inbox/outbox e HTTP por construtores. O domínio em `internal/domain` não depende de Fx, HTTP, SQS ou pgx. `internal/wagering` coordena persistência e casos de uso; `internal/httpapi` aplica identidade, limites de entrada e contratos HTTP.

A configuração é lida uma vez e validada antes da inicialização. As URLs do PostgreSQL e do emissor OIDC são obrigatórias. As chamadas externas usam contextos com timeout, e o servidor HTTP limita leitura de cabeçalhos, leitura, escrita e tempo ocioso.

A API só começa a aceitar conexões depois das verificações de PostgreSQL, SQS e descoberta OIDC. No SIGTERM, o Fx para novas requisições e aguarda as que estão em andamento até o prazo configurado. Se o prazo acabar, fecha as conexões HTTP. Os hooks cancelam e aguardam os workers antes de fechar SQS e PostgreSQL; a camada OIDC também libera suas conexões ociosas. Se a inicialização falhar, os recursos parcialmente abertos são fechados e o Fx desfaz os hooks concluídos.

Os logs são JSON, com `slog` e o adaptador do Fx. Não incluem strings de conexão, credenciais, tokens ou payloads financeiros. As respostas públicas de saúde mostram apenas o estado das dependências.

## Banco e migrations

O acesso ao PostgreSQL usa `pgx/v5`, pool limitado e SQL explícito. As migrations usam golang-migrate com o driver pgx v5 e locks consultivos do PostgreSQL. Conexão, aquisição do lock e execução de instruções têm limite de 10 segundos.

As migrations rodam em um serviço separado do Compose, antes da API. Isso evita que cada instância tente migrar o banco. O comando de avanço aplica as pendentes; o de reversão remove uma versão. A primeira migration cria o schema `wagering`, e a reversão usa `DROP SCHEMA ... RESTRICT`.

O executor fixa o `search_path` em `public` e mantém o histórico em `public.schema_migrations`. Com o padrão `$user`, criar um schema com o nome do usuário mudava o local em que o driver procurava o histórico. Todas as consultas financeiras usam o schema `wagering` explicitamente.

O teste de migrations usa um banco temporário e executa avanço duas vezes, reversão e avanço novamente.

## Autenticação e autorização

O Keycloak cuida da emissão de tokens. A aplicação usa OAuth/OIDC e não armazena senhas de usuários. O ambiente local importa dois clientes de provedor e um interno, todos com credenciais de cliente. Claims fixas por cliente definem o provedor; o mapeador de audiência inclui `wagering-api`.

O middleware aceita JWTs assinados com RS256 e verifica emissor, audiência, expiração, sujeito e `service_role`. Um provedor precisa ter `provider_id` preenchido; o serviço interno não pode ter esse campo. As funções de autorização verificam a correspondência do provedor ou exigem a identidade interna.

O emissor local é `http://localhost:8081/realms/jungle`. Dentro do Compose, um transporte HTTP do cliente OIDC troca apenas a origem da requisição por `http://keycloak:8080`. Assim, descoberta e JWKS funcionam na rede dos containers sem desabilitar a validação do emissor. Fora do Docker, o backchannel fica vazio. As consultas de JWKS também têm timeout.

Os tokens duram 60 segundos para permitir testes de expiração real. As rotas de carteira exigem serviço interno. A entrada financeira exige um provedor autenticado e compara seu `provider_id` com o corpo antes de chamar o caso de uso, incluindo replays. As duas consultas de transações filtram o provedor no SQL, inclusive ao buscar pelo ID interno.

## Dinheiro e entidades

`Money` guarda moeda e unidades mínimas em campos privados, sem ponto flutuante. O limite é o de `int64`: de -92233720368547758.08 a 92233720368547758.07. Parsing, soma, subtração e negação verificam overflow. A escala externa é exatamente duas casas; expoentes, sinais positivos, espaços, valores vazios e escala diferente são inválidos. O parsing interno aceita negativos para diferenças; o decoder JSON de entrada rejeita qualquer sinal negativo, inclusive `-0.00`. A serialização usa strings decimais.

BRL é a única moeda das carteiras e operações. O value object também reconhece USD e EUR para operações internas e testes de incompatibilidade, sempre com escala de duas casas. Códigos desconhecidos são rejeitados. Zeros à esquerda são aceitos e normalizados; UUIDs são normalizados para minúsculas. Nenhuma outra string de negócio é aparada ou alterada.

Wallet e Transaction têm estado privado, validação na criação/reidratação e métodos explícitos de transição. Reidratar não movimenta saldo nem emite eventos. Uma movimentação positiva incrementa a versão da carteira uma vez; LOSS e rejeições não mudam saldo ou versão. O ledger valida direção e a equação do saldo no domínio e no banco. OPENING positivo nasce processado, com versão da carteira 1; abertura zero não cria operação financeira.

## Transação SQL e concorrência

O serviço abre uma transação pgx em READ COMMITTED e passa o mesmo `pgx.Tx` às gravações de operação, carteira, ledger e outbox. Não há conexão separada para um desses registros. A abertura inclui carteira, OPENING e seus eventos no mesmo commit. Uma falha, inclusive ao gravar evento, desfaz tudo.

Cada operação nova obtém `SELECT ... FOR UPDATE` na carteira antes de avaliar saldo ou referências. Esse lock é do PostgreSQL, por carteira, compartilhado por todas as instâncias. Não há mutex ou lock global. Os replays já confirmados podem ler o resultado persistido sem bloquear escritores. Constraints únicas arbitram identidades concorrentes; após uma colisão no INSERT, uma nova consulta dentro da mesma transação observa a identidade confirmada e decide replay/conflito.

O schema impõe unicidade de jogador/moeda, provedor/ID externo, provedor/chave, OPENING por carteira e carteira/transação no ledger. FKs compostas mantêm carteira, jogador, moeda e valor coerentes. CHECKs impedem saldo negativo e valores financeiros inválidos. Triggers protegem identidade/versão da carteira, transações terminais, o payload dos eventos e o ledger contra UPDATE, DELETE e TRUNCATE. O append valida o saldo anterior contra o lançamento anterior e o saldo posterior/versão contra a carteira bloqueada. Constraints diferidas verificam a soma do ledger e a correspondência entre operação processada e lançamento antes de confirmar o commit.

As verificações do saldo por soma têm custo proporcional ao histórico da carteira e favorecem a auditabilidade deste desafio. O usuário local do banco é administrativo: as proteções valem para SQL comum, mas não pretendem impedir um administrador de desabilitar triggers.

## Idempotência e estados

O hash é SHA-256 do JSON gerado por mapas Go, com chaves ordenadas lexicograficamente, incluindo o mapa interno de Money. Campos: providerId, externalTransactionId, playerId, walletId, roundId, gameId, kind, money (amount/currency) e referenceExternalTransactionId. Referência ausente é string vazia no hash. UUIDs são minúsculos e Money usa sua forma decimal normalizada; chave de idempotência e metadados de transporte ficam fora do hash. O consumidor SQS usa esse mesmo contrato.

Chave, identidade externa e hash precisam coincidir para replay. Reutilizar chave com conteúdo diferente, reutilizar identidade externa com outra chave, ou cruzar duas identidades já existentes resulta em conflito. O resultado terminal armazena saldo e failureCode; o replay devolve esses dados originais, independentemente do saldo atual.

Transições: `PENDING → PROCESSED | REJECTED | FAILED | PENDING_REFERENCE`; `PENDING_REFERENCE → PENDING_REFERENCE | PROCESSED | REJECTED | FAILED`. Esperar referência exige uma referência externa. Estados terminais são imutáveis. OPENING interno já é criado em PROCESSED. Operações independentes não confirmam PENDING intermediário: terminam no mesmo commit ou sofrem rollback. O banco rejeita um PENDING confirmado, pois não há worker genérico para retomá-lo.

Rejeições de domínio são persistidas como REJECTED com evento. Erros de banco, cancelamento e indisponibilidade são transitórios: rollback e HTTP 503, com retry pela mesma chave. FAILED é um estado previsto e validado pelo domínio/schema, mas não é emitido por este caminho HTTP, que não transforma uma falha de infraestrutura não confirmada em resultado terminal.

## Referências e reversões

Referências são resolvidas por `(providerId, externalTransactionId)`. Provedor, jogador, carteira, moeda, rodada e jogo devem coincidir. REFUND aceita apenas BET processada e devolve seu valor integral. ROLLBACK aceita BET, WIN ou REFUND processada e aplica o movimento contrário, com valor integral. WIN pode referenciar BET da mesma rodada, com valor próprio.

Uma referência aceita no máximo uma reversão direta bem-sucedida, seja REFUND ou ROLLBACK; um índice único parcial reforça isso no banco. Portanto, refund seguido de rollback direto da mesma BET é rejeitado como ALREADY_REVERSED. É permitido fazer rollback do REFUND: ele debita o crédito devolvido, restaurando o efeito líquido da aposta. Mesmo assim, a BET original continua consumida para novas reversões diretas. ROLLBACK de ROLLBACK não é permitido. Reversões rejeitadas não consomem a referência.

Referência ausente ou ainda pendente mantém PENDING_REFERENCE com um único evento inicial. Uma referência terminal sem sucesso causa REFERENCE_NOT_PROCESSED. Referência incompatível, de tipo errado, de valor diferente ou autorreferência causa rejeição definitiva. Ciclos entre pendências não movimentam dinheiro e terminam pela política de expiração.

O worker consulta carteiras distintas com operações vencidas, bloqueia carteira e depois operação com SKIP LOCKED e conclui cada tentativa em uma única transação, sem I/O externo. Não usa lease persistida: os locks transacionais bastam porque não há publicação ou outro trabalho fora da transação. Crash ou cancelamento faz rollback e libera imediatamente a pendência. Múltiplas instâncias disputam sem aplicar o mesmo efeito duas vezes.

O polling ocorre a cada 500 ms, com até 32 operações por ciclo e timeout de 5 segundos por tentativa. O primeiro retry vence após 1 segundo; ausências seguintes usam 2, 4, 8... segundos, limitados a 256 segundos. Após 12 tentativas do worker sem resolução, ou 24 horas desde o aceite, a operação termina REJECTED com REFERENCE_NOT_FOUND e evento. O TTL é verificado antes de resolver a referência, mesmo que ela tenha chegado depois do prazo. Falha transitória no banco não confirma incremento da tentativa. Reenvios HTTP não antecipam a agenda.

| failureCode | Regra e ação |
| --- | --- |
| INSUFFICIENT_FUNDS | BET sem saldo suficiente |
| REVERSAL_INSUFFICIENT_FUNDS | ROLLBACK de crédito sem saldo suficiente para desfazer |
| BALANCE_OVERFLOW | Crédito excederia o limite exato de saldo/versão |
| INVALID_REFERENCE | Autorreferência ou referência inválida |
| REFERENCE_MISMATCH | Identidades, moeda, rodada ou jogo diferentes |
| INVALID_REFERENCE_KIND | Tipo referenciado não permitido |
| REFERENCE_AMOUNT_MISMATCH | Reversão não corresponde ao valor integral |
| REFERENCE_NOT_PROCESSED | Referência terminou rejeitada ou falhou |
| ALREADY_REVERSED | Efeito já consumido por uma reversão direta |
| REFERENCE_NOT_FOUND | Dependência não resolvida antes do TTL/limite de tentativas |

Todos esses resultados são definitivos para aquela operação: replay não tenta novamente. Entradas estruturalmente inválidas retornam 400 sem operação persistida; podem ser corrigidas antes do primeiro aceite. Após aceite, uma operação diferente exige nova identidade/chave e continua sujeita às regras financeiras.

## Outbox

Construtores concretos fixam tipo e versão 1 dos eventos WagerTransactionProcessed, WagerTransactionRejected, WagerTransactionPendingReference e WalletBalanceChanged. Os envelopes usam IDs UUID estáveis, aggregateId, correlationId igual ao ID da operação, UTC/RFC 3339 e Money em strings; causationId é opcional e omitido. Eventos de abertura omitem metadados externos inaplicáveis. WalletBalanceChanged inclui direção, valor, saldo anterior/posterior e versão da carteira. LOSS emite apenas o evento de processamento.

O JSON é serializado uma vez e gravado como snapshot imutável na transação financeira. O publisher publica somente linhas confirmadas, com claims atômicos SKIP LOCKED, leases de 15 segundos e confirmação condicionada ao token. Publicação fora da transação evita manter locks financeiros durante I/O. Reenvios preservam eventId e snapshot; consumidores deduplicam por eventId. A inbox é gravada e concluída no mesmo pgx.Tx financeiro, inclusive para replays e pendências.

## Filas e saúde

O LocalStack cria as filas FIFO de entrada, DLQ e saída, sem deduplicação por conteúdo. A entrada tem visibilidade de 30 segundos e envio à DLQ após cinco recebimentos. As URLs usam o endpoint do chamador, permitindo acesso pela máquina local e pelos containers. O SDK da AWS usa a cadeia padrão de credenciais e limita as novas tentativas.

A liveness verifica o processo HTTP. A readiness consulta o PostgreSQL e as três filas dentro de um prazo compartilhado. A descoberta OIDC é exigida na inicialização, mas a disponibilidade contínua do IdP não entra na readiness, seguindo o contrato do desafio. Com as chaves em cache, tokens válidos podem continuar sendo verificados durante uma falha temporária do Keycloak.

Os testes unitários cobrem configuração, validação de tokens, autorização e prazos das verificações de saúde. A composição do Fx é validada sem infraestrutura. Os testes com build tag de integração usam PostgreSQL, Keycloak e LocalStack reais e verificam também a liberação de recursos e o encerramento com requisições em andamento.

### Recuperação do Keycloak

O script de recuperação interrompe as dependências e verifica a retomada da API e da emissão de tokens. Esse teste mostrou que o Keycloak mantinha conexões inválidas depois de reiniciar o PostgreSQL.

A configuração local passou a validar conexões antes de reutilizá-las, por uma propriedade do Quarkus. As métricas do Keycloak também estão habilitadas para incluir o banco na verificação de readiness. Como a propriedade direta do Quarkus não faz parte da configuração oficialmente suportada pelo Keycloak, ela precisa ser revalidada ao atualizar a imagem. Referências: [configuração do Keycloak](https://www.keycloak.org/server/configuration) e [fontes de dados do Quarkus](https://quarkus.io/guides/all-config/#quarkus-agroal_quarkus-datasource-jdbc-validate-on-borrow).

## Entrega e limitações

Os fluxos financeiros, mensageria e recuperação estão implementados. Consultas por ID interno e externo filtram provider_id no SQL. Ledger usa cursor vinculado à carteira e wallet_version crescente, único por carteira. Reconciliação usa uma única instrução SQL para obter visão MVCC consistente e calcular armazenado menos créditos líquidos, sem mutações.

O SQS compartilha Process com HTTP. A inbox bloqueia por (consumer_name,message_id), valida hash SHA-256 do envelope tipado normalizado e registra conclusão junto com domínio e outbox. Alterações de conteúdo sob a mesma identidade retornam conflito e seguem para DLQ. Entregas confirmadas incrementam deliveries. Tentativas de processamento têm prazo de 5 segundos, visibilidade de 30 segundos e backoff transitório de 2 a 16 segundos; mensagens permanentes aguardam 2 segundos. Redrive ocorre após cinco recebimentos. Publisher usa backoff exponencial até 256 segundos, sem descartar eventos.

O Fx encerra HTTP e workers antes de fechar clientes e banco. Cancelamento desfaz transações não confirmadas; falhas de DeleteMessage deixam mensagens recuperáveis. Leases expiram após interrupção abrupta. Output usa grupo por carteira e deduplicação por eventId; publishers concorrentes não garantem ordenação financeira de eventos. O contrato é pelo menos uma vez.

A integração usa três processos independentes, tokens reais e barreiras injetadas exclusivamente no teste para SIGKILL nos pontos de commit, claim e publicação. PostgreSQL e LocalStack verificam efeitos, inbox, leases, retries e DLQ. Veja scripts/check-distributed.sh e README.md para reprodução. A observabilidade financeira está descrita abaixo.

Os modelos IAM e scripts/setup-broker-access.sh provisionam produtor confiável e worker com permissões distintas. LocalStack não comprova autorização IAM. A fila compartilhada pressupõe produtores confiáveis; providerId do corpo não é autenticado por IAM. Para produtores externos não confiáveis, exigir gateway autenticado ou filas por provedor. Produção também exige TLS, gestão de credenciais e permissões restritas no banco.

## Observabilidade

O serviço financeiro possui um registro de métricas por processo, protegido por mutex, sem dependência de frameworks. Os labels são estados de um conjunto fechado; identificadores não entram nas séries. A latência usa soma/contagem em segundos. Contadores de tentativas não são auditoria financeira: podem se perder no crash após commit ou contar replays. A auditoria durável continua no ledger/inbox/outbox.

O HTTP gera `X-Correlation-ID` UUID por requisição e propaga pelo contexto até o serviço. Logs JSON registram IDs disponíveis, resultado, replay e failureCode, sem tokens, credenciais, payloads completos ou erros SQL brutos. A resolução de referências usa o ID da transação para correlação; publicações registram eventId, walletId, tentativa e confirmação. IDs externos são tratados como dados pelo encoder JSON, sem interpolação em linhas de log.

`GET /metrics` passa pela autenticação e autorização interna. Consulta backlog/idade da outbox confirmada e profundidade visível aproximada da DLQ com timeout compartilhado; falhas retornam 503. Esses gauges refletem o banco/broker compartilhado e não devem ser somados entre instâncias. Contadores e soma/contagem de latência devem ser agregados entre processos. A diferença de reconciliação fica na resposta e no log em unidades mínimas; a métrica registra o número de divergências. Não há exporter externo, dashboards, alertas, histogramas de percentis ou tracing nesta entrega.

O catálogo de métricas e comandos de release está no README; RELEASE.md registra os checks executados. Os quatro eventos tipados de versão 1 e todos os failureCode estão definidos acima e em internal/domain/event.go. Todos os eventos vão para wager-events.fifo, com grupo por carteira e deduplicação por eventId. O evento de transação usa aggregateId=transactionId; WalletBalanceChanged usa aggregateId=walletId. occurredAt é UTC, Money é {amount,currency}, e causationId é omitido quando não disponível. Consumidores devem deduplicar eventId persistentemente e tolerar repetição e ordem de publicação diferente da versão financeira.

### Dados dos eventos de saída (version=1)

Todos os campos abaixo são JSON camelCase. O bloco comum de transação contém `transactionId`, `walletId`, `playerId`, `kind`, `money` e, para operações externas, `providerId`, `externalTransactionId`, `roundId`, `gameId`. Cada Money tem `amount` string decimal de duas casas e `currency`.

| eventType | Gatilho | data |
| --- | --- | --- |
| WagerTransactionProcessed | Operação confirmada em PROCESSED, inclusive OPENING positivo e LOSS | Bloco comum + balance (saldo original de processamento) |
| WagerTransactionRejected | Rejeição definitiva, inclusive expiração da referência | Bloco comum + failureCode |
| WagerTransactionPendingReference | Primeiro aceite pendente; retries não repetem esse evento | Bloco comum + referenceExternalTransactionId |
| WalletBalanceChanged | Movimento positivo confirmado, inclusive abertura; LOSS não emite | walletId, transactionId, direction (CREDIT/DEBIT), money, balanceBefore, balanceAfter, walletVersion |

Envelope ilustrativo:

```json
{
  "eventId": "0192f28f-5dc0-4d58-bdb2-814ad6a0f4a1",
  "eventType": "WagerTransactionProcessed",
  "aggregateId": "0192f28f-5dc0-4d58-bdb2-814ad6a0f4a2",
  "correlationId": "0192f28f-5dc0-4d58-bdb2-814ad6a0f4a2",
  "occurredAt": "2026-10-16T14:00:00Z",
  "version": 1,
  "data": {
    "transactionId": "0192f28f-5dc0-4d58-bdb2-814ad6a0f4a2",
    "walletId": "0192f28f-5dc0-4d58-bdb2-814ad6a0f4a3",
    "playerId": "0192f28f-5dc0-4d58-bdb2-814ad6a0f4a4",
    "providerId": "provider-a",
    "externalTransactionId": "bet-1",
    "roundId": "round-1",
    "gameId": "game-1",
    "kind": "BET",
    "money": {"amount": "25.00", "currency": "BRL"},
    "balance": {"amount": "75.00", "currency": "BRL"}
  }
}
```

Os IDs do exemplo são ilustrativos. A serialização real pode incluir frações de segundo em occurredAt. Consumidores devem validar eventType/version, tolerar campos adicionais compatíveis e encaminhar versões desconhecidas para tratamento próprio sem reaplicar efeitos financeiros. Não há roteamento distinto por tipo: todos os tipos compartilham a fila de saída.
