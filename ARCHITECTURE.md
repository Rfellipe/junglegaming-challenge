# Arquitetura

## Organização e ciclo de vida

O Uber Fx monta configuração, PostgreSQL, SQS, autenticação e HTTP por construtores. Os adaptadores ficam em `internal/`. O domínio será implementado separado dessas dependências.

A configuração é lida uma vez e validada antes da inicialização. As URLs do PostgreSQL e do emissor OIDC são obrigatórias. As chamadas externas usam contextos com timeout, e o servidor HTTP limita leitura de cabeçalhos, leitura, escrita e tempo ocioso.

A API só começa a aceitar conexões depois das verificações de PostgreSQL, SQS e descoberta OIDC. No SIGTERM, o Fx para novas requisições e aguarda as que estão em andamento até o prazo configurado. Se o prazo acabar, fecha as conexões HTTP. Depois libera as conexões ociosas de OIDC/SQS e o pool do banco. Se a inicialização falhar, os recursos parcialmente abertos são fechados e o Fx desfaz os hooks concluídos.

Os logs são JSON, com `slog` e o adaptador do Fx. Não incluem strings de conexão, credenciais, tokens ou payloads financeiros. As respostas públicas de saúde mostram apenas o estado das dependências.

## Banco e migrations

O acesso ao PostgreSQL usa `pgx/v5`, pool limitado e SQL explícito. As migrations usam golang-migrate com o driver pgx v5 e locks consultivos do PostgreSQL. Conexão, aquisição do lock e execução de instruções têm limite de 10 segundos.

As migrations rodam em um serviço separado do Compose, antes da API. Isso evita que cada instância tente migrar o banco. O comando de avanço aplica as pendentes; o de reversão remove uma versão. A primeira migration cria o schema `wagering`, e a reversão usa `DROP SCHEMA ... RESTRICT`.

O executor fixa o `search_path` em `public` e mantém o histórico em `public.schema_migrations`. Com o padrão `$user`, criar um schema com o nome do usuário mudava o local em que o driver procurava o histórico. As próximas consultas financeiras devem usar o schema `wagering` explicitamente.

O teste de migrations usa um banco temporário e executa avanço duas vezes, reversão e avanço novamente.

## Autenticação e autorização

O Keycloak cuida da emissão de tokens. A aplicação usa OAuth/OIDC e não armazena senhas de usuários. O ambiente local importa dois clientes de provedor e um interno, todos com credenciais de cliente. Claims fixas por cliente definem o provedor; o mapeador de audiência inclui `wagering-api`.

O middleware aceita JWTs assinados com RS256 e verifica emissor, audiência, expiração, sujeito e `service_role`. Um provedor precisa ter `provider_id` preenchido; o serviço interno não pode ter esse campo. As funções de autorização verificam a correspondência do provedor ou exigem a identidade interna.

O emissor local é `http://localhost:8081/realms/jungle`. Dentro do Compose, um transporte HTTP do cliente OIDC troca apenas a origem da requisição por `http://keycloak:8080`. Assim, descoberta e JWKS funcionam na rede dos containers sem desabilitar a validação do emissor. Fora do Docker, o backchannel fica vazio. As consultas de JWKS também têm timeout.

Os tokens duram 60 segundos para permitir testes de expiração real. A autenticação e a autorização são exercitadas em handlers de teste enquanto os endpoints de negócio não estão prontos.

## Filas e saúde

O LocalStack cria as filas FIFO de entrada, DLQ e saída, sem deduplicação por conteúdo. A entrada tem visibilidade de 30 segundos e envio à DLQ após cinco recebimentos. As URLs usam o endpoint do chamador, permitindo acesso pela máquina local e pelos containers. O SDK da AWS usa a cadeia padrão de credenciais e limita as novas tentativas.

A liveness verifica o processo HTTP. A readiness consulta o PostgreSQL e as três filas dentro de um prazo compartilhado. A descoberta OIDC é exigida na inicialização, mas a disponibilidade contínua do IdP não entra na readiness, seguindo o contrato do desafio. Com as chaves em cache, tokens válidos podem continuar sendo verificados durante uma falha temporária do Keycloak.

Os testes unitários cobrem configuração, validação de tokens, autorização e prazos das verificações de saúde. A composição do Fx é validada sem infraestrutura. Os testes com build tag de integração usam PostgreSQL, Keycloak e LocalStack reais e verificam também a liberação de recursos e o encerramento com requisições em andamento.

### Recuperação do Keycloak

O script de recuperação interrompe as dependências e verifica a retomada da API e da emissão de tokens. Esse teste mostrou que o Keycloak mantinha conexões inválidas depois de reiniciar o PostgreSQL.

A configuração local passou a validar conexões antes de reutilizá-las, por uma propriedade do Quarkus. As métricas do Keycloak também estão habilitadas para incluir o banco na verificação de readiness. Como a propriedade direta do Quarkus não faz parte da configuração oficialmente suportada pelo Keycloak, ela precisa ser revalidada ao atualizar a imagem. Referências: [configuração do Keycloak](https://www.keycloak.org/server/configuration) e [fontes de dados do Quarkus](https://quarkus.io/guides/all-config/#quarkus-agroal_quarkus-datasource-jdbc-validate-on-borrow).

## Próxima etapa e limitações

A estrutura atual não processa operações financeiras nem consome ou publica mensagens. O domínio, as tabelas financeiras e os repositórios ainda serão implementados.

A próxima etapa precisa definir uma transação SQL que confirme operação, idempotência, saldo, ledger e outbox juntos. Na entrada por SQS, a conclusão da inbox deve fazer parte dessa transação. Os futuros handlers devem obter o provedor da identidade autenticada e verificar a titularidade em operações, consultas e replays. Os workers precisarão de cancelamento e liberação de trabalho integrados ao ciclo de vida.

A configuração local usa portas expostas apenas na interface local e credenciais de exemplo. TLS, armazenamento e rotação de credenciais, permissões restritas no banco e políticas IAM do broker ficam pendentes para produção. O LocalStack não comprova essas regras de autorização.
