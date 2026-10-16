# Desafio backend em Go

Serviço de processamento de apostas com Go e Uber Fx. O enunciado está em [CHALLENGE.md](CHALLENGE.md) e as decisões técnicas em [ARCHITECTURE.md](ARCHITECTURE.md).

Estão implementados o ambiente local, autenticação, saúde, domínio financeiro, abertura e consulta de carteiras, processamento HTTP dos cinco tipos externos, ledger, idempotência persistente e retomada de referências pendentes. Também estão implementados consultas isoladas por provedor, paginação do ledger, reconciliação, inbox transacional, consumo SQS e publicação recuperável da outbox.

## Executar o projeto

Pré-requisitos: Docker com Compose e acesso ao daemon. Para rodar Go fora do container, use a versão **1.27.1**. Os scripts de token e recuperação precisam de `curl` e Python 3.

As portas **5432, 4566, 8080 e 8081** precisam estar livres.

```sh
cp .env.example .env
docker compose up --build
```

O Compose sobe PostgreSQL, Keycloak e LocalStack, cria as filas e aplica as migrations antes de iniciar a API. Ele já tem os valores locais configurados, então o `.env` é opcional para executar pelos containers.

Para rodar em segundo plano e consultar o estado da aplicação:

```sh
docker compose up --build --detach
curl http://localhost:8080/health/live
curl http://localhost:8080/health/ready
docker compose ps
```

A primeira inicialização pode demorar por causa do download das imagens e da compilação. Aguarde a API iniciar antes de chamar as rotas.

| Rota | Resposta |
| --- | --- |
| `/health/live` | `200` com `{"status":"alive"}` enquanto o HTTP responde |
| `/health/ready` | `200` com PostgreSQL e as três filas disponíveis; `503` em caso de falha |

As duas rotas são públicas. A readiness usa um prazo compartilhado para consultar as dependências.

Para acompanhar os logs ou parar o ambiente:

```sh
docker compose logs -f app
docker compose down
```

O comando `down` mantém o volume do PostgreSQL, incluindo o banco separado do Keycloak. As mensagens do LocalStack são temporárias; as filas são recriadas quando o container inicia.

## Configuração

O `.env.example` usa endereços acessíveis pela máquina local. No Compose, os serviços usam os endereços da rede dos containers. As credenciais incluídas são apenas exemplos de desenvolvimento.

| Variável | Uso / padrão |
| --- | --- |
| `HTTP_ADDR` | Endereço do servidor HTTP; `:8080` |
| `DATABASE_URL` | URL de conexão com o PostgreSQL; obrigatória |
| `AWS_REGION` | Região; `us-east-1` |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | Credenciais da AWS; localmente, `test` |
| `AWS_EC2_METADATA_DISABLED` | `true` no ambiente local |
| `SQS_ENDPOINT` | Endpoint do LocalStack; omita para usar a AWS |
| `SQS_INPUT_QUEUE`, `SQS_DLQ`, `SQS_EVENTS_QUEUE` | Nomes das filas descritas na seção SQS |
| `OIDC_ISSUER_URL` | Emissor do token; obrigatório. Local: `http://localhost:8081/realms/jungle` |
| `OIDC_BACKCHANNEL_URL` | Origem interna do OIDC; vazia fora do Docker e `http://keycloak:8080` no Compose |
| `OIDC_AUDIENCE` | Audiência do token; `wagering-api` |
| `DEPENDENCY_TIMEOUT` | Timeout das dependências e da readiness; `3s` |
| `STARTUP_TIMEOUT`, `SHUTDOWN_TIMEOUT` | Prazos do Fx; `30s` e `15s` |

### Rodar o Go fora do Docker

A aplicação lê variáveis de ambiente, mas não carrega o `.env` sozinha:

```sh
docker compose up --detach --wait postgres keycloak localstack
docker compose stop app
set -a
. ./.env
set +a
go run ./cmd/migrate up
go run ./cmd/challenge
```

Pare o processo local antes de voltar a API para o container: os dois usam a porta 8080.

## Migrations

O Compose aplica as migrations automaticamente em um serviço separado. Para aplicar as pendentes manualmente:

```sh
docker compose run --rm migrate up
```

Para reverter **uma migration**, pare a API primeiro:

```sh
docker compose stop app
docker compose run --rm migrate down
```

Para reaplicar e subir a API:

```sh
docker compose run --rm migrate up
docker compose up --detach app
```

Fora do Docker, os comandos são `go run ./cmd/migrate up` e `go run ./cmd/migrate down`, com as variáveis de ambiente carregadas.

A primeira migration cria o schema `wagering`; a segunda cria as tabelas financeiras, inbox, outbox e proteções do banco. Reverter a segunda remove essas tabelas e seus dados: use apenas em ambiente descartável. A reversão do schema usa `RESTRICT` para não excluir tabelas que venham a ser adicionadas. A terceira migration adiciona o contador de entregas da inbox. As versões ficam em `public.schema_migrations`. Se uma migration falhar e ficar marcada como `dirty`, confira o SQL e o estado do banco antes de corrigir a versão. Não há reset automático.

## Operações financeiras

As operações de carteira exigem token de `internal-service`. O envio de apostas exige token do provedor informado em `providerId`, inclusive nos replays. Valores externos usam strings com exatamente duas casas, somente BRL, sem sinal negativo, expoente ou arredondamento. Zeros à esquerda são normalizados antes do hash.

```sh
INTERNAL_TOKEN=$(scripts/token.sh internal-service)
PROVIDER_TOKEN=$(scripts/token.sh provider-a)

curl --fail-with-body http://localhost:8080/wallets \
  -H "Authorization: Bearer $INTERNAL_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"100.00","currency":"BRL"}}'
```

Copie o `id` retornado para `WALLET_ID` e envie a operação. A carteira desse exemplo só pode ser aberta uma vez por jogador/moeda; use outro UUID para repetir a abertura.

```sh
WALLET_ID='<id retornado>'
curl --fail-with-body http://localhost:8080/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: provider-a:bet-1' \
  -d "{\"providerId\":\"provider-a\",\"externalTransactionId\":\"bet-1\",\"playerId\":\"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1\",\"walletId\":\"$WALLET_ID\",\"roundId\":\"round-1\",\"gameId\":\"game-1\",\"kind\":\"BET\",\"money\":{\"amount\":\"25.00\",\"currency\":\"BRL\"}}"

curl --fail-with-body "http://localhost:8080/wallets/$WALLET_ID" \
  -H "Authorization: Bearer $INTERNAL_TOKEN"
```

O saldo fica em `75.00`. Repetir a aposta retorna o mesmo resultado com `idempotentReplay: true`, mesmo depois de outras movimentações ou reinicialização. Outra chave para a mesma operação, ou outro conteúdo para a mesma chave, retorna conflito. Renove os tokens quando expirarem (60 segundos).

`WIN` credita valor positivo. `LOSS` exige `0.00`, não muda saldo/versão e não cria ledger. `REFUND` e `ROLLBACK` exigem `referenceExternalTransactionId` e valor integral da referência. `OPENING` é exclusivo da abertura interna. Para uma reversão, use novos identificadores/chave, mantenha os metadados da aposta e acrescente a referência externa ao corpo.

Referências ausentes ou ainda pendentes retornam `202` e ficam persistidas em `PENDING_REFERENCE`. Um worker integrado ao Fx tenta resolver a dependência com backoff exponencial, até 12 tentativas de resolução ou 24 horas. Reenviar o mesmo pedido consulta o estado atualizado; não antecipa o retry. O worker retoma pendências após reinicialização.

| Situação | HTTP / corpo |
| --- | --- |
| Carteira criada | `201`, carteira com `version: 1` |
| Carteira consultada | `200`, carteira atual |
| Operação processada ou replay processado | `200`, `transactionId`, `status`, `balance`, `idempotentReplay` |
| Referência pendente | `202`, mesmo formato com `status: PENDING_REFERENCE`; saldo atual sem movimentação |
| Rejeição definitiva ou replay rejeitado | `422`, mesmo formato com `failureCode` e saldo persistido |
| Entrada inválida, inclusive chave ausente | `400`, `{"error":"invalid_input"}` |
| Conflito de carteira/chave/identidade externa | `409`, `{"error":"conflict"}` |
| Carteira inexistente | `404`, `{"error":"wallet_not_found"}` |
| Token ausente/inválido ou permissão insuficiente | `401` / `403` |
| Falha transitória ou timeout | `503`, `{"error":"temporarily_unavailable"}`, `Retry-After: 1`; reenvie a mesma chave |

Rejeições financeiras são terminais e auditáveis. Os códigos são `INSUFFICIENT_FUNDS`, `REVERSAL_INSUFFICIENT_FUNDS`, `BALANCE_OVERFLOW`, `INVALID_REFERENCE`, `REFERENCE_MISMATCH`, `INVALID_REFERENCE_KIND`, `REFERENCE_AMOUNT_MISMATCH`, `REFERENCE_NOT_PROCESSED`, `ALREADY_REVERSED` e `REFERENCE_NOT_FOUND`. A política detalhada está em [ARCHITECTURE.md](ARCHITECTURE.md).

## Autenticação

O Keycloak fica em <http://localhost:8081>, com acesso administrativo local `admin` / `admin-local`.

O realm vem com três clientes OAuth confidenciais. Todos usam `client_credentials`, com os fluxos de navegador e senha desabilitados:

| Cliente | Segredo local | Claims |
| --- | --- | --- |
| `provider-a` | `provider-a-local` | `service_role=provider`, `provider_id=provider-a` |
| `provider-b` | `provider-b-local` | `service_role=provider`, `provider_id=provider-b` |
| `internal-service` | `internal-service-local` | `service_role=internal`, sem provedor |

Para obter um token:

```sh
scripts/token.sh provider-a
scripts/token.sh provider-b
scripts/token.sh internal-service
```

Os tokens duram 60 segundos. Para ver a resposta completa do Keycloak:

```sh
curl --fail --silent --show-error \
  http://localhost:8081/realms/jungle/protocol/openid-connect/token \
  --data-urlencode grant_type=client_credentials \
  --data-urlencode client_id=provider-a \
  --data-urlencode client_secret=provider-a-local
```

O middleware valida assinatura RS256, emissor, audiência, expiração, sujeito e claims de identidade. Token ausente ou inválido retorna `401`; falta de permissão retorna `403`. Os testes verificam isolamento entre provedores e acesso exclusivo do serviço interno.

O realm só é importado se ainda não existir. Editar o JSON de importação não atualiza um realm já criado: é preciso aplicar a mudança no Keycloak ou recriar o ambiente descartável.

## SQS

O script de inicialização do LocalStack cria três filas FIFO:

| Fila | Configuração |
| --- | --- |
| `wager-transactions.fifo` | Visibilidade de 30 segundos, long polling de 10 segundos e envio à DLQ após cinco recebimentos |
| `wager-transactions-dlq.fifo` | DLQ da entrada |
| `wager-events.fifo` | Saída de eventos de integração |

A deduplicação por conteúdo fica desabilitada. Os produtores deverão enviar um `MessageDeduplicationId` estável e um `MessageGroupId` por carteira. Isso não substitui a idempotência no banco.

Para listar as filas:

```sh
docker compose exec localstack awslocal sqs list-queues
```

## Testes

Sem containers:

```sh
go test ./...
go test -race ./...
go vet ./...
```

Com PostgreSQL, Keycloak e LocalStack reais:

```sh
docker compose up --detach --wait postgres keycloak localstack
go test -tags=integration -race -count=1 -timeout=5m ./...
```

Os testes de integração cobrem migrations, configuração das filas e da DLQ, autenticação, isolamento de provedores, expiração de tokens, falhas de inicialização, readiness e encerramento com requisições em andamento. O teste de expiração leva cerca de um minuto.

Também cobrem abertura, operações financeiras, replays após novas movimentações e troca de conexões, 50 duplicatas concorrentes, duas apostas de 80.00 sobre 100.00, independência entre carteiras, reversões, expiração de referências, workers concorrentes e rollback integral quando a gravação da outbox falha. Um teste HTTP usa tokens reais e reinicia a aplicação Fx para verificar retomada automática. As proteções do banco são testadas com SQL direto.

Para executar somente os novos testes financeiros:

```sh
go test -tags=integration -race -count=1 -timeout=5m ./internal/wagering
go test -tags=integration -race -count=1 -timeout=5m ./integration -run TestFinancialHTTPAndWorkerLifecycle
```

As migrations são testadas em um banco temporário. A conta precisa de permissão para criar e excluir esse banco; a configuração local já permite isso. Use um ambiente descartável. Para mudar os endereços dos testes, defina `TEST_DATABASE_URL`, `TEST_OIDC_ISSUER_URL`, `TEST_OIDC_BACKCHANNEL_URL` e `TEST_SQS_ENDPOINT`.

### Recuperação de dependências

```sh
docker compose up --build --detach --wait
scripts/check-recovery.sh
```

O script para LocalStack e PostgreSQL, um por vez, e verifica se a readiness passa a responder `503` enquanto a liveness mantém `200`. Depois reinicia a dependência e confere a recuperação da API e da emissão de tokens. Precisa de `curl` e Python 3 e deixa o ambiente rodando ao terminar.

Para conferir o encerramento por SIGTERM, execute `docker compose stop app` e consulte os logs. Depois, reinicie com `docker compose up --detach --wait app`.

## Validação e limitações

O registro reproduzível de validação e as limitações estão em [RELEASE.md](RELEASE.md).

O ambiente usa HTTP e credenciais locais. TLS, gestão/rotação de credenciais e validação de IAM na AWS continuam sendo responsabilidades de produção. LocalStack não comprova isolamento IAM.

## Consultas e contrato HTTP

As consultas `GET /wagering/transactions/{transactionId}` e `GET /providers/{providerId}/wagering/transactions/{externalTransactionId}` exigem token do provedor proprietário. A primeira filtra o provedor no SQL e responde 404 para IDs de outro provedor; a segunda responde 403 se o provedor da rota difere do token. O corpo inclui os campos do pedido, transactionId, status, balance, failureCode quando aplicável, idempotentReplay=false, createdAt e updatedAt. Para pendências sem resultado financeiro, balance é zero; não representa saldo de processamento confirmado.

`GET /wallets/{walletId}/ledger?limit=50&cursor=...` exige serviço interno e devolve `{"entries":[...],"nextCursor":"..."}`. Cada entrada contém id, transactionId, direction, money, balanceBefore, balanceAfter, walletVersion e createdAt. O limite é 1–200; o cursor é opaco, vinculado à carteira e ordenado por walletVersion crescente. Ausência de nextCursor indica fim da página atual; lançamentos futuros podem surgir depois. Passe o cursor sem modificá-lo.

`POST /wallets/{walletId}/reconciliation` exige serviço interno. Devolve walletId, storedBalance, calculatedBalance, difference (armazenado menos calculado), consistent e checkedEntries. Uma única instrução SQL usa o mesmo snapshot para carteira e lançamentos, incluindo OPENING. Não altera registros.

| Situação | HTTP | Corpo |
| --- | --- | --- |
| Processado ou replay concluído | 200 | transactionId, status, balance, idempotentReplay |
| Referência aguardando | 202 | mesmo resultado, status=PENDING_REFERENCE |
| Rejeição de negócio | 422 | mesmo resultado, status=REJECTED e failureCode |
| JSON, IDs, valores ou cursor inválidos | 400 | `{"error":"invalid_input"}` |
| Chave, identidade externa ou carteira em conflito | 409 | `{"error":"conflict"}` |
| Recurso ausente ou transação invisível | 404 | `{"error":"not_found"}` |
| Token ausente/inválido | 401 | erro de autenticação |
| Papel/provedor não autorizado | 403 | `{"error":"forbidden"}` |
| Falha transitória | 503 | `{"error":"temporarily_unavailable"}` e Retry-After: 1 |

Uma consulta bem-sucedida retorna 200 mesmo para uma transação rejeitada ou pendente. O POST conserva a chave enviada e o saldo original nos replays de operações concluídas. Os códigos de negócio estão em ARCHITECTURE.md.

## Inbox, publicação e credenciais

Envie o envelope de CHALLENGE.md com type=WagerTransactionRequested, messageId, occurredAt RFC3339 e data contendo o pedido financeiro mais idempotencyKey. JSON desconhecido, envelopes maiores que 16 KiB e valores inválidos são retidos para DLQ. messageId identifica a inbox `wagering-v1`; SHA-256 do envelope tipado serializado detecta mudanças de conteúdo, chave e metadados. A formatação JSON não afeta o hash. Reenvie exatamente o mesmo envelope nas reentregas.

A inbox, sua conclusão, o resultado financeiro e eventos compartilham o commit. Rejeições e referências pendentes confirmadas permitem DeleteMessage; falhas não confirmadas não permitem remoção. O contador deliveries registra reentregas duráveis. Cada tentativa tem prazo de 5 segundos e visibilidade de 30 segundos. Retry transitório usa 2, 4, 8 e 16 segundos (teto 16); mensagens inválidas/conflitantes aguardam 2 segundos. A política de redrive envia à DLQ após cinco recebimentos. Não há confirmação de inbox para mensagens inválidas.

Cada processo tem consumidor e publisher. O publisher reivindica uma linha com SKIP LOCKED e lease de 15 segundos; publica fora da transação e confirma apenas com seu lease_token. Falhas persistem backoff exponencial com teto de 256 segundos; eventos continuam recuperáveis sem limite de tentativas. A saída usa o envelope versionado de ARCHITECTURE.md, MessageGroupId=walletId e MessageDeduplicationId=eventId. Os produtores devem usar grupo por carteira e ID de deduplicação estável por mensagem. Para testar reentrega da aplicação dentro dos cinco minutos de deduplicação FIFO, use outro ID de broker preservando o envelope.

A publicação é pelo menos uma vez: interrupção após SendMessage pode causar reenvio. Consumidores de saída precisam deduplicar por eventId e não presumir ordem financeira entre publishers. FIFO pode suprimir uma republicação no intervalo de deduplicação. No SIGTERM os workers param de receber, cancelam a tentativa e liberam visibilidade em caso de erro; commit concluído permanece seguro para reentrega. Leases abandonados expiram.

Para provisionar usuários e políticas locais de produtor e worker:

```sh
./scripts/setup-broker-access.sh
./scripts/setup-broker-access.sh wagering-producer /tmp/jungle-producer-credentials.json
```

O segundo comando cria uma chave e grava a resposta com modo 600. Os modelos em deploy/aws restringem o produtor a SendMessage/GetQueueUrl na entrada e o worker a leitura/remoção da entrada e envio à saída. Para AWS, substitua região/conta e associe essas permissões a roles com credenciais temporárias. O acesso de entrada é reservado a produtores confiáveis: IAM na fila compartilhada não valida o providerId no JSON. Não conceda envio direto a provedores não confiáveis sem gateway autenticado ou filas isoladas. As credenciais test usadas pelo Compose não validam IAM; o script demonstra o provisionamento, e os testes financeiros não alegam verificar autorização AWS.

## Três processos e recuperação

```sh
./scripts/check-distributed.sh
# Ou, com as dependências já prontas:
go test -tags=integration -race -count=1 -timeout=5m ./...
```

O runner cria banco e filas temporários e inicia três processos reais com portas efêmeras distintas e pools independentes. Usa tokens reais do Keycloak e pausa/retoma brevemente o container LocalStack para comprovar recuperação da outbox durante indisponibilidade do broker. Exercita 50 replays, duas apostas concorrentes de 80.00, isolamento de locks, HTTP/SQS, reentrega comprovada pelo contador da inbox, poison/DLQ, reinício e referências. Barreiras injetadas apenas no binário de testes interrompem o processo após commit/antes de delete, após claim/antes de SendMessage e após SendMessage/antes de confirmação. O teste envia SIGKILL ao processo que atingiu a barreira e aguarda recuperação por outro publisher após expirar o lease. Confere saldo, ledger, resultado persistido, inbox e outbox. Não há endpoint ou variável de aplicação que ative essas barreiras.

## Observabilidade e release

Os logs JSON incluem IDs disponíveis de correlação, mensagem, transação, carteira e provedor, status e failureCode. Cada requisição recebe um UUID novo em `X-Correlation-ID`; eventos usam o ID da transação como correlationId. Não são registrados tokens, credenciais, chaves de idempotência ou corpos completos. A aplicação não aceita um correlationId arbitrário do cliente. Tentativas financeiras são registradas após a execução, incluindo replays e erros; um log não substitui o resultado durável do banco.

```sh
INTERNAL_TOKEN=$(scripts/token.sh internal-service)
curl --fail-with-body http://localhost:8080/metrics \
  -H "Authorization: Bearer $INTERNAL_TOKEN"
./scripts/check-release.sh
```

`GET /metrics` usa o formato textual Prometheus e exige token interno (401 sem token, 403 para provedor). Os contadores são locais a cada processo, começam do zero no reinício e aparecem após o primeiro evento. Colete todas as instâncias. Não há IDs de usuário/carteira/provedor nos labels. Métricas disponíveis:

| Métrica (`jungle_`) | Significado |
| --- | --- |
| transactions_total{status} | Tentativas HTTP/SQS por estado confirmado ou error, incluindo replays |
| duplicates_total | Replays reconhecidos pelo serviço |
| concurrency_conflicts_total | Conflitos de identidade/chave, inclusive disputas concorrentes |
| processing_seconds_sum / processing_seconds_count | Tempo total e número de tentativas financeiras; razão = média |
| reference_retries_total / reference_outcomes_total{status} | Retries confirmados e seu estado resultante |
| input_deliveries_total / input_retries_total / input_invalid_total | Recebimentos, adiamentos e adiamentos permanentes para redrive |
| outbox_retries_total | Tentativas de envio à saída que falharam |
| outbox_pending / outbox_oldest_seconds | Quantidade e idade do evento confirmado mais antigo ainda não publicado, globais no banco |
| dlq_messages | Profundidade visível aproximada da DLQ informada pelo SQS |
| reconciliations_total / reconciliation_mismatches_total | Reconciliações concluídas e divergências detectadas |

Falhas ao consultar banco ou DLQ fazem o scrape retornar 503. A diferença exata de reconciliação, em unidades mínimas, fica no log interno e na resposta de reconciliação; a métrica conta divergências sem criar uma série por carteira. A profundidade da DLQ é uma estimativa atual, não um contador de transferências históricas. Um scrape não remove mensagens. O IAM do worker precisa permitir GetQueueAttributes na DLQ.

Os contratos de todos os eventos de saída, seus campos e um envelope de exemplo estão em [ARCHITECTURE.md](ARCHITECTURE.md#dados-dos-eventos-de-saída-version1).

O gate completo usa build tag `integration`, race detector e timeout de cinco minutos por pacote. Inclui os três processos e SIGKILL nas barreiras de commit/claim/publicação; o teste de expiração real leva cerca de um minuto. `scripts/check-recovery.sh` executa separadamente a interrupção e recuperação das dependências. Consulte RELEASE.md antes de considerar uma execução validada.
