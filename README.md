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
         │     api1       │                   │     api2       │
         │  (Go/stdlib)   │                   │  (Go/stdlib)   │
         │  KNN in-mem    │                   │  KNN in-mem    │
         └────────────────┘                   └────────────────┘
              ▲                                      ▲
              │ /run/sockets/api1.sock               │ /run/sockets/api2.sock
              └──────────────────────────────────────┘
                         (Unix Domain Sockets)
```

- **Load balancer**: nginx com round-robin simples, sem lógica de negócio
- **Comunicação nginx ↔ API**: Unix Domain Sockets via volume compartilhado `/run/sockets`
- **API**: Go 1.24, stdlib pura, zero dependências externas
- **Scorer**: KNN k=5, distância euclidiana, 3M vetores quantizados em i16
- **Limites de recursos**: 1 CPU / 350 MB total (nginx 0.1/20MB, cada instância 0.45/165MB)

### Detecção de Fraudes (KNN)

A cada requisição, a transação é convertida em um vetor de 14 dimensões normalizadas e comparada contra 3.000.000 vetores de referência pré-classificados. Os 5 vizinhos mais próximos determinam o score de fraude:

```
fraud_score = número_de_vizinhos_fraud / 5
approved    = fraud_score < 0.6
```

#### As 14 dimensões do vetor

| # | Campo | Normalização | Sentinel |
|---|-------|-------------|---------|
| 0 | `transaction.amount` | ÷ 10 000, clamp[0,1] | — |
| 1 | `transaction.installments` | ÷ 12, clamp[0,1] | — |
| 2 | `amount / customer.avg_amount` | ÷ 10, clamp[0,1] | 0 se avg=0 |
| 3 | hora do dia | ÷ 23 | — |
| 4 | dia da semana | ÷ 6 (Mon=0…Sun=6) | — |
| 5 | minutos desde última tx | ÷ 1440 | **-1** se sem histórico |
| 6 | `last_transaction.km_from_current` | ÷ 1000, clamp[0,1] | **-1** se sem histórico |
| 7 | `terminal.km_from_home` | ÷ 1000, clamp[0,1] | — |
| 8 | `customer.tx_count_24h` | ÷ 20, clamp[0,1] | — |
| 9 | `terminal.is_online` | 1.0 / 0.0 | — |
| 10 | `terminal.card_present` | 1.0 / 0.0 | — |
| 11 | merchant desconhecido | 1.0 se não está em `known_merchants` | — |
| 12 | `mcc_risk[merchant.mcc]` | lookup, default 0.5 | — |
| 13 | `merchant.avg_amount` | ÷ 10 000, clamp[0,1] | — |

#### Orçamento de memória

| Item | Cálculo | Tamanho |
|------|---------|---------|
| Vetores i16 | 3M × 14 × 2 bytes | 84 MB |
| Labels | 3M × 1 byte | 3 MB |
| **Total dataset** | | **~87 MB** |
| Limite por instância | | 165 MB |
| **Margem para runtime** | | ~78 MB |

A quantização f32→i16 (valores `[0,1]` → `[0, 32767]`, sentinel `-1` → `MinInt16`) reduz 168 MB para 84 MB por instância, cabendo dentro do limite.

### Unix Domain Sockets

A comunicação entre nginx e as instâncias da API usa **Unix Domain Sockets (UDS)** em vez de TCP.

| | TCP | Unix Domain Socket |
|---|---|---|
| Caminho no kernel | Pilha TCP/IP completa | VFS direto |
| Namespace de rede | Sim | Não |
| Latência relativa | Base | ~30-40% menor |

### Estrutura de pastas

```
cmd/
  server/
    main.go           ← entrypoint: inicializa KNN, escuta em UDS ou TCP
  preprocess/
    main.go           ← ferramenta de build: converte references.json.gz → references.bin (i16)
internal/
  domain/
    fraud.go          ← entidades (FraudInput, FraudResult) e interface FraudScorer
  usecase/
    fraud_score.go    ← ScoreFraud: orquestra a chamada ao scorer
  scorer/
    vectorize.go      ← FraudInput → [14]float64 (normalização dos 14 dims)
    knn.go            ← KNN brute-force sobre dataset i16 em memória
    noop.go           ← implementação placeholder (aprova tudo)
  handler/
    router.go         ← monta o http.ServeMux com as rotas
    fraud.go          ← POST /fraud-score: decode, mapeia para domain, chama usecase
    ready.go          ← GET /ready: healthcheck
test/
  k6/
    scenario.js       ← cenário de carga k6 com mix de transações legítimas e fraudes
```

## Endpoints

### `GET /ready`

Healthcheck. Retorna `200 OK` quando a instância está pronta (dataset carregado).

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
  "fraud_score": 0.2
}
```

## Build Docker

O Dockerfile usa 3 stages:

1. **preprocessor** — baixa `references.json.gz` (~16 MB) do repositório oficial, compila e roda `cmd/preprocess`, gerando `references.bin` (~87 MB em i16)
2. **builder** — compila o servidor Go com otimizações de produção
3. **final** — imagem alpine mínima com o binário + dataset

```bash
make prod        # builda a imagem e sobe nginx + api1 + api2
make prod-down   # derruba o ambiente de produção
```

> O build inclui download de ~16 MB do dataset. Em ambientes offline, o dataset pode ser gerado localmente: `make preprocess`.

## Testes de Carga (k6)

### Pré-requisito

```bash
# macOS
brew install k6

# Linux
sudo apt install k6  # ou via snap/flatpak
```

### Executar cenário

```bash
# API deve estar rodando em localhost:9999
make prod

# Cenário padrão (ramp-up 20→50→100 VUs, 4 minutos total)
k6 run test/k6/scenario.js

# Apontando para outro host
k6 run -e BASE_URL=http://api.example.com test/k6/scenario.js
```

### Métricas customizadas

| Métrica | Descrição |
|---------|-----------|
| `approved_legit` | Transações legítimas aprovadas corretamente |
| `blocked_fraud` | Fraudes bloqueadas corretamente |
| `false_positives` | Legítimas bloqueadas (custo: -1 por ocorrência) |
| `false_negatives` | Fraudes aprovadas (custo: -3 por ocorrência) |
| `fraud_detection_rate` | Taxa de acerto na detecção de fraudes |

### Thresholds configurados

```
http_req_duration p(99) < 2000ms   # evita penalidade -3000 por latência
http_req_failed   rate   < 15%     # evita penalidade -3000 por disponibilidade
```

### Verificar consumo de memória

```bash
docker stats --no-stream
```
Esperado: api1 e api2 abaixo de 165 MB cada.

## Scoring da Competição

```
score_final = score_p99 + score_det   (range: -6000 a +6000)
```

| Componente | Condição | Pontos |
|-----------|---------|--------|
| `score_p99` | p99 ≤ 1ms | +3000 |
| `score_p99` | p99 > 2000ms | -3000 |
| `score_det` | failure_rate > 15% | -3000 |
| `score_det` | Pesos: HTTP_ERR=5, FN=3, FP=1 | proporcional |

## Submissão

A branch `submission` deve conter apenas:
- `docker-compose.yml`
- `nginx.conf`
- `info.json`

```bash
# Criar branch de submissão (executar após validar prod)
git checkout -b submission
git rm -r --cached .
git add docker-compose.yaml nginx.conf info.json
git commit -m "feat: add submission files"
git push origin submission
```

O `info.json` na raiz do `main` e na branch `submission` deve ser criado como `./participants/IgorBrizack.json` no repositório da competição via PR.

## Comandos disponíveis

```
  dev             Sobe o ambiente de desenvolvimento com hot reload
  dev-debug       Sobe o ambiente de desenvolvimento sem hot reload (Delve)
  down            Derruba o ambiente de desenvolvimento
  down-v          Derruba containers e remove volumes de cache
  logs            Exibe os logs do ambiente de desenvolvimento
  rebuild         Reconstrói as imagens
  prod            Sobe o ambiente de produção (inclui build do dataset)
  prod-down       Derruba o ambiente de produção
  build           Compila o binário localmente
  run             Compila e executa localmente (PORT=8080)
  test            Executa os testes Go
  vet             Executa go vet
  help            Exibe esta mensagem de ajuda
```

## Desenvolvimento

### Pré-requisitos

- [Docker](https://docs.docker.com/get-docker/) e Docker Compose
- [Go 1.24+](https://go.dev/dl/)

### Subindo o ambiente dev

```bash
make dev
```

O [Air](https://github.com/air-verse/air) monitora alterações nos arquivos `.go` e recompila automaticamente. A API fica disponível em `http://localhost:9999`.

> No ambiente de dev, as variáveis `DATASET_PATH` e `MCC_RISK_PATH` devem apontar para um dataset local. Se os arquivos não existirem, o servidor falhará no startup com uma mensagem de erro clara.

### Debug remoto com VS Code

```bash
make dev-debug
```

- Acesse **Run and Debug** (`Ctrl+Shift+D`), selecione **Delve into Docker**, pressione `F5`
- O VS Code executa a task `attach-delve` que roda `dlv attach` no container `rinha-api1` na porta `2345`

## Produção

O build multi-stage:
1. `preprocessor`: baixa dataset (~16 MB), converte para binário i16 (~87 MB)
2. `builder`: compila o servidor Go
3. `final`: alpine mínima com binário + dataset embarcado

```bash
make prod      # builda e sobe nginx + 2 instâncias da API
make prod-down # derruba
```
