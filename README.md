# Desafio backend em Go

Serviço de processamento de apostas com Go e Uber Fx. O enunciado está em [CHALLENGE.md](CHALLENGE.md) e as decisões técnicas em [ARCHITECTURE.md](ARCHITECTURE.md).

Por enquanto, estão prontos o ambiente local, as migrations, a autenticação e os endpoints de saúde. As operações financeiras entram na próxima etapa.

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

A migration inicial cria o schema `wagering`. A reversão usa `RESTRICT` para não excluir tabelas que venham a ser adicionadas. As versões ficam em `public.schema_migrations`. Se uma migration falhar e ficar marcada como `dirty`, confira o SQL e o estado do banco antes de corrigir a versão. Não há reset automático.

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
go test -tags=integration -race -count=1 -timeout=3m ./...
```

Os testes de integração cobrem migrations, configuração das filas e da DLQ, autenticação, isolamento de provedores, expiração de tokens, falhas de inicialização, readiness e encerramento com requisições em andamento. O teste de expiração leva cerca de um minuto.

As migrations são testadas em um banco temporário. A conta precisa de permissão para criar e excluir esse banco; a configuração local já permite isso. Use um ambiente descartável. Para mudar os endereços dos testes, defina `TEST_DATABASE_URL`, `TEST_OIDC_ISSUER_URL`, `TEST_OIDC_BACKCHANNEL_URL` e `TEST_SQS_ENDPOINT`.

### Recuperação de dependências

```sh
docker compose up --build --detach --wait
scripts/check-recovery.sh
```

O script para LocalStack e PostgreSQL, um por vez, e verifica se a readiness passa a responder `503` enquanto a liveness mantém `200`. Depois reinicia a dependência e confere a recuperação da API e da emissão de tokens. Precisa de `curl` e Python 3 e deixa o ambiente rodando ao terminar.

Para conferir o encerramento por SIGTERM, execute `docker compose stop app` e consulte os logs. Depois, reinicie com `docker compose up --detach --wait app`.

## O que falta

As rotas `/wallets` e `/wagering/transactions` ainda retornam `404`. A próxima etapa é implementar dinheiro, carteiras, tabelas financeiras e processamento de apostas, com ledger, idempotência e outbox na mesma transação.

Também ficam pendentes os consumidores e publicadores SQS, inbox, recuperação de operações, testes financeiros com múltiplas instâncias e simulações de falha entre commit e publicação. As futuras rotas precisam aplicar autenticação e autorização também nas consultas e nos replays.

O ambiente usa HTTP e credenciais locais. TLS, gestão de credenciais e políticas IAM do broker ficam para a configuração de produção; as credenciais `test` do LocalStack não validam essas políticas.
