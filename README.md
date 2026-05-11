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
- **Scorer**: KNN k=5, distância euclidiana quadrática, 100K vetores quantizados em i16
- **Concorrência**: `GOMAXPROCS=2` + semáforo (cap=1) por instância + scan paralelo com 2 workers
- **Limites de recursos** (dentro do teto da competição: 1 CPU / 350 MB total):

| Serviço | CPU | Memória |
|---------|-----|---------|
| nginx   | 0.05 | 10 MB  |
| api1    | 0.475 | 170 MB |
| api2    | 0.475 | 170 MB |
| **Total** | **1.00** | **350 MB** |

### Detecção de Fraudes (KNN)

A cada requisição, a transação é convertida em um vetor de 14 dimensões normalizadas e comparada contra **100.000 vetores de referência** pré-classificados (amostra representativa do dataset original de 3M). Os 5 vizinhos mais próximos determinam o score de fraude:

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

| Item | 100K vetores | 3M vetores (original) |
|------|-------------|----------------------|
| Vetores i16 | **2.8 MB** | 84 MB |
| Labels | **0.1 MB** | 3 MB |
| **Total dataset** | **~2.9 MB** | ~87 MB |
| Limite por instância | 170 MB | 165 MB |
| **Margem para runtime** | **~167 MB** | ~78 MB |

A quantização f32→i16 (valores `[0,1]` → `[0, 32767]`, sentinel `-1.0` → `MinInt16`) mantém precisão suficiente para KNN enquanto reduz o footprint de memória. Usar 100K amostras em vez de 3M reduz o dataset embarcado de 87 MB para 2.9 MB e o tempo de busca de ~250ms para ~8ms por request, com impacto mínimo na acurácia (os casos limítrofes podem divergir, mas a distribuição geral das classes é preservada pela amostragem aleatória com seed fixo).

### Modelo de Concorrência

O gargalo do KNN é CPU-bound: cada busca varre N vetores sequencialmente. Sob alta concorrência sem controle, 100 goroutines competem pelo mesmo 0.475 CPU e cada uma avança a 1/100 da velocidade, tornando p(99) proporcional ao tamanho da fila.

A solução tem duas camadas:

**1. Semáforo por instância (capacidade=1)** — serializa as buscas KNN. Apenas uma busca roda de cada vez; as demais ficam bloqueadas no canal sem consumir CPU. Padrão de canal pré-preenchido em Go:

```go
// inicialização — token disponível
sem = make(chan struct{}, 1)
sem <- struct{}{}

// adquirir (Score) — bloqueia se outra busca estiver rodando
<-k.sem
defer func() { k.sem <- struct{}{} }()
```

**2. Scan paralelo com `nWorkers` goroutines** — dentro do slot adquirido, o dataset é dividido em chunks e cada goroutine busca seu chunk. Os heaps locais são mergeados ao final:

```go
// cada goroutine busca [start, end) e retorna seu top-k local
results[w].heap, results[w].count = k.searchRange(qi16, start, end)
// merge: iterar todos os resultados e manter o top-k global
```

Com `GOMAXPROCS=2`, as duas goroutines de busca podem rodar em paralelo, reduzindo o wall-clock por request.

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
    main.go           ← entrypoint: GOMAXPROCS=2, inicializa KNN, escuta em UDS ou TCP
  preprocess/
    main.go           ← ferramenta de build: references.json.gz → references.bin (i16, -max-samples)
internal/
  domain/
    fraud.go          ← entidades (FraudInput, FraudResult) e interface FraudScorer
  usecase/
    fraud_score.go    ← ScoreFraud: orquestra a chamada ao scorer
  scorer/
    vectorize.go      ← FraudInput → [14]float64 (normalização dos 14 dims)
    knn.go            ← KNN com semáforo, scan paralelo e merge de heaps
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

1. **preprocessor** — baixa `references.json.gz` (~16 MB) do repositório oficial, compila e roda `cmd/preprocess -max-samples 100000`, gerando `references.bin` (~2.9 MB com 100K vetores i16)
2. **builder** — compila o servidor Go com otimizações de produção (`-ldflags="-s -w"`)
3. **final** — imagem alpine mínima com o binário + dataset

```bash
make prod        # builda a imagem e sobe nginx + api1 + api2
make prod-down   # derruba o ambiente de produção
```

> O `-max-samples` pode ser ajustado no Dockerfile para calibrar o trade-off latência × acurácia. Valores menores = busca mais rápida; valores maiores = maior fidelidade ao ground truth da competição (que usa os 3M vetores originais).

## Otimizações de Performance

Jornada de tuning medida com o cenário k6 local (100 VUs, sem sleep — carga extrema):

| Versão | Mudança principal | p(99) | Throughput |
|--------|------------------|-------|------------|
| Baseline | KNN brute-force, 3M vetores | 42.9s ❌ | 4.7 req/s |
| +semáforo +workers | GOMAXPROCS=2, scan paralelo, semáforo=1 | 16.2s ❌ | 6.1 req/s |
| **+sampling 100K** | **100K amostras representativas** | **859ms ✅** | **166 req/s** |

### Por que o baseline era tão lento

Com 3M vetores e 0.475 CPU, cada busca KNN consumia ~225ms de CPU → ~500ms de wall-clock. Sem controle de concorrência, 100 goroutines competiam pelo mesmo CPU: cada uma avançava a 1/100 da velocidade, tornando p(99) ≈ 100 × 500ms / 2 instâncias ≈ 25s.

### Por que 100K amostras são suficientes para KNN

KNN com muitos pontos de referência tem retornos decrescentes: a fronteira de decisão entre classes já está bem definida com uma fração dos dados, desde que a distribuição das classes seja preservada. A amostragem aleatória com seed fixo (42) mantém a proporção legítima/fraude do dataset original. Casos claramente fraudulentos ou legítimos são classificados corretamente; apenas casos limítrofes — onde os 5 vizinhos são uma mistura — podem divergir do ground truth com 3M vetores.

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
make prod        # garante que o ambiente está rodando com o build mais recente

make k6-prod     # roda contra http://localhost:9999 (atalho)
make k6          # equivalente; BASE_URL pode ser sobrescrito:
BASE_URL=http://api.example.com make k6
```

### Métricas customizadas

| Métrica | Descrição |
|---------|-----------|
| `approved_legit` | Transações legítimas aprovadas corretamente |
| `blocked_fraud` | Fraudes bloqueadas corretamente |
| `false_positives` | Legítimas bloqueadas incorretamente (custo: FP=1) |
| `false_negatives` | Fraudes aprovadas incorretamente (custo: FN=3) |
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

Esperado: api1 e api2 abaixo de 170 MB cada.

## Scoring da Competição

```
score_final = score_p99 + score_det   (range: -6000 a +6000)
```

**Latência (p99):**

```
score_p99 = 1000 × log₁₀(1000ms / max(p99, 1ms))   se p99 ≤ 2000ms
score_p99 = -3000                                    se p99 > 2000ms
```

Cada redução de 10× na latência vale +1000 pontos. Exemplos:

| p99 | score_p99 |
|-----|-----------|
| 1ms | +3000 |
| 10ms | +2000 |
| 100ms | +1000 |
| 500ms | +301 |
| 2000ms | 0 |
| > 2000ms | **-3000** |

**Detecção (acurácia):**

```
E = 1×FP + 3×FN + 5×Erros_HTTP
ε = E / N   (mínimo: 0.001)
score_det = 1000×log₁₀(1/ε) − 300×log₁₀(1+E)
score_det = -3000   se failure_rate > 15%
```

Os pesos refletem o custo real: um falso negativo (fraude não detectada) é 3× mais caro que um falso positivo (transação legítima bloqueada), e um erro HTTP (serviço indisponível) é 5× mais caro.

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
  k6              Roda o cenário k6 (BASE_URL=http://localhost:9999 por padrão)
  k6-prod         Atalho: roda k6 contra o ambiente de produção local
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

## Referências

- **[Rinha de Backend 2026](https://github.com/zanfranceschi/rinha-de-backend-2026)** — repositório oficial com as regras, dataset de referência (`references.json.gz`), especificação dos 14 atributos e critérios de avaliação.

- **Bruno Gonzaga — ["Rinha de Backend 2026: Vetores, Memória e Vizinhos"](https://brunogonzaga.dev/artigos/rinha-backend-2026-vetores-memoria/)** — artigo fundamental para entender os desafios desta edição. Os principais aprendizados:
  - **Separação build-time / runtime**: tudo que é custoso (parse de JSON, quantização, conversão de formato) deve ir para o build; o runtime só mapeia e responde. Essa separação é o que torna viável ter 3M vetores disponíveis em memória sem impactar o startup da API.
  - **Orçamento de memória com memória mapeada**: o autor explora usar `mmap` para que múltiplas instâncias compartilhem o mesmo dataset em memória física, evitando cópias duplicadas e o "penhasco de memória" onde duas instâncias com 84 MB de dataset cada ultrapassariam o limite de 350 MB.
  - **Baseline-first antes de HNSW/IVF**: a abordagem recomendada é implementar o brute-force primeiro para ter uma linha de base mensurável, e só então considerar índices aproximados (HNSW, IVF, VP-tree) se o brute-force não for suficiente. Isso evita complexidade prematura.
  - **O custo diferenciado dos erros molda a arquitetura**: com FP=1, FN=3 e HTTP_ERR=5, um falso negativo (fraude que passa) custa 3× mais que um falso positivo, e indisponibilidade custa 5×. Isso justifica priorizar estabilidade e disponibilidade sobre otimismo na classificação.
