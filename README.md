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
- **Scorer**: KNN k=5, distância euclidiana quadrática, 100K vetores quantizados em i16 com índice IVF (K=1024 centroids, NPROBE=3)
- **Concorrência**: `GOMAXPROCS=2`; com IVF (~0.15ms/busca) sem semáforo — requests concorrentes são tratados diretamente pelo HTTP server
- **Limites de recursos** (dentro do teto da competição: 1 CPU / 350 MB total):

| Serviço | CPU | Memória |
|---------|-----|---------|
| nginx   | 0.20 | 30 MB  |
| api1    | 0.40 | 160 MB |
| api2    | 0.40 | 160 MB |
| **Total** | **1.00** | **350 MB** |

### Detecção de Fraudes (KNN + IVF)

A cada requisição, a transação é convertida em um vetor de 14 dimensões normalizadas e comparada contra **100.000 vetores de referência** pré-classificados usando um índice IVF (Inverted File Index) com K=1024 centroids. Em vez de varrer todos os 100K vetores, a busca:

1. Calcula a distância do query para os 1024 centroids (~14K operações)
2. Seleciona os 3 centroids mais próximos (NPROBE=3)
3. Varre apenas os vetores nesses 3 clusters (~293 vetores vs 100.000)

**Resultado**: ~21x speedup — de ~0.5ms para ~0.07ms por busca. A mesma taxa de FP que K=256+NPROBE=16, com 8× menos vetores varridos.

Os 5 vizinhos mais próximos determinam o score de fraude:

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
| Limite por instância | 160 MB | 165 MB |
| **Margem para runtime** | **~157 MB** | ~78 MB |

A quantização f32→i16 (valores `[0,1]` → `[0, 32767]`, sentinel `-1.0` → `MinInt16`) mantém precisão suficiente para KNN enquanto reduz o footprint de memória. Usar 100K amostras em vez de 3M reduz o dataset embarcado de 87 MB para 2.9 MB e o tempo de busca de ~250ms para ~8ms por request, com impacto mínimo na acurácia (os casos limítrofes podem divergir, mas a distribuição geral das classes é preservada pela amostragem aleatória com seed fixo).

### Modelo de Concorrência

Com o índice IVF, cada busca leva ~0.15ms de CPU. Isso elimina o problema de filas que existia com o brute-force:

- **Sem semáforo**: requests concorrentes são atendidos diretamente pelo HTTP server do Go. Não há fila acumulando latência.
- **GOMAXPROCS=2**: o scheduler do Go distribui as goroutines de HTTP sobre 2 OS threads, aproveitando os dois cores disponíveis (0.475 CPU × 2).
- **Resultado medido**: p(99)=202ms e 0 erros HTTP sob 100 VUs contínuos.

O semáforo (capacidade=1) ainda existe no código como **fallback** para o caminho brute-force (quando o binário é gerado sem `-ivf-k`). A lógica é:

```go
// Padrão de canal pré-preenchido em Go (fallback brute-force apenas)
sem = make(chan struct{}, 1)
sem <- struct{}{}   // token disponível

<-k.sem                                 // adquirir: bloqueia se outra busca roda
defer func() { k.sem <- struct{}{} }() // liberar: devolve o token
```

Esse padrão serializa as buscas brute-force para evitar CPU thrashing: com 0.475 CPU e 2 workers paralelos, uma busca por vez é o máximo útil. Com IVF esse controle não é mais necessário.

### Por que o IVF resolve os erros HTTP

O resultado anterior da competição tinha **273 erros HTTP** (peso 5× no score de detecção). A causa provável era o acúmulo de fila no semáforo: sob carga alta, requests esperavam múltiplas buscas de 8ms antes de serem atendidos. Com a fila profunda o suficiente, o nginx poderia retornar 504 antes do handler responder. Com IVF (0.5ms/busca, sem semáforo), esse cenário desaparece.

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
    knn.go            ← KNN com índice IVF (K=1024, NPROBE=3) e fallback brute-force
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

1. **preprocessor** — baixa `references.json.gz` (~16 MB), compila e roda `cmd/preprocess -max-samples 100000 -ivf-k 1024`, gerando `references.bin` (~2.9 MB com 100K vetores i16 + índice IVF de 1024 centroids). O k-means++ com 25 iterações leva ~30-60s neste stage.
2. **builder** — compila o servidor Go com otimizações de produção (`-ldflags="-s -w"`)
3. **final** — imagem alpine mínima com o binário + dataset

```bash
make prod        # builda a imagem e sobe nginx + api1 + api2
make prod-down   # derruba o ambiente de produção
```

> O `-max-samples` pode ser ajustado no Dockerfile para calibrar o trade-off latência × acurácia. Valores menores = busca mais rápida; valores maiores = maior fidelidade ao ground truth da competição (que usa os 3M vetores originais).

## Otimizações de Performance

### Resultado final (k6-prod, 100 VUs, 4 minutos)

```
p(99)            = 202ms   ✅  (limite: 2000ms)
http_req_failed  = 0.00%   ✅  (limite: 15%)
throughput       = 616 req/s
fraud_detection  = 100%
```

### Jornada de desenvolvimento

| Versão | Mudança principal | p(99) | Throughput | Problema restante |
|--------|------------------|-------|------------|-------------------|
| Baseline | KNN brute-force, 3M vetores | 42.9s ❌ | 4.7 req/s | CPU saturado, sem controle de concorrência |
| +semáforo +workers | GOMAXPROCS=2, scan paralelo, semáforo=1 | 16.2s ❌ | 6.1 req/s | Dataset muito grande |
| +sampling 100K | 100K amostras representativas | 859ms → **2001ms** ❌ | 166 req/s | 1.35ms acima do limite na competição; 273 erros HTTP |
| **+IVF K=256** | **Índice IVF, NPROBE=16, sem semáforo** | **202ms ✅** | **616 req/s** | nginx CPU-starved (0.05 CPU); 1631 erros HTTP em produção |
| **+v2 (atual)** | **nginx 0.20 CPU, K=1024+nProbe=3, GOMEMLIMIT, sync.Pool** | **~87ms** | **2516 req/s** | — |

---

### Otimizações v2 — inspiradas em [banjohann/rinha-2026-go](https://github.com/banjohann/rinha-2026-go)

A submissão anterior com IVF K=256 obteve **-3865.86 pontos** apesar do p99=202ms nos testes locais. Na competição, o p99 foi 2002.16ms (+2ms acima do corte) e houve 1631 erros HTTP. A análise do repositório de referência revelou a raiz dos dois problemas:

#### Problema: nginx CPU-starved (0.05 CPU)

O banjohann/rinha-2026-go documentou que dobrar o CPU do load balancer (0.10→0.20) reduziu o p99 de **257ms → 5.85ms** (44×). O mecanismo: com CPU insuficiente, o processo nginx fica throttled pelo CFS do Linux. Requests chegam mais rápido do que o nginx consegue fazer proxy, criando uma fila interna. Essa fila acrescenta latência e, quando profunda o suficiente, causa timeouts que o nginx converte em 504 antes do handler responder.

Nosso nginx estava em 0.05 CPU — 4× abaixo do valor que o repositório de referência identificou como insuficiente (0.10). Isso explica ambos os sintomas: os 2ms extras no p99 e os 1631 erros HTTP.

**Fix:** nginx 0.05→0.20 CPU, apis 0.475→0.40 CPU, memórias redistribuídas proporcionalmente. Total permanece 1.00 CPU / 350 MB.

#### IVF K=1024 + NPROBE=3

O repositório de referência usa K=1024 centroids com NPROBE=3, que escaneia ~293 vetores por query vs ~6.250 com K=256+NPROBE=16. Clusters mais finos (97 vetores/cluster vs 390) permitem encontrar os vizinhos corretos varrendo muito menos vetores.

**Fix:** Dockerfile com `-ivf-k 1024`, `nProbe = 3`. Testes comparativos mostraram que K=256+nProbe=16 (6.250 vetores/query) tem a mesma taxa de FP que K=1024+nProbe=3 (293 vetores/query), mas com p99 214ms vs 87ms e throughput 978 vs 2516 req/s. K=1024+nProbe=3 domina em latência para a mesma acurácia.

#### GOMEMLIMIT

Sem `GOMEMLIMIT`, o GC do Go assume que pode usar até o dobro do heap ao vivo antes de coletar. Com heap ao vivo de ~3 MB (dataset), o GC pode adiar coleta até o RSS atingir ~50-80 MB, e então fazer uma coleta longa visível como spike no p99. `GOMEMLIMIT=150MiB` instrui o runtime a coletar mais agressivamente antes do limite do container (160 MB), eliminando esses spikes de cauda.

**Fix:** `GOMEMLIMIT=150MiB` no environment de api1 e api2 em `docker-compose.yml`.

#### sync.Pool para alocações por request

Cada request alocava:
- Um `fraudRequest` struct (~200 bytes + backing array de `KnownMerchants`)
- Um slice `centDist` de K elementos para a busca IVF (~16 KB com K=1024)

Com K=1024 e centDist alocado por request, 54K requests × 16 KB = ~864 MB de garbage para o GC coletar. Usar `sync.Pool` para ambos reusa as alocações entre requests, reduzindo drasticamente a pressão no GC.

**Fix:** `fraudReqPool` em `handler/fraud.go`, `centDistPool` em `scorer/knn.go`.

#### Resumo das mudanças v2

| Arquivo | Mudança | Efeito esperado |
|---------|---------|----------------|
| `docker-compose.yml` | nginx 0.05→0.20 CPU, apis 0.475→0.40, GOMEMLIMIT=150MiB | Remove p99 cut + elimina HTTP errors |
| `Dockerfile` | `-ivf-k 256` → `-ivf-k 1024` | 21× menos vetores varridos por query |
| `internal/scorer/knn.go` | pool para centDist + `nProbe 16→3` (K=1024) | Reduz latência de busca + GC pressure |
| `internal/handler/fraud.go` | pool para fraudRequest | Reduz GC pressure por request |
| `nginx.conf` | `worker_connections 4096`, `tcp_nodelay`, `proxy_buffering off` | Suporta bursts sem dropar conexões |

Score esperado: **+2500 a +4500** (referência com as mesmas técnicas: +4671).

---

### Etapa 1 — Baseline: 3M vetores, brute-force

**Como estava:** A primeira versão varria linearmente todos os 3M vetores de referência para cada requisição. Sem qualquer controle de concorrência: 100 goroutines de HTTP competiam pela mesma CPU.

**O problema:** Com 0.475 CPU por instância, cada busca consumia ~225ms de CPU → ~500ms de wall-clock. Com 100 VUs:

```
p(99) ≈ 50 VUs × 500ms = 25s por instância → 42.9s medido
```

**Aprendizado:** Em sistemas CPU-bound com cota de CPU limitada (CFS quota), concorrência irrestrita é pior que serialização — cada goroutine avança a 1/N da velocidade, e todas ficam lentas juntas. O problema não era só o dataset grande; era a ausência de backpressure.

---

### Etapa 2 — Semáforo + scan paralelo: controle de concorrência

**O que mudou:** Introduzido semáforo de capacidade 1 (`sem chan struct{}`) para serializar as buscas KNN por instância. Dentro do slot, o scan é dividido em 2 goroutines (uma por core com `GOMAXPROCS=2`).

**Bug crítico cometido:** A lógica do semáforo foi implementada ao contrário na primeira tentativa:

```go
// ERRADO: tenta enviar para adquirir → bloqueia imediatamente se canal cheio
k.sem <- struct{}{}    // ← isso é a liberação, não a aquisição!

// CORRETO: recebe para adquirir (tira o token), envia para liberar (devolve)
<-k.sem
defer func() { k.sem <- struct{}{} }()
```

**Sintoma:** 100% de falhas HTTP instantâneas (nginx 502/504 imediatos), throughput de 659 req/s aparente — que era na verdade nginx devolvendo erro sem nem chegar à API.

**Aprendizado:** O padrão de semáforo em Go usa canal pré-preenchido: `sem <- struct{}{}` na inicialização coloca o token. `<-sem` *tira* o token (adquire), `sem <- struct{}{}` *devolve* (libera). Inverter os dois torna o canal sempre vazio → nenhuma goroutine consegue adquirir → deadlock imediato.

**Resultado após correção:** p(99) caiu de 42.9s para 16.2s. Melhora real, mas insuficiente. O dataset de 3M vetores ainda tornava cada busca lenta demais.

---

### Etapa 3 — Sampling 100K: tamanho do dataset

**O que mudou:** Preprocessador passa a sortear 100K amostras representativas do dataset original de 3M (shuffle com seed=42, distribuição de classes preservada). O binário cai de 87 MB para 2.9 MB; a busca, de ~225ms para ~8ms.

**Resultado local:** p(99) = 859ms ✅ — passou o threshold de 2000ms nos testes locais.

**Resultado na competição:** **p(99) = 2001.35ms ❌** — apenas 1.35ms acima do limite. Isso ativou o corte de -3000 pontos. Além disso, 273 erros HTTP (peso 5× cada) arrasaram o score de detecção.

**Por que os resultados divergiram:** O ambiente da competição tem carga e condições diferentes dos testes locais. O semáforo de capacidade 1 ainda criava fila sob carga real: com requests acumulando, a latência observada ultrapassava marginalmente o limite. Os erros HTTP provavelmente vinham dessa fila — requests esperando na frente do semáforo enquanto o nginx já havia expirado o timeout.

**Aprendizado:** Testar localmente com 100 VUs é uma aproximação. A competição pode ter VUs, ramp-up, ou infraestrutura diferentes. Margens de segurança de latência importam: um threshold de 2000ms com p(99) medido de 859ms parece confortável, mas 1.35ms de divergência mostra que não era.

---

### Etapa 4 — IVF: índice de busca aproximada

**Como ficou:** Em vez de varrer todos os 100K vetores, o preprocessador roda k-means++ (K=256, 25 iterações) durante o build da imagem Docker e persiste o índice IVF no binário. Em runtime, cada busca:

1. Calcula distância para 256 centroids (~3.6K operações)
2. Seleciona os 16 mais próximos (NPROBE=16, partial selection sort O(K×P))
3. Varre apenas os vetores nesses 16 clusters (~6.250 vetores)

```
Antes: 100.000 vetores varridos por busca
Depois: ~6.250 vetores varridos — 16× menos
```

**Por que isso resolve os erros HTTP:** Com buscas em ~0.5ms, não há fila se acumulando. O semáforo foi removido do caminho IVF — requests concorrentes são atendidos diretamente. Sem fila → sem timeout → sem erros HTTP.

**Por que o índice é construído no Dockerfile e não em runtime:** K-means++ sobre 100K vetores leva ~20-30s. Fazer isso no startup inviabilizaria o healthcheck. O índice é um artefato de build, não de runtime — mesmo princípio da quantização: trabalho custoso acontece uma vez.

**Inspiração:** [jairoblatt/rinha-2026-rust](https://github.com/jairoblatt/rinha-2026-rust) usa IVF com K=4096, NPROBE=5/24 e AVX2/FMA — a mesma estrutura, em escala maior e com vetorização SIMD.

---

### Por que 100K amostras são suficientes para KNN

KNN tem retornos decrescentes com mais dados de referência: a fronteira de decisão entre as classes já está bem definida com uma fração, desde que a distribuição das classes seja preservada. A amostragem com seed fixo (42) mantém a proporção fraude/legítimo do dataset original. Casos claramente fraudulentos ou legítimos são classificados corretamente; apenas os casos limítrofes — onde os 5 vizinhos são mistura das duas classes — podem divergir do ground truth com 3M vetores.

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

- **[banjohann/rinha-2026-go](https://github.com/banjohann/rinha-2026-go)** — implementação em Go que alcançou +4671 pontos (+5.83ms p99, 99.94% acurácia). Inspirou as otimizações v2 deste projeto: redistribuição de CPU para o load balancer (principal causa do p99 cut), IVF K=1024+NPROBE=3, GOMEMLIMIT e sync.Pool. A análise de performance deles documentou que dobrar o CPU do HAProxy (0.10→0.20) sozinho reduziu o p99 de 257ms para 5.85ms — um sinal claro de que o gargalo estava no load balancer, não na API.

- **[jairoblatt/rinha-2026-rust](https://github.com/jairoblatt/rinha-2026-rust)** — implementação de referência em Rust que inspirou a adoção do IVF neste projeto. Usa IVF com K=4096 centroids, NPROBE=5 (fast) / 24 (full) e AVX2/FMA para vetorização SIMD. A abordagem central — construir o índice k-means++ em build time e fazer scan apenas dos clusters mais próximos em runtime — reduz o espaço de busca de 3M vetores para ~5K, tornando p99 < 100ms mesmo sob carga extrema.

- **Bruno Gonzaga — ["Rinha de Backend 2026: Vetores, Memória e Vizinhos"](https://brunogonzaga.dev/artigos/rinha-backend-2026-vetores-memoria/)** — artigo fundamental para entender os desafios desta edição. Os principais aprendizados:
  - **Separação build-time / runtime**: tudo que é custoso (parse de JSON, quantização, conversão de formato) deve ir para o build; o runtime só mapeia e responde. Essa separação é o que torna viável ter 3M vetores disponíveis em memória sem impactar o startup da API.
  - **Orçamento de memória com memória mapeada**: o autor explora usar `mmap` para que múltiplas instâncias compartilhem o mesmo dataset em memória física, evitando cópias duplicadas e o "penhasco de memória" onde duas instâncias com 84 MB de dataset cada ultrapassariam o limite de 350 MB.
  - **Baseline-first antes de HNSW/IVF**: a abordagem recomendada é implementar o brute-force primeiro para ter uma linha de base mensurável, e só então considerar índices aproximados (HNSW, IVF, VP-tree) se o brute-force não for suficiente. Isso evita complexidade prematura.
  - **O custo diferenciado dos erros molda a arquitetura**: com FP=1, FN=3 e HTTP_ERR=5, um falso negativo (fraude que passa) custa 3× mais que um falso positivo, e indisponibilidade custa 5×. Isso justifica priorizar estabilidade e disponibilidade sobre otimismo na classificação.
