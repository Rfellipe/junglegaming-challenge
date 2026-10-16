# Arquitetura

## Organização e ciclo de vida

O Uber Fx monta configuração, PostgreSQL, SQS, autenticação, serviço financeiro, worker de referências e HTTP por construtores. O domínio em `internal/domain` não depende de Fx, HTTP, SQS ou pgx. `internal/wagering` coordena persistência e casos de uso; `internal/httpapi` aplica identidade, limites de entrada e contratos HTTP.

A configuração é lida uma vez e validada antes da inicialização. As URLs do PostgreSQL e do emissor OIDC são obrigatórias. As chamadas externas usam contextos com timeout, e o servidor HTTP limita leitura de cabeçalhos, leitura, escrita e tempo ocioso.

A API só começa a aceitar conexões depois das verificações de PostgreSQL, SQS e descoberta OIDC. No SIGTERM, o Fx para novas requisições e aguarda as que estão em andamento até o prazo configurado. Se o prazo acabar, fecha as conexões HTTP. Depois cancela e aguarda o worker de referências, libera as conexões ociosas de OIDC/SQS e fecha o pool do banco. Se a inicialização falhar, os recursos parcialmente abertos são fechados e o Fx desfaz os hooks concluídos.

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

Os tokens duram 60 segundos para permitir testes de expiração real. As rotas de carteira exigem serviço interno. A entrada financeira exige um provedor autenticado e compara seu `provider_id` com o corpo antes de chamar o caso de uso, incluindo replays. Não há endpoint de leitura de transações nesta etapa; consultas futuras deverão repetir o isolamento por provedor.

## Dinheiro e entidades

`Money` guarda moeda e unidades mínimas em campos privados, sem ponto flutuante. O limite é o de `int64`: de -92233720368547758.08 a 92233720368547758.07. Parsing, soma, subtração e negação verificam overflow. A escala externa é exatamente duas casas; expoentes, sinais positivos, espaços, valores vazios e escala diferente são inválidos. O parsing interno aceita negativos para diferenças; o decoder JSON de entrada rejeita qualquer sinal negativo, inclusive `-0.00`. A serialização usa strings decimais.

BRL é a única moeda das carteiras e operações. O value object também reconhece USD e EUR para operações internas e testes de incompatibilidade, sempre com escala de duas casas. Códigos desconhecidos são rejeitados. Zeros à esquerda são aceitos e normalizados; UUIDs são normalizados para minúsculas. Nenhuma outra string de negócio é aparada ou alterada.

Wallet e Transaction têm estado privado, validação na criação/reidratação e métodos explícitos de transição. Reidratar não movimenta saldo nem emite eventos. Uma movimentação positiva incrementa a versão da carteira uma vez; LOSS e rejeições não mudam saldo ou versão. O ledger valida direção e a equação do saldo no domínio e no banco. OPENING positivo nasce processado, com versão da carteira 1; abertura zero não cria operação financeira.

## Transação SQL e concorrência

O serviço abre uma transação pgx em READ COMMITTED e passa o mesmo `pgx.Tx` às gravações de operação, carteira, ledger e outbox. Não há conexão separada para um desses registros. A abertura inclui carteira, OPENING e seus eventos no mesmo commit. Uma falha, inclusive ao gravar evento, desfaz tudo.

Cada operação nova obtém `SELECT ... FOR UPDATE` na carteira antes de avaliar saldo ou referências. Esse lock é do PostgreSQL, por carteira, compartilhado por todas as instâncias. Não há mutex ou lock global. Os replays já confirmados podem ler o resultado persistido sem bloquear escritores. Constraints únicas arbitram identidades concorrentes; após uma colisão no INSERT, uma nova consulta dentro da mesma transação observa a identidade confirmada e decide replay/conflito.

O schema impõe unicidade de jogador/moeda, provedor/ID externo, provedor/chave, OPENING por carteira e carteira/transação no ledger. FKs compostas mantêm carteira, jogador, moeda e valor coerentes. CHECKs impedem saldo negativo e valores financeiros inválidos. Triggers protegem identidade/versão da carteira, transações terminais, o payload dos eventos e o ledger contra UPDATE, DELETE e TRUNCATE. O append valida o saldo anterior contra o lançamento anterior e o saldo posterior/versão contra a carteira bloqueada. Constraints diferidas verificam a soma do ledger e a correspondência entre operação processada e lançamento antes de confirmar o commit.

As verificações do saldo por soma têm custo proporcional ao histórico da carteira e favorecem a auditabilidade deste desafio; otimizações futuras precisam preservar a mesma garantia. O usuário local do banco é administrativo: as proteções valem para SQL comum, mas não pretendem impedir um administrador de desabilitar triggers. Permissões de produção continuam pendentes.

## Idempotência e estados

O hash é SHA-256 do JSON gerado por mapas Go, com chaves ordenadas lexicograficamente, incluindo o mapa interno de Money. Campos: providerId, externalTransactionId, playerId, walletId, roundId, gameId, kind, money (amount/currency) e referenceExternalTransactionId. Referência ausente é string vazia no hash. UUIDs são minúsculos e Money usa sua forma decimal normalizada; chave de idempotência e metadados de transporte ficam fora do hash. Esse contrato deverá ser usado sem alterações pelo futuro consumidor SQS.

Chave, identidade externa e hash precisam coincidir para replay. Reutilizar chave com conteúdo diferente, reutilizar identidade externa com outra chave, ou cruzar duas identidades já existentes resulta em conflito. O resultado terminal armazena saldo e failureCode; o replay devolve esses dados originais, independentemente do saldo atual.

Transições: `PENDING → PROCESSED | REJECTED | FAILED | PENDING_REFERENCE`; `PENDING_REFERENCE → PENDING_REFERENCE | PROCESSED | REJECTED | FAILED`. Esperar referência exige uma referência externa. Estados terminais são imutáveis. OPENING interno já é criado em PROCESSED. Operações independentes não confirmam PENDING intermediário: terminam no mesmo commit ou sofrem rollback. O banco rejeita um PENDING confirmado, pois não há worker genérico para retomá-lo.

Rejeições de domínio são persistidas como REJECTED com evento. Erros de banco, cancelamento e indisponibilidade são transitórios nesta etapa: rollback e HTTP 503, com retry pela mesma chave. FAILED é um estado previsto e validado pelo domínio/schema, mas ainda não é emitido por este caminho HTTP, que não transforma uma falha de infraestrutura não confirmada em resultado terminal.

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

## Outbox nesta etapa

Construtores concretos fixam tipo e versão 1 dos eventos WagerTransactionProcessed, WagerTransactionRejected, WagerTransactionPendingReference e WalletBalanceChanged. Os envelopes usam IDs UUID estáveis, aggregateId, correlationId igual ao ID da operação, UTC/RFC 3339 e Money em strings; causationId é opcional e omitido. Eventos de abertura omitem metadados externos inaplicáveis. WalletBalanceChanged inclui direção, valor, saldo anterior/posterior e versão da carteira. LOSS emite apenas o evento de processamento.

O JSON é serializado uma vez e gravado como snapshot imutável na transação financeira. Não há publicação SQS nesta etapa. As colunas de tentativas, próximo envio, publicação e lease ficam prontas para o publisher do próximo milestone; a inbox também existe apenas como schema.

## Filas e saúde

O LocalStack cria as filas FIFO de entrada, DLQ e saída, sem deduplicação por conteúdo. A entrada tem visibilidade de 30 segundos e envio à DLQ após cinco recebimentos. As URLs usam o endpoint do chamador, permitindo acesso pela máquina local e pelos containers. O SDK da AWS usa a cadeia padrão de credenciais e limita as novas tentativas.

A liveness verifica o processo HTTP. A readiness consulta o PostgreSQL e as três filas dentro de um prazo compartilhado. A descoberta OIDC é exigida na inicialização, mas a disponibilidade contínua do IdP não entra na readiness, seguindo o contrato do desafio. Com as chaves em cache, tokens válidos podem continuar sendo verificados durante uma falha temporária do Keycloak.

Os testes unitários cobrem configuração, validação de tokens, autorização e prazos das verificações de saúde. A composição do Fx é validada sem infraestrutura. Os testes com build tag de integração usam PostgreSQL, Keycloak e LocalStack reais e verificam também a liberação de recursos e o encerramento com requisições em andamento.

### Recuperação do Keycloak

O script de recuperação interrompe as dependências e verifica a retomada da API e da emissão de tokens. Esse teste mostrou que o Keycloak mantinha conexões inválidas depois de reiniciar o PostgreSQL.

A configuração local passou a validar conexões antes de reutilizá-las, por uma propriedade do Quarkus. As métricas do Keycloak também estão habilitadas para incluir o banco na verificação de readiness. Como a propriedade direta do Quarkus não faz parte da configuração oficialmente suportada pelo Keycloak, ela precisa ser revalidada ao atualizar a imagem. Referências: [configuração do Keycloak](https://www.keycloak.org/server/configuration) e [fontes de dados do Quarkus](https://quarkus.io/guides/all-config/#quarkus-agroal_quarkus-datasource-jdbc-validate-on-borrow).

## Próxima etapa e limitações

A estrutura atual processa as cinco operações externas por HTTP e retoma referências pendentes. Consultas de transação, paginação do ledger e reconciliação ficam para a etapa 4. Consumidor SQS, inbox transacional, publicador da outbox e recuperação de publicação ficam para a etapa 5. A conclusão futura da inbox deve compartilhar o mesmo pgx.Tx do caso de uso financeiro.

Os testes usam PostgreSQL, Keycloak e LocalStack reais, além de testes puros do domínio. Cobrem concorrência de apostas e reversões, SQL inválido, erro injetado na outbox, TTL, workers concorrentes e reinício da aplicação Fx. Ainda faltam três processos independentes, interrupção abrupta nos pontos de publicação/remoção SQS, métricas financeiras e logs correlacionados por operação.

A configuração local usa portas expostas apenas na interface local e credenciais de exemplo. TLS, armazenamento e rotação de credenciais, permissões restritas no banco e políticas IAM do broker ficam pendentes para produção. O LocalStack não comprova essas regras de autorização.
