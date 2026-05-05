# rinha-de-backend-2026

API de detecção de fraudes para a [Rinha de Backend 2026](https://github.com/zanfranceschi/rinha-de-backend-2026).

## Arquitetura

```
                        ┌─────────────────────────────┐
                        │         nginx :9999          │
                        │      (round-robin LB)        │
                        └────────────┬────────────────┘
                                     │
                  ┌──────────────────┴──────────────────┐
                  ▼                                      ▼
         ┌────────────────┐                   ┌────────────────┐
         │   api1 :8080   │                   │   api2 :8080   │
         │   (Go/stdlib)  │                   │   (Go/stdlib)  │
         └────────────────┘                   └────────────────┘
```

- **Load balancer**: nginx com round-robin simples, sem lógica de negócio
- **API**: Go 1.24, stdlib pura, sem dependências externas
- **Limites de recursos**: 1 CPU / 350 MB total (nginx 0.1/50MB, cada instância 0.45/150MB)

### Estrutura de pastas

```
cmd/
  server/
    main.go           ← entrypoint: lê PORT e sobe o HTTP server
internal/
  domain/
    fraud.go          ← entidades (FraudInput, FraudResult) e interface FraudScorer
  usecase/
    fraud_score.go    ← ScoreFraud: orquestra a chamada ao scorer
  scorer/
    noop.go           ← implementação placeholder (aprova tudo, score 0.0)
  handler/
    router.go         ← monta o http.ServeMux com as rotas
    fraud.go          ← POST /fraud-score: decode, mapeia para domain, chama usecase
    ready.go          ← GET /ready: healthcheck
```

## Endpoints

### `GET /ready`

Healthcheck. Retorna `200 OK` quando a instância está pronta para receber tráfego.

### `POST /fraud-score`

Avalia o risco de fraude de uma transação e retorna se ela deve ser aprovada.

**Request**

```json
{
  "id": "tx-3576980410",
  "transaction": {
    "amount": 384.88,
    "installments": 3,
    "requested_at": "2026-03-11T20:23:35Z"
  },
  "customer": {
    "avg_amount": 769.76,
    "tx_count_24h": 3,
    "known_merchants": ["MERC-009", "MERC-001"]
  },
  "merchant": {
    "id": "MERC-001",
    "mcc": "5912",
    "avg_amount": 298.95
  },
  "terminal": {
    "is_online": false,
    "card_present": true,
    "km_from_home": 13.7
  },
  "last_transaction": {
    "timestamp": "2026-03-11T14:58:35Z",
    "km_from_current": 18.86
  }
}
```

**Response**

```json
{
  "approved": true,
  "fraud_score": 0.0
}
```

## Comandos disponíveis

```
  dev             Sobe o ambiente de desenvolvimento com hot reload
  dev-debug       Sobe o ambiente de desenvolvimento sem hot reload (pronto para Delve)
  down            Derruba o ambiente de desenvolvimento
  down-v          Derruba o ambiente de desenvolvimento e remove volumes
  logs            Exibe os logs do ambiente de desenvolvimento
  rebuild         Reconstrói as imagens e sobe o ambiente de desenvolvimento
  prod            Sobe o ambiente de produção
  prod-down       Derruba o ambiente de produção
  build           Compila o binário localmente
  run             Compila e executa localmente (PORT=8080)
  test            Executa os testes
  vet             Executa go vet
  help            Exibe esta mensagem de ajuda
```

> Execute `make help` para ver todos os comandos com descrições.

## Desenvolvimento

### Pré-requisitos

- [Docker](https://docs.docker.com/get-docker/) e Docker Compose
- [Go 1.24+](https://go.dev/dl/) (apenas para build e execução local)

### Subindo o ambiente dev

```bash
make dev
```

O [Air](https://github.com/air-verse/air) monitora alterações nos arquivos `.go` e recompila automaticamente. A API fica disponível em `http://localhost:9999`.

### Derrubando o ambiente dev

```bash
make down        # para os containers
make down-v      # para os containers e remove os volumes de cache do Go
```

### Reconstruindo as imagens

Necessário após alterar `Dockerfile.dev`, `go.mod` ou dependências:

```bash
make rebuild
```

## Debug remoto com VS Code

O ambiente de debug desabilita o hot reload para que o processo não seja reiniciado durante a sessão do Delve.

**1. Suba o ambiente em modo debug:**

```bash
make dev-debug
```

**2. Aguarde o build inicial** — o Air compila o binário uma vez e mantém o processo rodando sem observar arquivos.

**3. Inicie o debug no VS Code:**

- Acesse **Run and Debug** (`Ctrl+Shift+D`)
- Selecione **Delve into Docker**
- Pressione `F5`

O VS Code executa automaticamente a task `attach-delve`, que roda `dlv attach` dentro do container `rinha-api1` na porta `2345`, e conecta o debugger ao processo em execução.

### Como funciona internamente

| Componente | Detalhe |
|---|---|
| `air.debug.toml` | Igual ao `air.toml`, mas com `include_ext = []` — Air compila uma vez e não observa mudanças |
| `AIR_CONFIG` | Variável de ambiente lida pelo entrypoint do container (`air -c ${AIR_CONFIG:-air.toml}`) |
| `cap_add: SYS_PTRACE` | Permite que o Delve use `ptrace` para se anexar ao processo dentro do container |
| `security_opt: seccomp:unconfined` | Remove o perfil seccomp padrão do Docker que bloqueia syscalls de ptrace |
| `preLaunchTask` | Executa `nohup dlv attach <PID>` no container e aguarda 1s para o Delve estar pronto |
| `postDebugTask` | Mata o processo `dlv` no container ao encerrar a sessão |
| `substitutePath` | Mapeia `/home/.../rinha_backend_2026/` → `/app/` para o VS Code encontrar os arquivos fonte |

### Versões do Delve

O Delve instalado no container (`Dockerfile.dev`) deve ter a mesma versão do Delve local usado pelo VS Code. Versão atual: **v1.25.2**.

Para verificar a versão local:

```bash
dlv version
```

## Produção

```bash
make prod
```

A imagem de produção usa um build multi-stage: compila o binário com `golang:1.24-alpine` e copia apenas o executável para uma imagem `alpine:3.20` mínima. O servidor roda como usuário não-root.

```bash
make prod-down   # derruba o ambiente de produção
```
