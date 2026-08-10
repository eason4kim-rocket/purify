# Purify Search — MASTERPLAN 总体作战计划

| 项 | 值 |
|---|---|
| 版本 | v1 |
| 日期 | 2026-08-09 |
| 状态 | planning — 按任务卡逐张执行 |
| 执行人 | liulin（本人写码；本文档是作战手册，不是外包说明书） |
| 分支策略 | 能力层/北极星在 `codex/scrape-v1` 系；Search 线在 `codex/search-api-v1`（见 §13） |
| 关联文档 | `SEARCH.md`（Search M0–M4 契约）、`SCRAPE-PLAN.md`、`AGENTS.md`（边界与提交纪律） |
| 战略来源 | 三层战略报告（竞品调研 + 能力设计 + 北极星 + 极限层），2026-08 |

**使用方法**：每张任务卡三段式——「完成什么 / 交付什么 / 验收标准」；下面附「实现参考」（签名、DDL、伪码、契约），照参考写，但具体实现自己定夺。每张卡对应一个（或一组）commit，做完勾验收。

---

## 0. 三层战略总览 · Three-Tier Strategy

### 0.1 阶梯

```text
第一层 能力层    带收据的 JSON            JSON you can prove
                （证据锚定/复验/编译提取/共识）        ↓ 器官
第二层 北极星    信念检索                Belief Retrieval
                （FactSpec → BeliefState，敢答"不知道"）  ↓ 编排
第三层 极限层    事实清算所              The Fact Clearinghouse
                （收据/担保/制图/审计/租约五工件）        经济工件
```

下层是上层的器官：证据与快照喂 /verify，/verify 喂校准与账本，账本喂审计与担保。**每一层的数据都让上一层的壁垒随运营时间复利。**

### 0.2 五条竞争事实（2026-08 调研核实）

1. Firecrawl v2.11 已上线 `deterministicJson`（按站点缓存可复用提取器）——但完全黑盒：不暴露提取器版本、验证分、漂移信号；其 change-tracking 官方文档自认"不提供加密验证或任何证据"。
2. Parallel Basis 是可信度赛道最强对手：字段级 citations + excerpts + 校准置信度——但证据止于 URL+文字摘录，不锚定 DOM、不存快照、无法事后复验。
3. **独立性计数（N_eff）全市场缺失**：十个镜像转载=十个"来源"。在位者有反向动机（展示 20 个来源显得可信）。
4. **公开网页的时点事实 API 空白**：Zep/Graphiti 的时序知识图谱只做 agent 自身记忆；Wayback 只存页面不做事实。
5. 数据行业标准条款是 **"No Warranty of Accuracy"**——没有任何 API 为数据错误承担财务责任。

### 0.3 五个空白点（我们的进攻面）

| # | 空白 | 对应任务 |
|---|---|---|
| 1 | 字段级证据锚定（quote+selector+偏移+快照哈希） | Phase 0 |
| 2 | 事后可复验（/verify 三态） | Phase 1 |
| 3 | 提取器透明健康度（版本/验证分/漂移/回归） | Phase 2–3 |
| 4 | 共识提取与冲突暴露（N_eff） | Phase 3 |
| 5 | BYO-URL 提取的校准置信度 | Phase 5+（数据攒够才启用） |

### 0.4 明确不做

自建搜索索引（Exa/Brave 资本战场）；自主浏览 agent（Firecrawl /agent 军备赛）；PII 脱敏等跟随特性（后补）；区块链存证（SHA-256+可回放快照足够，zkTLS 是后期互补轨道）；事实期货/质押/去中心化清算（监管沼泽）。

> **EN —** Three tiers: evidence-grade extraction (organs) → belief retrieval (orchestration) → the fact clearinghouse (economic instruments). Verified market facts: Firecrawl's deterministicJson is a black box and its change tracking admits "no verification or evidence"; Parallel's Basis stops at URL+excerpt with no re-verification; independence accounting and point-in-time web-fact APIs simply do not exist; the industry default is "No Warranty of Accuracy". We attack five whitespaces and explicitly refuse to build indexes, browsing agents, or crypto notarization.

---

## 1. 现状盘点 · Current State（已核实）

### 1.1 代码现状

7,164 行 Go，单二进制。路由（`api/router.go`）：

```text
GET  /api/v1/health                    无鉴权
POST /api/v1/scrape                    auth + ratelimit
POST /api/v1/extract                   scrape→clean→单次 LLM(json_object)
POST /api/v1/batch/scrape  GET /batch/:id
POST /api/v1/crawl         GET /crawl/:id
POST /api/v1/map
```

### 1.2 资产 → 未来能力映射

| 现有资产 | 位置 | 喂给 |
|---|---|---|
| SimHash 全家（`Fingerprint/Distance/Similar/FingerprintDOM`） | `simhash/` | 去重、模板簇、漂移检测、独立性折叠 |
| 多引擎竞速（http→rod→rod-stealth 梯次）+ 自适应页池 | `engine/` | /verify 低成本重访 |
| 清洗管线 9 文件（readability/pruning/markdown/citations/selector/tokens） | `cleaner/` | 证据对齐的文本底座 |
| goquery + cascadia（已是直接依赖） | go.mod | 编译式提取器执行，零新增浏览器依赖 |
| klauspost/compress（间接依赖，含 zstd） | go.mod | 快照压缩，转直接依赖即可 |
| araddon/dateparse（间接依赖） | go.mod | 日期 transform |
| mcp-go v0.44 HTTP 代理型 MCP（`mcp.NewTool` 注册模式） | `cmd/purify-mcp/` | verify_fact / search_web 新工具 |
| webhook 投递 | `webhook/` | fact.changed / extractor.promoted 事件 |
| OpenAI 兼容 LLM 客户端 + 公网限定 transport | `llm/` | 请求级 BYOK 提取；由独立 process credential adapter 供编译期真值提取 |

### 1.3 短板（Phase 0 逐一补掉）

- `llm/openai.go` 只有 prompt + `json_object`：schema 靠嘴约束，不校验、无 strict structured outputs、无修复重试。
- `cache/` 纯内存、容量满随机驱逐、1h TTL——**没有任何持久层**。
- extract 不存快照、无证据、无溯源字段。
- 无 SQLite/磁盘存储，无签名密钥体系。

> **EN —** 7,164 lines of Go with six endpoints. Reusable assets map directly onto the roadmap: simhash → dedup/drift/independence, the racing engine → cheap re-verification, goquery/cascadia → LLM-free extractor execution, mcp-go → new agent tools. Gaps: no schema validation, no persistence, no snapshots, no signing.

---

## 2. 目标架构与全局技术决策 · Target Architecture

### 2.1 终态包图

```text
现有: api/ cleaner/ engine/ scraper/ llm/ models/ cache/ proxy/ simhash/ webhook/ config/ cmd/
新增: snapshot/    内容寻址快照库 (CAS)
      evidence/    值→锚点对齐器
      receipts/    Ed25519 签名收据
      ledger/      SQLite 统一持久层 (migrations)
      verify/      三态复验服务
      compiler/    提取器 IR 编译与执行、模板簇、漂移、自愈
      consensus/   多源合并 + N_eff
      search/      Search 服务 + providers/ + answer (Phase 4+)
      watch/       常设 FactSpec 调度 (Phase 6)
```

### 2.2 数据流

```text
scrape ─▶ snapshot.Put(raw HTML) ─▶ clean ─▶ extract ──compiled──▶ compiler.Execute
   │                                          └──llm──▶ llm.Extract → validate → repair
   │                                                        │
   └────────────────────────────────────────────▶ evidence.AlignAll ─▶ receipts.Sign
                                                            │
/verify ◀── claims/receipt ── 重访(engine) ── 判定三态 ──▶ ledger.verifications
/watch  ──▶ scheduler ──▶ /verify ──▶ ledger.facts(双时态) ──▶ webhook fact.changed
```

### 2.3 存储决策（定案）

- **快照 = 磁盘 CAS**：`$PURIFY_DATA_DIR/snapshots/<sha[0:2]>/<sha[2:4]>/<sha256>.html.zst` + 同名 `.json` sidecar（url/fetched_at/engine/status_code/content_type）。zstd 单例 Encoder/Decoder。临时文件+rename 原子写。成本账：20KB/页（zstd 后），100 万页 ≈ 20GB——量级无忧，留 retention 配置。
- **结构化 = SQLite**（`modernc.org/sqlite`，纯 Go 无 CGO，不破坏现有单二进制发布与 GitHub Actions release 流程）。`ledger/db.go` 统一持有连接：WAL 模式、`busy_timeout=5000`、写操作过单 goroutine（channel 序列化）或全局写锁。migrations = 内嵌 `[]string` 按序执行，`schema_migrations(version)` 记录。
- **为什么不是 Postgres**：单人、单机、单二进制阶段，嵌入式先赢；表设计保持可平移（无 SQLite 特有语法），并发瓶颈出现时迁移。

### 2.4 新依赖（只加 3 个）

| 依赖 | 用途 | 理由 |
|---|---|---|
| `github.com/santhosh-tekuri/jsonschema/v6` | 服务端 JSON Schema 校验 | 现有 invopop/jsonschema 只做生成不做校验 |
| `modernc.org/sqlite` | 嵌入式持久层 | 纯 Go 无 CGO |
| `github.com/klauspost/compress`（间接→直接） | zstd | 已在依赖树 |

签名不引 jose 全家桶：标准库 `crypto/ed25519` + 自实现紧凑 JWS（见 P0-5）。eTLD+1 用 `golang.org/x/net/publicsuffix`（x/net 已是直接依赖）。

### 2.5 config 扩展总表（照 `config.go` 的 `envOr` 模式）

| Env | 默认 | 用途 | 引入阶段 |
|---|---|---|---|
| `PURIFY_DATA_DIR` | `./data` | CAS + SQLite 根目录 | P0 |
| `PURIFY_SNAPSHOT_ENABLED` | `true` | 快照开关 | P0 |
| `PURIFY_SIGNING_KEY` | 空（自动生成） | Ed25519 seed hex | P0 |
| `PURIFY_COMPILER_ENABLED` | `false` | 仅开启 process-owned 后台合成；不关闭 compiled 执行/verify revision resolver | P2 |
| `PURIFY_COMPILER_API_KEY` | 空 | 后台合成专用 provider credential，不作为请求 BYOK fallback | P2 |
| `PURIFY_COMPILER_MODEL` | `gpt-4o-mini` | 后台真值提取模型 | P2 |
| `PURIFY_COMPILER_BASE_URL` | `https://api.openai.com/v1` | 后台真值提取 OpenAI-compatible base URL | P2 |
| `PURIFY_SEARCH_BRAVE_KEY` | 空 | 首个 search provider | P4 |
| `PURIFY_WATCH_ENABLED` | `false` | watch 调度器 | P6 |

> **EN —** Storage is decided: content-addressed snapshots on disk (zstd, ~20KB/page) plus a single embedded SQLite ledger (modernc.org/sqlite, WAL, serialized writes) so the binary stays self-contained. Only three new dependencies; signing uses stdlib Ed25519 with a hand-rolled compact JWS. All new env vars follow the existing `envOr` pattern.

---

## 3. Phase 0 — 地基：校验 + 快照 + 证据 + 收据（W1–W2）

**目标一句话**：让 `/extract` 的每个字段能出示收据，同时把 schema 从"嘴上约束"变成硬校验。做完这一阶段，产品叙事"带收据的 JSON"即成立。

### 任务卡 P0-1 · 严格 schema 校验 + 修复重试

**完成什么**：LLM 输出必须过服务端 JSON Schema 校验；不过则带着违规清单重试一次；仍不过则如实标注 partial。

**交付什么**：
- 新文件 `llm/schema.go`：`ValidateAgainstSchema`
- `llm/openai.go`：`json_schema (strict)` 支持 + provider 能力探测降级 + `ExtractWithRepair`
- `api/handler/extract.go`：校验-修复流程接线
- `llm/schema_test.go`：5 用例矩阵

**验收标准**：
- [ ] 类型不符 / 缺 required / 枚举越界 / 嵌套数组错误 / 多余字段 五类均能报出 `Violation{Path,Message}`
- [ ] 修复重试最多 1 次；两次均败时响应 `partial:true` + `violations[]`，而非假装成功
- [ ] 对不支持 `response_format: json_schema` 的 provider（400 报错含 "response_format"）自动降级 `json_object` 并缓存该 baseURL 的能力
- [ ] `go test ./...` 全绿

**实现参考**：

```go
// llm/schema.go
type Violation struct{ Path, Message string }
func ValidateAgainstSchema(schema, data json.RawMessage) ([]Violation, error)
// santhosh-tekuri/jsonschema/v6: Compile → Validate → 展平 ValidationError.Causes

// llm/openai.go 增量
var rfSupport sync.Map // baseURL → bool（json_schema 能力缓存）
func (c *Client) ExtractWithRepair(ctx context.Context, content string,
    schema, prev json.RawMessage, viol []Violation, p ExtractParams) (*ExtractResult, error)
// system prompt 附违规清单与上次输出，要求只修不改对的字段
```

extract.go 步骤 4 之后：

```text
viol := ValidateAgainstSchema(req.Schema, llmResult.Data)
if len(viol) > 0:
    llmResult = ExtractWithRepair(...)   // 第二次调用
    viol = ValidateAgainstSchema(...)    // 复检
resp.Partial = len(viol) > 0 ; resp.Violations = viol
```

### 任务卡 P0-2 · snapshot/ 内容寻址快照库

**完成什么**：每次成功抓取的原始 HTML 落盘为内容寻址快照——证据体系与未来时间账本的物理地基。

**交付什么**：
- 新包 `snapshot/`：`store.go` + `store_test.go`
- `config.go`：`StorageConfig{DataDir, SnapshotEnabled, SigningKey}`
- `api/handler/scrape.go`、`extract.go` 接入（`DoScrape` 成功后 `Put`）；`api.NewRouter` 与 `cmd/purify/main.go` 增参

**验收标准**：
- [ ] 同内容重复 `Put` 返回同 ID，磁盘只有一份（幂等）
- [ ] `Put`→`Get` 往返字节一致，Meta 完整
- [ ] 写入是原子的（tmp+rename），损坏文件 `Get` 报错不 panic
- [ ] `PURIFY_SNAPSHOT_ENABLED=false` 时零磁盘写入，主流程不受影响

**实现参考**：

```go
type ID string // "sha256:<hex64>"
type Meta struct {
    URL string; FetchedAt time.Time; Engine string
    StatusCode int; ContentType string
}
func NewStore(dir string) (*Store, error)
func (s *Store) Put(html []byte, m Meta) (ID, error)
func (s *Store) Get(id ID) ([]byte, Meta, error)
func (s *Store) Has(id ID) bool
// 路径: snapshots/ab/cd/<sha256>.html.zst + <sha256>.json
// zstd: 包级共享 *zstd.Encoder/Decoder（klauspost/compress/zstd）
```

### 任务卡 P0-3 · evidence/ 值→锚点对齐器

**完成什么**：LLM 提取出的每个叶子值，在清洗文本与原始 HTML 里定位出"收据"：原文引文、字符区间、CSS 选择器。定位失败如实标 `unlocated`——**unlocated 率 = 免费的幻觉检测器**，这是别家给不出的指标。

**交付什么**：
- 新包 `evidence/`：`align.go`（三级匹配）+ `selector.go`（goquery 反查）+ `align_test.go`
- extract.go：`req.Evidence == true` 时挂载

**验收标准**：
- [ ] 表驱动 8 用例全过：精确命中 / 空白差异 / 千分位价格（"1,299" vs "1299"）/ 全角半角 / 改写句 fuzzy 命中 / 不存在值 → unlocated / 嵌套数组路径 `items.0.name` / selector 在文档内唯一
- [ ] `AlignAll` 返回 basis 与 unlocated 率；响应可见
- [ ] evidence:true 的 P95 额外延迟 < 30ms（用 `scripts/benchmark` 量测）

**实现参考**：

```go
type Method string // "exact" | "normalized" | "fuzzy" | "unlocated"
type Anchor struct {
    Quote     string    `json:"quote"`
    TextRange [2]int    `json:"text_range"`
    Selector  string    `json:"selector,omitempty"`
    Method    Method    `json:"method"`
    SnapshotID string   `json:"snapshot_id"`
    FetchedAt time.Time `json:"fetched_at"`
}
func AlignValue(value, cleaned, rawHTML string) Anchor
func AlignAll(data json.RawMessage, cleaned, rawHTML, snapID string) (map[string]Anchor, float64)
// 叶子路径键: "price" / "items.0.name"（点号+下标）
```

AlignValue 算法（编号执行）：

```text
1 exact      strings.Index(cleaned, value) 命中即取区间
2 normalized 双方归一化: 折叠空白/lower/全角→半角/去千分位与货币符号;
             归一化过程同步构建 offset 映射表, 命中后回原文区间
3 fuzzy      value 分词; cleaned 词级滑窗(窗宽 len±2); Jaccard ≥ 0.8 取最优窗
4 selector   goquery 遍历文本节点找含 Quote 的最深元素;
             生成最短唯一 CSS 路径: 优先 #id; 否则 tag.class:nth-of-type 链;
             以 doc.Find(sel).Length()==1 验唯一
5 全部失败   Method = unlocated（进 unlocated 率统计）
```

### 任务卡 P0-4 · models 扩展与响应契约

**完成什么**：extract 请求/响应模型承载证据与校验结果，公开契约固定下来。

**交付什么**：`models/extract.go` 增量字段；文档级响应示例。

**验收标准**：
- [ ] 未开 evidence 时响应与现状**完全向后兼容**（新字段全 omitempty）
- [ ] 响应示例进 README/文档

**实现参考**（响应契约）：

```jsonc
// POST /api/v1/extract  请求增: "evidence": true
{
  "success": true,
  "data": { "price": "$29.99", "title": "Pro Plan" },
  "partial": false,
  "snapshot_id": "sha256:9f2c…",
  "unlocated_rate": 0.0,
  "basis": {
    "price": {
      "quote": "Pro plan — $29.99/month, billed annually",
      "text_range": [1204, 1246],
      "selector": "div.pricing-card:nth-of-type(2) .amount",
      "method": "exact",
      "snapshot_id": "sha256:9f2c…",
      "fetched_at": "2026-08-09T08:00:00Z"
    }
  },
  "receipts": { "price": "eyJhbGciOiJFZERTQSJ9.…" },
  "extractor": null,          // Phase 2 起填充
  "metadata": { "…": "现有字段不动" }
}
```

### 任务卡 P0-5 · receipts/ 可携带签名收据

**完成什么**：证据升维成可流通对象——Ed25519 签名、自包含、离线可验；公共验证端点对所有人免费（获客漏斗 + "Verified by Purify" 徽章的地基）。

**交付什么**：
- 新包 `receipts/`：`receipts.go`（Sign/Verify）+ `keys.go`（密钥加载）+ 测试
- `api/handler/receipts.go`：公开 `POST /api/v1/receipts/verify`、`GET /api/v1/receipts/pubkey`（router 放 health 同级，不鉴权）
- extract 响应 `receipts` 字段（evidence:true 时）

**验收标准**：
- [ ] 签→验往返成功；篡改 payload 任一字节验签失败；错误公钥验签失败
- [ ] 无 `PURIFY_SIGNING_KEY` 时首次启动自动生成 `data/signing.key`（0600）并告日志
- [ ] 验证端点无需 API key 可调

**实现参考**：

```go
type Payload struct {
    V string `json:"v"` // "purify-receipt/1"
    URL, Path string
    Value  json.RawMessage
    Anchor evidence.Anchor
    ExtractorVersion string
    IssuedAt time.Time
    KID string // 公钥前 8 字节 hex
}
func Sign(p Payload, priv ed25519.PrivateKey) (string, error)
// b64url(header{"alg":"EdDSA","kid":…}) + "." + b64url(payload) + "." + b64url(sig)
func Verify(token string, pub ed25519.PublicKey) (*Payload, error)
func LoadOrCreateKey(cfg config.StorageConfig) (ed25519.PrivateKey, string, error)
```

### 3.6 Phase 0 提交序列

每个 commit 前：`go test ./...` + `git diff --check`；单一关注；**不带任何 AI 署名尾注**。

```text
feat(llm): enforce json schema validation with repair retry
feat(snapshot): content-addressed page store
feat(evidence): field-level anchor alignment
feat(receipts): signed portable fact receipts
docs(scrape): document evidence mode and receipts
```

> **EN —** Phase 0 delivers the foundation: hard server-side schema validation with one repair retry, a content-addressed snapshot store, a three-level value-to-anchor aligner whose `unlocated` rate doubles as a free hallucination metric, and Ed25519-signed portable receipts with a free public verification endpoint. After this phase the "JSON you can prove" story is shippable.

---

## 4. Phase 1 — /verify 复验端点 + MCP（W3）

**目标一句话**：把"事实"变成可以随时重新执行的断言；每次裁决落库，校准数据从第一天开始积累。

### 任务卡 P1-1 · verify/ 三态复验服务

**完成什么**：给定 URL + 旧值（claims 或直接给收据），重访页面，裁决 confirmed / changed / gone，附新证据与整页相似度。

**交付什么**：
- 新包 `verify/`：`service.go` + `service_test.go`
- `models/verify.go`：完整请求/响应模型

**验收标准**：
- [ ] 三态判定表全覆盖（fixtures 驱动）：值未变 / 值变 / 字段消失 / 整页 404 / selector 失效但 quote 仍在
- [ ] `page_similarity` 来自 `simhash.FingerprintDOM` 距离
- [ ] 传收据时自动解签还原 claim，无需手填

**实现参考**：

```go
type Claim struct {
    Path string; Value json.RawMessage
    Quote, Selector string // 来自原 Anchor
}
type VerifyRequest struct {
    URL string; Claims []Claim
    Receipt string // 与 Claims 二选一
}
type ClaimResult struct {
    Path string
    Status string // "confirmed" | "changed" | "gone"
    NewValue json.RawMessage `json:",omitempty"`
    Evidence *evidence.Anchor
    Receipt  string // 新签收据
}
type VerifyResponse struct {
    Results []ClaimResult
    PageSimilarity float64
    SnapshotID string
    VerifiedAt time.Time
}
```

判定表：

| 观测 | 裁决 |
|---|---|
| fetch 404/410 | 全部 claims → `gone`（page 级） |
| selector 命中，规范化比较相等（数值解析后比、字符串归一化比） | `confirmed` |
| selector 命中，值不等 | `changed`（新值 + 新锚 + 新收据） |
| selector 失效，`AlignValue(旧 Quote)` 命中 | `confirmed` |
| selector 与 quote 双失效 | `gone`（field 级；`page_similarity` 提示是否整页改版） |

### 任务卡 P1-2 · ledger/ 持久层起步

**完成什么**：SQLite 统一持久层 + 第一张表 `verifications`。

**交付什么**：`ledger/db.go`（Open/WAL/单写序列化/migrations 机制）+ `migrations.go`（001）。

**验收标准**：
- [ ] 重复启动 migrations 幂等；并发写不 `SQLITE_BUSY`（压测 100 并发 verify 落库）
- [ ] 每次 /verify 的每条 claim 裁决一行入库

**实现参考**（migration 001）：

```sql
CREATE TABLE verifications(
  id INTEGER PRIMARY KEY,
  url TEXT NOT NULL, path TEXT,
  old_value TEXT, new_value TEXT,
  outcome TEXT CHECK(outcome IN('confirmed','changed','gone')),
  page_similarity REAL,
  verified_at TEXT NOT NULL,
  receipt TEXT
);
CREATE INDEX idx_verifications_url ON verifications(url, verified_at);
```

### 任务卡 P1-3 · HTTP + MCP + webhook 接线

**交付什么**：
- router：`protected.POST("/verify", handler.Verify(vs))`
- `cmd/purify-mcp/main.go`：`verify_fact` 工具（照 `scrape_url` 的 `mcp.NewTool` 模式；参数 `url` + `claims` JSON 字符串）
- webhook 事件 `fact.changed`（复用现有投递约定）

**验收标准**：
- [ ] MCP 客户端可调 verify_fact 并拿到三态结果
- [ ] changed 时 webhook 收到新旧值成对

**提交序列**：

```text
feat(ledger): sqlite store with migrations
feat(verify): three-state fact re-verification
feat(mcp): verify_fact tool
docs(scrape): verify endpoint guide
```

> **EN —** /verify turns facts into re-executable assertions: three-state verdicts (confirmed/changed/gone) driven by a decision table over selector hits, quote alignment, and DOM simhash similarity. Every verdict lands in the new SQLite ledger — calibration data accrues from day one. MCP gains `verify_fact`.

---

## 5. Phase 2 — compiler/ 编译式提取器（W4–W6）

**目标一句话**：LLM 合成一次、确定性执行 N 次；与 Firecrawl deterministicJson 同路，但**透明**——版本、验证分、漂移全部亮牌。热路径纯 Go，毫秒级，边际成本≈0。

> **实况对齐 · 2026-08-09（Phase 2 收口 HEAD `81ce10c`）** —— 本节已按提交与磁盘实况回写。图例：✅ 已提交 · 🚧 进行中（未提交 WIP）· ⬜ 未开始 · ⟳ 与早期规划不同（以实际接口/DDL 为准）。
> 进度：P2-1 ✅　P2-2 ✅　P2-3 ✅　P2-4 ✅；REST、managed synthesis、benchmark、安全边界与 MCP 文档均已对齐。

### 任务卡 P2-1 · IR 定义与确定性执行

**交付什么**：`compiler/ir.go` + `execute.go` + `transforms.go` + 测试。

**状态**：✅ 已提交 `5ce96a4 feat(compiler): execute deterministic extraction IR`。⟳ 实际另落地整套资源上限（`MaxHTMLBytes` 4 MiB、`MaxFields` 100、`MaxSelectorBytes`、`MaxTransformsPerField` 16、`MaxOutputBytes` 等，见 `ir.go`），`CurrentIRVersion=1`；畸形 selector 经 `compileRules` 编译失败即字段级报错、不 panic。

**验收标准**：
- [x] `Execute` 单次 goquery 解析全字段；selector 命中即产出 Anchor（Method="compiled"）
- [x] transforms 注册表内置：trim / collapse_ws / lower / parse_number / parse_date（用已有 dateparse）/ currency_amount
- [x] 恶意/畸形 selector 不 panic（cascadia 解析失败即字段报错）

**实现参考**：

```go
type FieldRule struct {
    Name, Selector string
    Attr  string   // 空 = 取 text
    Regex string   // 可选，捕获组 1
    Transforms []string
    Type string    // string|number|boolean|date
    Required bool
}
type IR struct{ Version int; Fields []FieldRule }
func Execute(ir IR, html string) (json.RawMessage, map[string]evidence.Anchor, error)
```

### 任务卡 P2-2 · LLM 引导编译 + 验证报告

**完成什么**：用快照样本编译出 IR，并给出可公示的验证分——**validation ≥ 0.9 才允许启用**。

**交付什么**：`compiler/compile.go` + 测试（fixtures：同站 3 页）。

**状态**：✅ 已提交 `60ce9f3 feat(compiler): synthesize validated extraction rules`。

**实现参考**（编号伪码）：

```text
Compile(ctx, samples 3..20, schema, TruthExtractor):
1 每样本经 TruthExtractor 直提 → 真值集（valid samples 计数）
2 每字段×样本 对齐定位 → 候选集（含 attr/regex/transforms 流水线）
3 跨样本泛化: 一致处剥 nth-of-type、偏好稳定 class/#id; cascadia 验合法
4 每字段选跨样本命中率最高的候选（betterCandidate 排序）
5 按 schema type 推断 transforms
6 ValidationReport: Execute(IR) vs 真值 逐字段一致率（规范化比较）
返回 (IR, ValidationReport{..., CanEnable})
```

⟳ 实际签名与类型（`compile.go`）——编译期 LLM client 抽象为**传输中立接口** `TruthExtractor`，密钥/provider/修复/HTTP 全在调用方 adapter；`compiler` 不依赖 LLM client/transport，但仍复用 `llm` 包内纯函数式 schema 规范化与校验工具：

```go
type TruthExtractor interface {
    ExtractTruth(context.Context, string, json.RawMessage) (json.RawMessage, error)
}
type Sample struct{ Content, HTML string } // Content 喂真值提取；HTML 学 selector + 验证
func Compile(ctx context.Context, samples []Sample, schema json.RawMessage,
    extractor TruthExtractor) (IR, ValidationReport, error)

type ValidationReport struct { // 可随提取器一同公示
    PerField     []FieldValidation // {Name, Matches, Samples, Score}
    Overall      float64
    Samples      int
    ValidSamples int
    Threshold    float64 // ValidationThreshold = 0.9
    CanEnable    bool    // 每样本 schema-valid 且 Overall≥Threshold 才 true —— 启用门闩
}
// MinCompileSamples=3 · MaxCompileSamples=20
```

### 任务卡 P2-3 · 提取器仓库：缓存键、模板簇、漂移信号

**交付什么** ⟳：实况拆两层——(a) **持久层**落在 `ledger/`：`migrations.go` 的 **migration 003**（建 `extractors` + `extractor_page_bindings`）与 `transactions.go` 的窄事务原语 `View`/`Update`；(b) 提取器 **repository**（`BuildPageKey / TemplateMedoid / Lookup / Save / Get / Touch / RecordEmpty / Retire`），建在 `ledger.Store` 之上。credential-free 的样本目录与 `Coordinator.Observe`/后台 synthesis 归 P2-4；生产环境仅在显式启用 managed compiler 后实例化这条合成链。

**状态**：✅ 持久层已提交 `fd1de32 feat(ledger): add extractor registry foundation`；repository 已提交 `f5a32ba feat(compiler): persist template-clustered extractors`。全仓测试、ledger/compiler race、vet 与两路独立 P0/P1 审查均通过。

**验收标准**：
- [x] 缓存键 `(host, schema_hash, template_cluster_id)` 命中正确；同站不同模板（列表页/详情页）各自成器；`extractor_page_bindings(page_hash→extractor_id)` 在一次成功执行后建立
- [x] 每个 `(host, schema_hash, cluster)` 至多一个 active —— 由**部分唯一索引** `WHERE state='active'` 从 SQL 层强制
- [x] state 仅允许 `active→stale/retired`、`stale→retired`；版本化字段不可原地覆写，且只有 retired 行可物理删除
- [x] 漂移双信号触发 state=stale：已绑定页模板距离 > 6；required 近 20 次空值率 > 30%（6/20 保持 active，7/20 stale）

**migration 003 简化结构示意（非逐字 DDL）**：为便于阅读省略了部分类型/长度/时间格式检查与 trigger body；完整实况和全部约束只以 `ledger/migrations.go` 为准。

```sql
CREATE TABLE extractors (
  id TEXT NOT NULL PRIMARY KEY,                 -- lowercase UUID
  host TEXT NOT NULL,                           -- canonical lowercase host/eTLD+1
  schema_json TEXT NOT NULL CHECK (json_valid(schema_json)),
  schema_hash TEXT NOT NULL CHECK (length(schema_hash) = 64),
  template_cluster_id TEXT NOT NULL,
  template_simhash BLOB NOT NULL CHECK (
    typeof(template_simhash) = 'blob' AND length(template_simhash) = 8
    AND template_simhash <> zeroblob(8)),
  ir TEXT NOT NULL CHECK (json_valid(ir)),
  ir_hash TEXT NOT NULL CHECK (length(ir_hash) = 64),
  ir_format_version INTEGER NOT NULL CHECK (ir_format_version > 0),
  version INTEGER NOT NULL CHECK (version > 0),
  validation_report TEXT NOT NULL CHECK (json_valid(validation_report)),
  validation REAL NOT NULL CHECK (validation >= 0.0 AND validation <= 1.0),
  state TEXT NOT NULL CHECK (state IN ('active','stale','retired')),
  empty_window TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(empty_window)),
  stale_reason TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL, last_used_at TEXT, updated_at TEXT NOT NULL,
  CHECK (state <> 'active' OR (
    validation >= 0.9 AND json_extract(validation_report, '$.can_enable') IS 1)),
  UNIQUE (id, schema_hash),
  UNIQUE (host, schema_hash, template_cluster_id, version)
) STRICT;
-- 每个模板簇至多一个 active
CREATE UNIQUE INDEX idx_extractors_active_cluster
  ON extractors(host, schema_hash, template_cluster_id) WHERE state = 'active';
CREATE INDEX idx_extractors_lookup
  ON extractors(host, schema_hash, state, template_cluster_id, version DESC);
CREATE INDEX idx_extractors_ir_hash ON extractors(ir_hash);
-- 真实 migration 另含：state+reason 单调、revision immutable、retired-only delete、
-- empty_window vocabulary/长度等 BEFORE triggers。
-- 页→器绑定缓存：重复页直取，跳过聚类
CREATE TABLE extractor_page_bindings (
  page_hash TEXT NOT NULL CHECK (length(page_hash) = 64),
  schema_hash TEXT NOT NULL CHECK (length(schema_hash) = 64),
  extractor_id TEXT NOT NULL,
  bound_at TEXT NOT NULL, last_seen_at TEXT NOT NULL,
  PRIMARY KEY (page_hash, schema_hash),
  FOREIGN KEY (extractor_id, schema_hash)
    REFERENCES extractors(id, schema_hash) ON DELETE RESTRICT
) STRICT;
CREATE INDEX idx_extractor_page_bindings_extractor
  ON extractor_page_bindings(extractor_id);
```

⟳ **与早期规划的差异**：`id` INTEGER→**TEXT（UUID）**；`template_simhash` INTEGER→**BLOB(8)** 且新增固定 `template_cluster_id`；两张新表使用 `STRICT`；新增 schema/IR/report 哈希与 active 0.9 门闩、部分唯一索引、复合外键、版本不可变/状态原因/retired-only delete/window 触发器。migration 编号 002→**003**（Phase 1 实产两个：001 `verifications`、002 `outbox_events`）。

repository 已建在 ledger 的窄事务原语之上（调用方拿不到 commit/rollback）：

```go
func (s *Store) View(ctx context.Context, view func(ReadTx) error) error     // 只读事务（PRAGMA query_only=ON）
func (s *Store) Update(ctx context.Context, update func(WriteTx) error) error // 序列化单写 + 原子提交
```

模板匹配：当前页 `FingerprintDOM` 与库内固定 canonical representative 取最近且距离 ≤ 6；generic miss 不降级任何簇，只有已有页绑定且事务内重查仍无其他 active 命中时才标记 `template_drift`。`Lookup` 的常规命中/未绑定 miss 是只读，但 **bound-drift** 分支会升级到 `Update`，事务内重读 binding 与全部 active 后才 CAS 标 stale。实际执行后由 `Touch/RecordEmpty` 原子绑定并更新健康窗口；`not_executed` 严格零写。**硬不变量 1**：部分唯一索引要求新版本晋级必须在同一 `ledger.Update` 内先 demote 旧 active、再 insert 新 active 并重绑同簇页面。**硬不变量 2**：`extractor_page_bindings.extractor_id ... ON DELETE RESTRICT`，所以 retired extractor 的 GC/物理删除必须先解绑。

### 任务卡 P2-4 · extract 接线：auto 引擎与降级链

**完成什么**：`engine: "auto"|"compiled"|"llm"`（默认 auto）；compiled 优先、llm 兜底、编译异步进行；响应亮牌 extractor 元数据。

**状态**：✅ 已收口。核心 dispatch `8ebad26`、样本目录 `a11080a`、schema cache `71772f7`、provider 响应边界 `2df46a7`、真实热路径 benchmark `9462c19`、生产接线 `88c3471`、managed coordinator `eb742b2`、MCP `b0497a4` 与 REST/provider 安全边界 `81ce10c` 均已提交。

**验收标准**：
- [x] compiled 命中 benchmark P95 < 50ms、**零 LLM 调用**；口径与本机观测见下
- [x] compiled 响应含 `"extractor":{"id","version","compiled_at","validation","mode"}` 且不含 `llm_usage`
- [x] 所有合法的既有 `engine:"llm"` 请求保留 BYOK + strict schema + 单次 repair 语义；全仓/race/vet/build gates 通过

**真实 dispatch 契约**：

| engine | request `llm_api_key` | 行为 |
|---|---|---|
| `auto`（默认） | 无 | compiled-only；miss/不兼容返回 409 `EXTRACTOR_UNAVAILABLE`，绝不借 managed process key |
| `auto` | 有 | compiled-first；只有 unavailable 或非 context 的 compiled subsystem failure 才在**同一次 fresh fetch**上回退请求 BYOK LLM；cancel/deadline 不回退 |
| `compiled` | 不需要（即使传入也不使用） | deterministic-only、零 LLM；miss/不兼容 409，registry/IR corruption 500 fail-closed |
| `llm` | 必须有 | 直接请求 BYOK LLM；schema 校验，至多一次 repair |

compiled eligibility 固定为默认内容 profile：无 `css_selector`、`output_format=markdown`、`extract_mode=readability`；规范化 schema 必须是非空顶层 object，字段为 flat scalar（string、number/integer、boolean 或 date-time string，可带单一 nullable scalar），不接受 nested object/array、composition 或 refs。显式 `compiled` 不满足即 409；`auto+key` 可直走 LLM。只有成功执行才 `Touch`；只有 `ErrRequiredField` 才 `RecordEmpty`，optional-empty/schema-incompatible/not-executed 都不污染漂移窗口。

**接线逻辑摘要**：

```text
prepareRequest(strict bounds + canonical schema)
prepareDispatch(engine/profile/schema/key matrix)       # fetch 前 fail-fast
fresh fetch once (MaxAge=0)
if eligible compiled:
    Lookup(page key)                                    # bound-drift 分支可能写 stale
    Execute(IR) -> schema revalidate -> optional signed evidence(UUID@version)
    Touch on success / RecordEmpty on required-empty
    return compiled metadata, no llm_usage
if allowed fallback/direct LLM:
    Extract -> schema validate -> at most one repair
    on full valid/default-profile/fresh-2xx snapshot: Observe(ref) best-effort
```

**managed compiler（默认关闭，只控制合成）**：`PURIFY_COMPILER_ENABLED=false` 时 `compiler.Store` 仍始终注入 `/extract`，并在 snapshot-backed `/verify` 可用时作为 revision resolver；因此已有 compiled revision 继续执行/重放。启用后台合成必须同时满足 snapshots enabled、非空 `PURIFY_COMPILER_API_KEY`、合法 model 与绝对 http(s) base URL。process key 只存在于 `managedTruthExtractor`，永不补 request `llm_api_key`；请求 BYOK/provider overrides 也永不进入 coordinator。开启此开关意味着允许后台把持久快照清洗内容发往所配 provider，是明确的数据外发 opt-in。`TruthExtractor` 让 `compiler` 不依赖 LLM client、transport、provider 或 key；包内仍复用 `llm` 的纯 schema normalize/validate 工具。

只有 full schema-valid LLM 成功、默认 profile、fresh 2xx 且有 snapshot 的结果才允许观察；observer error/panic 不反转客户成功。migration 004 `compiler_samples` **只存引用**（page/snapshot hash、sample/cluster simhash、fetched/seen time），页面仍在 snapshot CAS，credential/cleaned content/truth output 均不落 catalog。ready 必须同时满足同一固定 cluster 中 `>=3 distinct page_hash` **且** `>=3 distinct snapshot_id`。边界：样本 3..20、TTL 30d、每 cluster 20、每 host/schema 最多 256 clusters、全局 10,000 refs。

migration 005 `compiler_attempts` 持久化 revision watermark、cooldown 与 lease。coordinator 是单 worker、public queue 32（另留一个已接收 dirty revision 的物理槽）、task timeout 2m、lease 3m、transient cooldown 15m、weak/no-candidate cooldown 24h、attempt TTL 30d/全局 10,000 rows；同 key/revision 以 durable lease + in-process singleflight 合并。进程 crash 后由后续观察重新调度，语义是**有界 at-least-once**，不宣称 exactly-once。shutdown 顺序为 coordinator → managed HTTP idles → snapshot → ledger。

**资源与出网边界**：REST `/extract` 只收一个 JSON object，unknown fields/trailing value 拒绝，body 1 MiB；raw/normalized schema 各 512 KiB，URL/key/base URL 各 16 KiB，model 256 bytes，均在 fetch/provider 前 N/N+1 拒绝。request 与 managed provider 共用 public-only HTTP client：完整 DNS answer 校验、dial-time literal-IP pin、禁 private/reserved/mixed answers、忽略环境 proxy、禁止携 credential/body redirect、TLS ≥1.2、timeout 120s（coordinator 另受 2m task bound）。完整 provider envelope 上限 32 MiB；managed truth 与 deterministic output 各 4 MiB。schema compiler 是 success-only LRU：128 entries / 8 MiB / 单 schema 512 KiB；inflight coordination map 上限 128，溢出请求绕过 cache 独立 compile；失败不缓存、不保留 caller buffer 引用。

**MCP `extract_data`**：tool schema 的 `engine` enum 默认 auto，key/model/base URL 可选且各自有同 REST 等级上限，`schema` 是恰好一个 JSON value 的字符串；unknown argument、trailing JSON 与非法 schema 在 HTTP 前拒绝。`engine=compiled` 时 adapter **物理剥离** provider fields。`PURIFY_API_KEY` 仅通过 `X-API-Key` 鉴权 Purify API，与 payload 内 `llm_api_key` 分离。API success 以 strict `models.ExtractResponse` 解码并返回 structured content + pretty JSON fallback；非 2xx 保留结构化错误 code，异常文本会 redaction 两类 key，API response 读取上限 32 MiB。

**benchmark 口径**：

```bash
go run ./scripts/benchmark/main.go compiled -runs 1000 -warmup 100
```

它在临时真实 SQLite + `compiler.Store` 上执行完整 `ExtractArtifact(auto)`（Lookup → Execute → schema validation → Touch），明确排除 page fetch，并用 guard 强制 LLM calls=0；门槛是严格 `p95 < 50ms`。2026-08-09 本机一次观测如下，**仅是可复现开发样本，不是 SLA/跨机器保证**：

```json
{
  "runs": 1000,
  "warmup_runs": 100,
  "p50_ms": 0.435875,
  "p95_ms": 0.755709,
  "p99_ms": 1.016917,
  "llm_calls": 0,
  "threshold_ms": 50,
  "threshold_passed": true
}
```

**提交序列**：

```text
✅ 5ce96a4  feat(compiler): execute deterministic extraction IR       # 规划名: extractor IR and deterministic execution
✅ 60ce9f3  feat(compiler): synthesize validated extraction rules      # 规划名: llm-guided compilation with validation report
✅ fd1de32  feat(ledger): add extractor registry foundation             # migration 003 + View/Update
✅ f5a32ba  feat(compiler): persist template-clustered extractors        # repository + clustering + drift
✅ b22e32d  docs(plan): align phase 2 implementation status             # P2-1..P2-3 中途实况回写
✅ a11080a  feat(compiler): catalog bounded compile samples              # migration 004 refs-only catalog
✅ 71772f7  perf(llm): cache compiled schemas within bounds              # success-only bounded LRU/singleflight
✅ 8ebad26  feat(extract): dispatch compiled extraction safely           # engine matrix + same-fetch fallback
✅ 2df46a7  fix(llm): bound provider responses                            # 32 MiB complete envelope
✅ 9462c19  perf(extract): benchmark compiled hot path                    # real SQLite/full ExtractArtifact benchmark
✅ 88c3471  feat(extract): wire managed compiler                          # default-off config + production bindings
✅ eb742b2  feat(compiler): coordinate managed synthesis                  # migration 005 + durable lease/cooldowns
✅ b0497a4  feat(mcp): support compiled extraction                        # strict extract_data structured tool
✅ 81ce10c  fix(extract): harden request and provider boundaries          # strict body/limits/public-only HTTP
```

> **EN — (Phase 2 closed 2026-08-09, HEAD `81ce10c`)** P2 now ships deterministic IR execution, validated managed synthesis, an immutable template-clustered SQLite registry, compiled-first REST dispatch, bounded refs-only sampling and durable attempt coordination, a real zero-LLM hot-path benchmark, strict MCP structured output, and fail-closed request/provider boundaries. Managed synthesis is default-off and uses only its process credential; compiled execution and verification resolution remain active independently. Auto without BYOK is compiled-only, while auto with BYOK may reuse the same fetch for bounded fallback. Registry promotion preserves the partial unique index by demoting then inserting in one `ledger.Update`, and retired GC must unbind before delete because the binding FK is `ON DELETE RESTRICT`.

---

## 6. Phase 3 — 自愈闭环 + consensus/（W7–W8）

**目标一句话**：漂移→影子重编译→快照回归→晋级或告警，闭环全程留痕；多源提取给出 N_eff 与带证据的冲突集。

> **实况对齐 · 2026-08-10（第二次回写，HEAD `8565ac8`）** —— 图例：✅ 已提交并可用 · 🚧 部分 · ⬜ 未开始 · ⟳ 与早期规划不同（以实际接口/状态机为准）。
> 进度：**P3-1 ✅　P3-2 ✅　P3-3 ✅，Phase 3 收口**。原待授权的生产 main 接线已获授权并完成：`3017c9e` 把一个 process-owned safe relay（`newManagedSafeRelay`，随 snapshot 能力启停）同时注入 verify revisit 与 `extract.Config.SafeProxyURL`（`sources[]` 点亮）；`e6fd741` 实例化 `managedHealRuntime`、注入 `POST /api/v1/extractors/:id/heal`、并用 `managedBackgroundLifecycle` 建立 drain 后 compiler→heal→outbox→relay 的显式 shutdown 顺序。

### 任务卡 P3-1 · 自愈：回归晋级制

**交付什么** ⟳：实际拆成四层：`compiler/candidate.go` 将 synthesis 与 publication 分离并持久化 immutable candidate；`compiler/selfheal.go` 对 confirmed 历史做有界回放并原子晋级；`compiler/heal_worker.go` 轮询 durable run/回收过期 lease；`api/handler/extractor_heal.go` 提供只唤醒既有 run 的手动入口。`ledger` 的 subject-aware outbox 承载 `extractor.promoted|extractor.degraded`。

**状态**：✅ 核心 `3590371`、`440bade`、`2f4f715`、`135dba1`；生产接线 `e6fd741 feat(main): run production self-heal runtime`（worker 后台轮询、手动 heal 路由注入、显式 shutdown 顺序）。

**验收标准**：
- [x] 晋级门槛：candidate 对 exact stale revision 的 confirmed 历史重放，至少 1 fact 且 `matched/total ≥ 0.9`
- [x] 回归不过或历史不足：run 进入 `degraded`，旧 stale revision 不被静默替换；invalid candidate 进入 `failed`
- [x] 晋级在一个 `ledger.Update` 内完成：旧 stale revision→retired、新 revision=`source.version+1`→active、页面重绑、run terminal 与可选 outbox event 同事务提交
- [x] durable lease、过期回收、terminal idempotency、手动 202 scheduling receipt 与 webhook outbox 已有测试
- [x] production main 启动 worker、注入 protected route service，并纳入明确 shutdown 顺序（`e6fd741`）

⟳ **真实流程不是 `Heal` 内再调用 LLM Compile**。合成结果先经 `SubmitCandidate` 作非破坏性 admission：新 exact cluster 可登记 v1 active；健康 active 保留；可归属 stale lineage 的 candidate 才创建/复用 immutable pending heal run；retired/ambiguous lineage 不复活。随后 Healer 只重放这份已冻结 candidate：

```text
Observe refs → managed synthesis → SubmitCandidate(candidate)
  new exact cluster                         → v1 active
  attributable stale source                → pending heal run (immutable)
  active / retired / ambiguous history      → preserve/reject; never replace in place

HealWorker poll or manual wake
  → claim pending/expired-replaying run with lease
  → revalidate frozen schema/IR/report/samples + exact stale source identity
  → replay bounded confirmed history from immutable snapshots
  → ratio ≥ 0.9 and total > 0: atomic promoted
  → otherwise: degraded/failed; stale source remains non-active
```

`state=stale` 本身不会凭空创建 candidate/run：必须有后续合格 observation，经 managed synthesis 与 `SubmitCandidate` 形成 pending run；只有在 HealWorker 被 production 启动后，pending/expired run 才会被自动轮询。

**回放与租约边界**：只扫描 candidate `created_at` 之前、同 exact stale source lineage（`extractor_id + schema_hash + source template_cluster_id`）的 `confirmed` facts；SQL 最多扫描 1,000 行，最多计 100 个去重 string/number/bool facts、读取 20 个 snapshot，每份最多 4 MiB、总 snapshot bytes 最多 80 MiB。畸形/超限历史按未匹配计分或跳过执行，缺 provenance、非 2xx observation、URL/time 不符、snapshot 缺失、IR 执行失败均不会制造 match。单次 task 2 分钟、lease 3 分钟、异常释放窗口 5 秒；worker 固定单 goroutine、默认 1 秒轮询，wake channel 只是提示，run identity 始终来自 ledger。

**交付语义是 resource-bounded at-least-once，不是 exactly-once**：进程在 claim 后退出，过期 lease 可被下一 worker 重放；terminal CAS/immutable triggers 与同一事务晋级阻止重复 publication。terminal heal audit 不可删除，promotion 后旧 revision 以 retired 行保留。若配置 lifecycle webhook，terminal run 与 outbox event 原子入库；通用 outbox 以 durable event ID（`X-Purify-Event-ID`）支持下游幂等，retryable failure 的持久预算为 4 次 attempts（1s/5s/30s backoff、单次 10s）。delivery 成功后、落 `delivered_at` 前崩溃仍可能额外重投，所以 webhook 只承诺 duplicate-capable at-least-once，不保证 exactly-once 或最终必达。

**手动 API 的真实含义**：`POST /api/v1/extractors/:id/heal` 必须是空 body；它只解析 exact stale extractor revision 已有的 pending/replaying run，成功返回 202 `{"extractor_id":"…","heal_run_id":"…","status":"accepted"}`，不会凭 extractor ID 推断 compile key，也不会现场合成 candidate。not found→404、没有 ready candidate/歧义→409、worker unavailable→503。

### 任务卡 P3-2 · consensus/ 多源合并与 N_eff v0

**完成什么**：N 源同 schema 各自提取，字段级合并；**十个镜像 = 1 路独立证据**；冲突不裁决、带证据全暴露。

**交付什么**：`consensus/merge.go` + `materialize.go` + 测试。

**状态**：✅ 已提交 `8a30f74 feat(consensus): merge sources with independence accounting` 与 `e0e918c feat(consensus): materialize unambiguous merged data`。

**验收标准**：
- [x] 表驱动覆盖全一致、同根/通稿镜像折叠、真分歧、URL 去重冲突与输入顺序无关
- [x] 每个 winner/conflict 都输出 `agreement:{pages, independent_roots}` 与逐 source support（URL/root/evidence/receipt）
- [x] top score 并列不擅自裁决；所有候选进入 `conflicts`，`ambiguous:true`
- [x] typed materialization 覆盖 node kind、object key presence、array length 与 scalar value；任一最高分并列即 aggregate ambiguous

**真实公共类型（简化）**：root 不接受 caller 输入，而由 canonical final URL 推导。

```go
type SourceResult struct {
    URL      string                     // canonical final URL
    Data     json.RawMessage
    Basis    map[string]evidence.Anchor
    Receipts map[string]string
    SimText  uint64                     // simhash.Fingerprint(cleaned content)
}
type Agreement struct{ Pages, IndependentRoots int }
type Support struct { URL, Root string; Evidence *evidence.Anchor; Receipt string }
type Conflict struct { Value json.RawMessage; Agreement Agreement; Supports []Support }
type FieldConsensus struct {
    Value json.RawMessage; Agreement Agreement; Supports []Support
    Conflicts []Conflict; Ambiguous bool
}
func Merge([]SourceResult) (Result, error)
func MergeWithMaterialization([]SourceResult) (Result, Materialization, error)
```

**N_eff 与冲突排序实况**：

```text
1 canonical URL 完全相同且内容/证据相同 → 去重；同 URL 非同结果 → ErrDuplicateSourceConflict
2 Root = lowercase publicsuffix eTLD+1；literal IP 使用 Unmap 后地址
3 两 source 同 Root，或两者 SimText 非零且 Hamming distance ≤ 3 → union-find 同一独立 component（传递闭包）
4 scalar 按 null / bool / exact-rational number / string 规范化分组
5 Agreement.Pages = 该 value 的 canonical unique pages
  Agreement.IndependentRoots = 该 value 覆盖的 union-find components 数（即 N_eff）
6 value groups 按 IndependentRoots DESC、Pages DESC、deterministic scalar order 排序
7 前两组同分 → 无 winner，所有 groups 均暴露为 conflicts；否则第一组 winner，其余全部保留为 conflicts
```

因此字段名虽为 `independent_roots`，计数实际同时折叠**同 eTLD+1/IP root**与**跨 root 的近重复通稿 component**；不是简单 `COUNT(DISTINCT root)`。

**Materialization**：先 probe 整棵 typed JSON decision tree，不编码 bytes；node kind、对象字段“存在/缺失”、数组长度、scalar value 都使用相同 `(independent_roots, pages)` 排序。任何最高分 tie 都返回成功态 `ambiguous` 且不带 `data`，所以 structural ambiguity 可以发生在没有任何 `field.ambiguous` 的响应中。只有全树唯一决定时才 canonical encode `complete` 数据；materialized `data` 独立上限 4 MiB。`extract` 随后再按 caller schema 校验，失败映射为 `schema_invalid + violations`，而不是把不合 schema 的文档冒充 complete。

**资源边界**：1..8 sources；URL 16 KiB；每源 JSON 4 MiB、合计 32 MiB；每源 depth 64 / nodes 20,000 / scalar leaves 10,000；path 4 KiB；evidence+receipt metadata 合计 32 MiB；field-consensus 完整编码 32 MiB。所有输入先 clone/strict decode，输出与输入顺序无关。

### 任务卡 P3-3 · sources[] 接线

**交付什么**：`ExtractRequest.Sources []string`；REST multi-source adapter/service；`MultiExtractResponse`；MCP `extract_data.sources[]`。

**状态**：🚧 REST/服务已提交 `31d44a7 feat(extract): multi-source consensus extraction`，MCP 已提交 **`43a1574 feat(mcp): support multi-source consensus extraction`**。代码与测试完整，但 production `extract.Service` 尚未收到 `SafeProxyURL`，因此当前 main binary 对 `sources[]` 会 fail-closed 为 503 `MULTI_SOURCE_UNAVAILABLE`；待授权的 shared-relay main 接线完成前，本卡不能标 ✅。

**验收标准**：
- [x] 单源失败不拖垮仍有 valid source 的整体；每个 canonical request source 都有 stable、sanitized summary
- [x] final URL 重复只保留 newest `fetched_at`（同时间按 request URL 排序）的 valid winner，其余标 `duplicate_of=<winner request URL>`
- [x] aggregate `complete|ambiguous|schema_invalid`、per-source status、schema violations、usage/timing 与 bounded response encoding 已实现
- [x] MCP 保留 legacy `url` 调用，并完整支持/严格解码 `sources[]`
- [ ] production main 创建并共享 process-owned safe relay，使 REST/MCP multi-source 路径实际可用

**REST request 契约**：整个 HTTP body 是 ≤1 MiB 的单个 strict JSON object（unknown field/trailing value 均拒绝）；`url` 与 non-nil `sources` **严格 XOR**。multi-source 要求 1..8 个 raw URL，每个 ≤16 KiB、合计 ≤128 KiB，先按 raw UTF-8 bytes 计费，再 canonicalize 为 public HTTP(S)、去重并排序。只支持默认 content profile，拒绝 request `proxy_url`；timeout 默认 30 秒，显式值必须在 1..120 秒。服务强制 `evidence=true`，并用 process-owned literal-loopback SOCKS5 relay 覆盖内部 scrape proxy，caller 无法指定出网边界。

```json
{
  "sources": [
    "https://one.example/product",
    "https://two.example/product"
  ],
  "schema": {
    "type": "object",
    "properties": {
      "name": { "type": "string" }
    },
    "required": ["name"],
    "additionalProperties": false
  },
  "engine": "auto",
  "timeout": 30
}
```

**并发与 admission**：每个请求 `errgroup.SetLimit(4)`，同时还有 `Service` 级全局 4-slot semaphore；source fetch/extraction、consensus CPU 与最终 JSON encoding 共用该全局上限。每源必须是 fresh 2xx artifact，具 snapshot ID/fetched_at/raw HTML，且 cleaned content/raw HTML 各自 ≤4 MiB；单源提取必须 full schema-valid，且每个 scalar leaf 都有非空 receipt 与 exact/normalized/fuzzy/compiled located anchor，anchor snapshot/time 必须匹配 artifact、quote byte range 必须真实落在 cleaned content，`unlocated_rate` 必须为 0。任何缺口只淘汰该源，不把不可定位事实送入 consensus。

**响应状态**：

| 层级 | 状态/错误 | wire 语义 |
|---|---|---|
| aggregate success | `complete` | 有 `consensus` 与 schema-valid `data`（合法 JSON `null` 也算 data） |
| aggregate success | `ambiguous` | 有 `consensus`、无 `data`；可能是 field tie，也可能只有 structural tie |
| aggregate success | `schema_invalid` | materialization 唯一但 caller schema 不通过；无 `data`，有非空 `violations` |
| per source | `valid` | 进入 consensus，带 canonical `final_url`、snapshot/tokens/timing/可选 LLM usage |
| per source | `duplicate` | 不进入 consensus；`duplicate_of` 精确指向 valid winner 的 request URL，final URL 相同 |
| per source failure | `timeout`, `fetch_failed`, `extraction_failed`, `partial`, `schema_invalid`, `evidence_unavailable` | 不含 provider 原始错误/credential，只返回 stable code/message |
| aggregate failure | `NO_VALID_SOURCE`, `MULTI_SOURCE_UNAVAILABLE`, `SCRAPE_TIMEOUT`, `LLM_AUTH_FAILURE`, `LLM_RATE_LIMITED`, `EXTRACTOR_UNAVAILABLE`, `INTERNAL_ERROR` | `success:false`，保留已完成的 per-source summaries；HTTP 分别按 502/503/504/401/429/409/500 映射 |

完整 `MultiExtractResponse` 编码上限 32 MiB；deadline 可取消 source work，并在 merge/final encode 前后重复检查，deadline 后产生的 bytes 不会返回（同步 merge/`json.Marshal` 本身不宣称可中途抢占）。aggregate tokens/timing 做 checked-add，`usage_complete` 明示失败/取消后 provider usage 是否可能不完整。

**MCP `extract_data`（✅ `43a1574`）**：tool schema 继续要求 `schema`，`url` 与 `sources` 均为可选属性，runtime 再执行严格 XOR，因此旧 `url+schema` client 不破坏。`sources` 的 1..8、单 URL 16 KiB、raw 合计 128 KiB 均在 HTTP 前按 bytes 复核；single payload 物理省略 `sources`，multi payload 物理省略 `url`，`engine=compiled` 物理省略全部 LLM fields。`PURIFY_API_KEY` 只进 `X-API-Key`，request BYOK 只在允许的 JSON body 路径出现。

2xx decoder 由 caller 选择的 target 决定单源还是 multi contract；multi 路径 strict 检查 UTF-8/EOF/unknown fields、三种 aggregate shape、mixed valid/duplicate/failure source、canonical URL/final URL/duplicate winner、authoritative eTLD+1/IP root、support↔valid snapshot evidence admission、agreement/N_eff 上界、winner/conflict score 顺序、每字段跨 value-group support URL 互斥，以及 consensus value 只能是 JSON scalar（`null|bool|number|string`）。它明确接受 `data:null` 和“无 field ambiguous 的 structural ambiguous”，同时拒绝 server 不可能生成的混合状态。

非 2xx 在 target decoder **之前**转成 MCP tool error，保留 stable code；Purify key 与 BYOK 都做 redaction。共用 response reader 接受恰好 32 MiB、拒绝 N+1 并丢弃 oversized body；production extract HTTP client 禁止所有 redirect，避免跨 origin 转发 `X-API-Key` 或 request body credential。成功返回 typed structured content + pretty JSON fallback。

**唯一待授权的 production diff 边界**：只在 `cmd/purify/main.go` 组合已存在的 helper——snapshot enabled 时创建一个 shared safe relay，交给 revisit 与 `extract.Config.SafeProxyURL`；创建独立于 managed compiler 开关的 `managedHealRuntime`；用 `api.NewRouterWithOptions(..., api.WithExtractorHealService(runtime))` 注入手动 route；按 compiler→heal→outbox→relay→HTTP-idles 的依赖顺序关闭。当前代码仍在 verify block 内单独 `StartDirectRelay`、仍以空 `SafeProxyURL` 构造 extract service、仍调用无 option 的 `api.NewRouter`，所以这不是文档推测，而是可 grep 的 fail-closed 实况。

**提交序列**：

```text
✅ 8a30f74  feat(consensus): merge sources with independence accounting
✅ 3590371  refactor(compiler): separate synthesis from publication
✅ e0e918c  feat(consensus): materialize unambiguous merged data
✅ 440bade  refactor(ledger): generalize durable outbox subjects
✅ 2f4f715  feat(compiler): replay verified healing candidates
✅ 135dba1  feat(compiler): schedule durable healing runs
✅ 31d44a7  feat(extract): multi-source consensus extraction
✅ 43a1574  feat(mcp): support multi-source consensus extraction
✅ 3017c9e  feat(main): share one safe relay across verify and multi-source extract
✅ e6fd741  feat(main): run production self-heal runtime
```

> **EN — (Phase 3 complete, HEAD `8565ac8`)** The durable core is implemented: synthesis and publication are separated; immutable candidates replay bounded, provenance-checked confirmed history under reclaimable durable leases; ≥90% with at least one fact promotes atomically, while insufficient or failing replay degrades without a silent swap. Consensus derives eTLD+1/IP roots, collapses same-root or SimHash-distance≤3 sources into N_eff components, preserves every evidence-backed conflict, and materializes typed JSON only when topology and values are unambiguous. REST and MCP `sources[]` ship with bounded fan-out, strict evidence/status/error contracts, credential separation, and fail-closed decoders. The previously pending production wiring landed: one snapshot-gated safe relay now feeds both verification revisit and multi-source extraction (`3017c9e`), and the self-heal worker runs in the production binary with an explicit post-drain shutdown order (`e6fd741`).

---

## 7. Phase 4 — Verified Search（W9–W10，分支 `codex/search-api-v1`）

**目标一句话**：按 `SEARCH.md` M0–M4 落地 provider 中立的 Search，再叠三个别人没有的参数：`verify` / simhash 去重 / `schema`。

> **实况对齐 · 2026-08-10（第二次回写，HEAD `8565ac8`）** —— 图例同前：✅ 已提交 · 🚧 部分 · ⟳ 与早期规划不同。
> 进度：**P4-1 ✅　P4-2 ✅　P4-3 ✅，Phase 4 收口**。生产 enrichment 接线已完成：`4e32af4` 使 `main.go` 在 snapshot 能力可用时以 `WithEnrichment(extractService, receiptSigner)` 构造 Search，`verify`/`schema`/`include_content` 在生产二进制真实可用。全仓测试 + vet 通过；非测试代码零 "calibrated"。

### 任务卡 P4-1 · M0–M1：契约、服务、首个 provider

**完成什么**：`SEARCH.md` 的请求/响应契约照建；provider 接口 + Brave 适配器。

**交付什么**：`search/service.go`、`search/providers/brave.go`、`models/search.go`、`api/handler/search.go`、router 注册。

**状态**：✅ 已提交 `130fee8`（models + provider 契约 + freshness/错误分类）、`19f61df`（Brave 适配器：专用 public-only transport、不跟随重定向、响应 2 MiB 上限 + 有界数组解码）、`a3f6622`（service + 归一化 + 60s/256 条/16 MiB baseline cache + simhash 连通分量去重）、`5460e70`（HTTP `POST /api/v1/search` + SearchAuth + 独立 capability 闸）、`27323ec`（生产 main 接线，`PURIFY_SEARCH_BRAVE_KEY`，request-driven 零后台工作）。⟳ 实际文件为 `search/brave.go`（非 `search/providers/`）；domain 过滤在 provider 响应后按 IDNA 规范化 + publicsuffix 校验执行，与 provider 语法解耦。

**验收标准**（含 SEARCH.md 自带 DoD）：
- [x] 公开契约零 provider 痕迹；超时/取消/错误映射齐（429→`ErrCodeRateLimited`）
- [x] 默认测试套件不需要真实 key（provider 打桩）

**实现参考**：

```go
type Query struct{ Text string; Limit int; Domains []string; Freshness string }
type Result struct{ Title, URL, Snippet string; Score float64; PublishedAt *time.Time }
type Provider interface {
    Name() string
    Search(ctx context.Context, q Query) ([]Result, error)
}
// Brave: GET api.search.brave.com/res/v1/web/search
// header X-Subscription-Token = cfg.Search.BraveKey；字段映射表写在 brave.go 顶部注释
```

### 任务卡 P4-2 · 差异化三参数

**完成什么**：
1. `verify: true`——top_k ≤ 5 并发实抓（复用 scraper），`AlignValue(snippet 主张 → 页面文本)`，404/失效结果剔除，命中附收据；
2. simhash 去重——`Distance(title+snippet 指纹) ≤ 3` 视为同文只留一条；
3. `schema: {...}`——逐结果走 extract（engine auto），搜索直接实体化。

**状态**：✅ 代码与测试已提交 `d8acbb4`（`evidence.AlignValueContext` 可取消对齐）+ `1c43a37`（enrichment：top-5 并发实抓、snippet 对齐 → 签名收据、simhash 去重默认开启、schema 逐结果 `ExtractArtifact` 复用同一次 fetch、有界编码）；生产接线 `4e32af4 feat(search): enable production result enrichment`（enrichment 随 snapshot 能力启停；baseline-only 时重参数继续 fail-closed）。⟳ 与规划的差异：404/410 才剔除进 `dropped_stale`；对齐失败不剔除而是 `verified:false` + `verification_status:"mismatch"`（不销毁 baseline 信息）；去重在 limit 截断前对全 baseline 做传递闭包（连通分量取最早 provider 排名者）。

**验收标准**：
- [x] verify:true 时响应每条含 `verified: true|false` 与 `receipt`；被剔除数量可见（`dropped_stale: n`）
- [x] 站群通稿在结果中只出现一次
- [ ] schema 模式复用 compiler 缓存（同站二次搜索显著变快）——接线已通，待真实流量实测后勾

### 任务卡 P4-3 · MCP `search_web`

照 SEARCH.md M3：同一 Search 服务，不另写逻辑。

**状态**：✅ 已提交 `1e047c2 feat(mcp): expose verified web search`。

**提交序列**（实际）：

```
✅ b4c245e  refactor(extract): expose public-only reusable artifacts   # FetchPublicArtifact/ExtractArtifact 边界
✅ 130fee8  feat(search): define request models and provider contract
✅ 19f61df  feat(search): add brave provider adapter
✅ a3f6622  feat(search): add search service and result normalization
✅ 5460e70  feat(search): expose search HTTP endpoint
✅ d8acbb4  refactor(evidence): add cancelable value alignment
✅ 27323ec  feat(search): wire baseline provider runtime
✅ 1c43a37  feat(search): enrich results with verified artifacts
✅ 1e047c2  feat(mcp): expose verified web search
✅ 4e32af4  feat(search): enable production result enrichment
```

> **EN —** Phase 4 executes SEARCH.md's M0–M4 (provider-neutral contract, Brave first, MCP `search_web`) and adds the three differentiators nobody ships: `verify:true` (re-fetch top-k, align the snippet claim, drop dead results, attach receipts), simhash dedup of syndicated copies, and `schema` for entity-shaped results reusing the compiled-extractor cache.

---

## 8. Phase 5–7 — 北极星：/answer、/watch、预测式新鲜度（2026Q4–2027Q1）

**Gate**：Phase 0–4 全部验收 + 出现付费流量后启动。

> **实况对齐 · 2026-08-10（第二次回写，HEAD `8565ac8`）** —— 实施已先于 Gate 启动（付费流量条件未满足即动工，属计划外提前；技术上依赖已就绪故无返工风险）。
> **P5 /answer**：核心与传输面 ✅ —— `617f1e8`（ledger 批量钩子）、`6e3691e`（`answer/service.go`：search→multi-extract→consensus 组装；两形态契约照建——tie→`conflicting_independent_sources`、`independent_roots < min`→`insufficient_independent_sources`，两者都带 `closest`/`needs`；confidence 只用 low/medium/high 档位；响应带固定 24h `lease` 块，形态即 P7 契约）、`0a9c92d`（HTTP `POST /api/v1/answer` + AnswerAuth + capability 闸）、`86a5cd0`（MCP evidence-backed answers）、**`8565ac8`（生产 main 接线：search 与 multi-source 双能力可用时构造，否则 fail-closed）——P5 生产可用**。
> **P6 /watch + facts**：核心已提交、HTTP 面在未提交 WIP —— `f848af6`（verify 对 gone verdict 也发 FactChange，含 `status`/`gone_scope`，不伪造替代证据）、`300e0bd`（`VerifyWithRecorder` 作用域事务记录器）、`174a911`（migration：双时态 `facts`（observed_at/valid_from/valid_to/superseded_by）+ `watches`（EWMA/lease/state 机），STRICT + 重 CHECK/触发器）、`b1ca162`（`watch/store.go`：lifecycle、keyset 分页、10k 活跃上限、3min 租约 claim）。原 WIP 已审查并落成提交：**`5a3dfee`（`watch/verification_recorder.go`：lease 绑定的原子 fact 物化，bootstrap/fact 双模式、幂等重放、EWMA 调度数学 0.3/0.7 + clamp 10m–7d；`watch/facts.go`：半开区间 as_of 查询 + 关系完整性校验）、`eff4047`（7 条路由：watch CRUD/pause/resume + `GET /api/v1/facts`，`WatchAuth` + router capability 闸）**。**scheduler ✅ `4d10f60`**（`watch/scheduler.go`：30s tick、每 tick ≤16 claim；bootstrap 用 Answer 信念取 baseline（evidence×receipts 按规范化 URL 配对）、fact 模式把开放 fact 的收据作 receipt 形态 `VerifyRequest` 走 `VerifyWithRecorder`；bootstrap changed 允许恰好一次 refreshed-baseline 跟进、二次 changed 记 `WATCH_BOOTSTRAP_UNSTABLE`；失败 `ReleaseLease` 指数退避 10m×2ⁿ 封顶 7d；⟳ 顺带扩展 bootstrap recorder 允许 active+无开放 fact，补齐 fact-gone→重建闭环）。**main 接线 ✅ `fb68193`**（`managedWatchRuntime` 随 verify+answer 双能力启停——不能复验就不收 watch；scheduler 纳入 shutdown 顺序首位，先于 relay/outbox 关停）。**P6 代码收口**；⬜ 仅剩 commons 冷启动 ≥30 条（数据录入，非代码）。
> **P7**：`lease` 响应块已随 P5 落地（固定 24h + 1 周半衰期提示）；自适应间隔与续租语义 ⬜。

### 任务卡 P5 · /answer 信念模式

**完成什么**：search → 多源 extract → consensus → 信念响应；**n_eff 不足时诚实返回 unknown**。

**实现参考**（两形态契约，管线 = 已有件组装）：

```jsonc
// POST /api/v1/answer
{ "spec": { "subject": "anthropic claude-fable-5",
            "predicate": "price_per_mtok_input",
            "freshness": "7d",
            "min_independent_sources": 2,
            "on_conflict": "expose" } }

// 形态 A
{ "belief": { "value": "$…", "confidence": "high",
    "agreement": { "pages": 6, "independent_roots": 3 },
    "as_of": "2026-11-02T02:10:00Z",
    "evidence": [ { "quote": "…", "selector": "…", "snapshot_id": "sha256:…", "root": "anthropic.com" } ],
    "receipts": { "…": "…" } } }

// 形态 B（行业没人敢返回的那种）
{ "belief": null, "status": "unknown",
  "needs": { "more_independent_sources": 1 },
  "closest": { "value": "$…", "independent_roots": 1, "note": "single syndicated root" } }
```

**验收**：n_eff < spec.min 时必须走形态 B；confidence 字样只用档位（high/medium/low），**"calibrated" 一词在校准数据成熟前禁止出现在文档与响应**。

### 任务卡 P6 · /watch + 双时态事实账本 + as_of

**交付什么**：migration 003；`watch/scheduler.go`；watch CRUD 端点；`GET /api/v1/facts?subject&predicate&as_of`。

**实现参考**（migration 003 + 调度伪码）：

```sql
CREATE TABLE facts(
  id INTEGER PRIMARY KEY,
  subject TEXT, predicate TEXT, value TEXT, root TEXT, receipt TEXT,
  observed_at TEXT,          -- 我们何时看到（transaction time）
  valid_from TEXT, valid_to TEXT,  -- 网页世界何时如此（valid time）
  superseded_by INTEGER
);
CREATE INDEX idx_facts_sp ON facts(subject, predicate, valid_from);

CREATE TABLE watches(
  id INTEGER PRIMARY KEY,
  spec TEXT, state TEXT,
  next_check_at TEXT, ewma_interval_s REAL,
  last_change_at TEXT, created_at TEXT
);
```

```text
scheduler (30s tick):
  due = watches where next_check_at <= now
  对每个 due: 走 verify 管线
    changed  → 旧 fact 封口(valid_to=now, superseded_by=新id) + 插新行
               ewma = 0.3*observed_interval + 0.7*ewma
               next = now + clamp(ewma/2, 10m, 7d)
    unchanged→ next = now + clamp(ewma*1.5, 10m, 7d)
as_of 查询: WHERE valid_from <= :t AND (valid_to IS NULL OR valid_to > :t)
```

**Commons 冷启动**（/watch 上线第一天启动，不可回填的历史从此积累）：首批公开垂类 = AI 厂商 pricing/models/limits。spec 示例：

```jsonc
{ "subject": "anthropic.com claude-fable-5", "predicate": "price_per_mtok_input",
  "url": "https://www.anthropic.com/pricing", "freshness": "1d",
  "schema": { "type": "object", "properties": { "price_per_mtok_input": { "type": "string" } } } }
// 同构 spec: openai.com 模型价格页、deepseek.com 定价页 …… 首批 ≥30 条
```

### 任务卡 P7 · 预测式新鲜度与租约

**完成什么**：staleness 重定义为置信衰减；所有事实响应带租约块；freshness SLA 产品化。

**实现参考**：

```jsonc
"lease": { "expires_at": "…",                  // verified_at + clamp(ewma/2, …) 或默认 24h
           "renew_url": "/api/v1/verify",
           "confidence_halflife_s": 604800 }
```

> **EN —** The north-star phases compose existing organs: /answer returns beliefs with independent-root agreement and an honest `unknown` shape; /watch writes verified changes into a bitemporal facts ledger with `as_of` queries, seeded from day one by a public commons (AI-vendor pricing/models/limits); freshness becomes an EWMA-scheduled lease (`expires_at`, `renew_url`) instead of a TTL guess. The word "calibrated" is banned until the calibration dataset is real.

---

## 9. 极限层工件排期 · Limit-Tier Instruments

| 工件 | 最小可发布形态 | 前置数据条件 | 落点 |
|---|---|---|---|
| 事实收据 | JWS 收据 + 免费公共验证端点（已含于 P0-5）；规范独立成文档 + 开源验证 CLI | 无 | Phase 0 内置；spec 开源在 W4 前后 |
| 信念租约 | 响应 `lease` 块 + /verify 续租语义 | /verify 上线 | Phase 5 起默认携带 |
| 传播制图 | `facts.observed_at` 先后推导"谁抄谁"（SQL 分析即可起步）；产品化：原创检测、上游源推荐 | watch 语料 ≥ 3 个月 | 2027Q1 v0 |
| 追溯审计 | `POST /api/v1/audit`：claims[] + timestamp → 逐条 true-then / false-then / changed-since / uncovered | 账本覆盖 ≥ 5 垂类 | 2027Q1 预览 |
| 真值担保 | 高置信答案 100× 请求价赔付额度（封顶）；先跑**影子赔付**（内部记账不对外承诺） | 校准损失表 ≥ 6 个月 + 法务审 | 压轴 |

zkTLS（Reclaim/TLSNotary）定位一句话：**它证传输，我们证语义与新鲜度**——互补轨道，后期给收据加共同见证选项，不自研密码学。

> **EN —** Five instruments ride the dependency ladder: receipts (built in Phase 0, spec open-sourced), leases (free once /verify exists), propagation cartography (emerges from ≥3 months of watch data via observed_at precedence), retroactive audit (needs ledger coverage), and the truth warranty last (≥6 months of shadow-payout loss data plus legal review). zkTLS proves transport; we prove semantics — complementary, adopt later.

---

## 10. 评测与 CI · Evaluation & CI

**完成什么**：速度基准之外补准确率基准；准确率回退在 CI 红灯。

**交付什么**：
- `scripts/benchmark` 增 `accuracy` 子命令：
  - `-dataset replay`：`testdata/pages/` 快照重放（**修一个提取 bug 就固化一个 fixture**，永不删）
  - `-dataset swde -sample 5`：SWDE 子集（8 垂类 × 5 站，语料放 `testdata/swde/`），输出逐字段 F1
- CI：现有 release/docker workflows 之上加 test job——replay 必跑（快、无网络）；swde nightly

**验收标准**：
- [ ] replay 集在 CI 每次 PR 必跑，任何字段 F1 下降即失败
- [ ] SWDE 基线数字写进本文档 §11 并随版本更新

> **EN —** Extend `scripts/benchmark` with an `accuracy` subcommand: a snapshot-replay fixture set (every extraction bugfix adds a permanent fixture) gating every PR, plus a sampled SWDE suite (8 verticals × 5 sites) running nightly with per-field F1.

---

## 11. KPI 指标体系 · KPIs

| 指标 | 目标 | 口径 |
|---|---|---|
| unlocated 率 | < 5% | evidence:true 响应的滑动 7 日均值 |
| compiled 命中率（重复站点） | > 70% | 同 (host, schema) 第 ≥4 次请求走 compiled 的比例 |
| compiled 路径延迟 | P95 < 50ms | Execute 段计时，零 LLM 调用 |
| /verify 延迟 | P95 < 3s | 端到端（多引擎竞速内） |
| SWDE F1 | 不回退 | §10 基线 |
| LLM 成本 / 千次提取 | 持续下降 | compiled 占比上升驱动 |
| 收据外部验证调用 | 增长 | 免费端点 = 获客漏斗（无 key 调用数） |
| watch 存量 / commons 垂类 | ≥30 条起步 | P6 上线日起 |
| 校准样本量 | 单调增长 | verifications 表行数（confirmed+changed） |

> **EN —** Nine KPIs: unlocated <5%, compiled hit-rate >70% on repeat sites at P95 <50ms, verify P95 <3s, SWDE F1 never regresses, LLM cost per 1k extractions trending down, free receipt-verification calls as the top-of-funnel metric, watch inventory, and monotonically growing calibration samples.

---

## 12. 风险登记册 · Risk Register

| 风险 | 影响 | 缓解 |
|---|---|---|
| Firecrawl 功能跟进快 | 单点功能被抄 | 壁垒押在数据复利（快照史/验证史/传播图）而非功能本身；每 Phase 独立可发布抢时间窗 |
| 存储增长 | 磁盘成本 | zstd 后 ~20KB/页，100 万页 ≈20GB；retention 配置 + CAS 天然去重 |
| 反爬升级 | verify 成本上升 | 多引擎竞速 + stealth 已有；watch 调度让重访频率∝变化率而非轮询 |
| 担保法务 | 赔付条款风险 | 影子赔付先行 ≥6 个月；上线前法务审；封顶额度 |
| 单人带宽 | 进度风险 | 任务卡粒度独立可发布、可独立创收；严禁跨卡并行超过 2 张 |
| SQLite 并发上限 | 高并发写瓶颈 | WAL + 单写序列化；表设计无 SQLite 专有语法，可平移 Postgres |
| LLM provider 依赖 | 编译/修复不可用 | BYOK 多供应商（OpenAI 兼容面）已缓解 |

> **EN —** Top risks: fast-following competitors (mitigated by data-compounding moats, not features), storage growth (trivial at zstd scale), anti-bot escalation (racing engine + change-rate scheduling), warranty legal exposure (shadow payouts first), solo bandwidth (independently shippable cards), SQLite write ceiling (WAL now, Postgres-portable schema later).

---

## 13. 边界与工程纪律 · Boundaries & Discipline

1. **AGENTS.md 全文有效**：Search 工作在 `codex/search-api-v1`；scrape 线在 `codex/scrape-v1` 系分支。
2. 每 commit 单一关注；code commit 前 `go test ./...` + `git diff --check`；提交前 `git status --short` 排除无关文件。
3. **所有 commit 不带任何 AI 署名 / Co-Authored-By 尾注。**
4. 不 push、不 merge、不 tag、不部署，除非明确决定。
5. LCI/lithium、多模态、DataOS/同化引擎代码**一律不进本仓库**；同化引擎只借鉴接口语义（FactSpec/N_eff/证据收据），在 purify 全新实现。
6. 措辞纪律：校准数据成熟前，任何对外文案与 API 响应不得使用 "calibrated"；担保上线前不得预售"保证正确"。
7. 公开契约向后兼容：新增字段一律 `omitempty`，已有字段不改名不删除。

> **EN —** AGENTS.md governs: search on its own branch, one concern per commit, tests before every code commit, no AI attribution in commit messages, no push/merge/deploy without an explicit decision, no LCI/multimodal/DataOS code in this repo, no "calibrated" wording before the data exists, and strictly additive public contracts.

---

## 14. 里程碑时间线 · Timeline

| 周 | 里程碑 | 可对外发布物 | 验收口径 |
|---|---|---|---|
| W1–W2 | Phase 0 地基 | "每个字段带收据"+免费收据验证端点 | §3 五卡全勾，evidence P95 +<30ms |
| W3 | Phase 1 复验 | /verify + MCP verify_fact | §4 三卡全勾，裁决入库 |
| W4–W6 | Phase 2 编译 | "透明确定性提取"（对标 deterministicJson 黑盒） | compiled P95<50ms、validation 亮牌 |
| W7–W8 | Phase 3 自愈+共识 | n_eff 独立性计数 + 冲突暴露 | 回归晋级制生效 |
| W9–W10 | Phase 4 搜索 | Verified Search（verify/去重/schema 三参数） | SEARCH.md DoD + 三参数验收 |
| W11–W13 | Phase 8 EAV 实体归因（§15，PLAN.md §6 步 2） | right-source-wrong-entity 报警（Parallel 结构做不到的洞） | 跨域标注集 P>90% / R>90% / 干净误报<2% |
| 2026 Q4 | Phase 5–6 | /answer（敢答 unknown）+ /watch + commons ≥30 垂类上线 | 形态 B 可复现；账本开始积累 |
| 2027 Q1 | Phase 7 + 极限层前两件后续 | freshness SLA + 传播制图 v0 + 审计预览 | lease 默认携带；as_of 查询公开 |

**第一行动（本周）**：开卡 P0-1 严格 schema 校验——它同时是当前 `/extract` 最大质量短板与整个证据体系的第一块砖。

> **EN —** Ten weeks to ship tiers one through four milestone by milestone — receipts (W2), /verify (W3), transparent compiled extraction (W6), independence accounting (W8), verified search (W10) — then the north-star quarter (answers, watches, commons) and the 2027Q1 limit-tier openers. First action: task card P0-1, strict schema validation.

---

## 15. Phase 8 — EAV 实体归因校验（PLAN.md §6 步 2）

> **状态**：executing — 2026-08-10 拆卡并开工。进度：E-1 ✅（`612fd50`）　E-2 ✅（`b5be194`）　E-3…E-6 ⬜。图例沿用：✅ 已提交 · 🚧 进行中 · ⬜ 未开始 · ⟳ 与规划不同（以实码为准）。
> **上游依据**：PLAN.md §5.3（算法与验收）、§6 步 2（顺序）。**落点定案：新包 `verify/eav/`**——不做 evidence/ 扩展：evidence 管「值在哪」（定位），eav 管「这页在讲谁」（判断），职责不同。
> **一句话**：抓 right-source-wrong-entity——系统如实引用了真实文档、每个字都锚得上，但文档说的是 B，你问的是 A。对幻觉检测、忠实度、引用核查全部隐形；Parallel Basis 结构上抓不到。

### 15.0 设计定案（先读，防漂移）

六条，全部有意为之：

1. **盲抽取（blind extraction）**：主实体抽取的输入**永不包含查询实体**。先独立回答「这份文档在讲谁」，再比对「和问的是不是同一个」。抽取一旦被查询污染就是确认偏误——论文级 0% 干净误报做不到的根源。
2. **三态裁决**：`entity_match / entity_mismatch / entity_uncertain`，与 verify 的 supported / refuted / unlocatable 同哲学：不确定就说不确定。**报警（mismatch）只能来自确凿证据**；抽不出主实体、锚不回原文、LLM 出错，一律 uncertain，绝不升级成报警。干净样本误报 < 2% 是硬门，这条纪律是达标前提。
3. **抽取选型：候选板 + LLM 选择题（混合），不是纯 NER 也不是纯 LLM**：
   - 纯 Go NER（jdkato/prose 一类）：英文单语、少维护、给的是 token span 不是「这页在讲谁」（aboutness）→ ❌。
   - ONNX 多语 NER 模型：引入 cgo/onnxruntime 重依赖，违反「只加 3 个依赖」纪律，同样不解决 aboutness → ❌。
   - 纯 LLM 自由生成：会幻觉出页面上不存在的实体名，不可锚定 → ❌。
   - ✅ **确定性信号收割候选板**（title / h1 / og:* / JSON-LD mainEntity / URL slug / 词频，零成本、可单测）→ **LLM 在候选板内做选择题 + 定类型 + 合并别名**（strict JSON schema）→ **每个输出必须能用 `evidence.AlignValue` 锚回原文**，锚不上即丢弃。选择而非生成，幻觉面收敛到零，且与本仓「透明可校验提取」的 DNA 一致。
4. **确定性比对阶梯先行，LLM referee 只管灰区**：exact / alias / 仅法律后缀差异 → match；相似度低于地板 → mismatch；**词形接近但未经别名验证的（Apple Inc vs Apple Bank、Metformin vs Metformin HCl ER、AMD vs ARM）才进 referee** 做同指裁决。referee 可关（关= 灰区一律 uncertain）——确定性内核先独立成立、独立测试。
5. **文档级主实体，不做 claim 级**：v1 每个来源文档判一次主实体。/answer 是单事实×单文档，文档级≈claim 级；逐 anchor 上下文归因是后续精化，不进本相位。
6. **公开契约只加不改**：请求侧可选 `expected_subject`，响应侧 `entity` 全部 `omitempty`（§13 纪律 7）。

**诚实边界（不假装解决）**：同形异指（homonym——subject 字符串 "Georgia" 到底指州还是国）在纯字符串 subject 下不可判，v1 判 match；缓解靠 `Subject.Hint`（/answer 把 predicate 传进来给 referee 当消歧上下文），根治需要 FactSpec 类型化实体，记 backlog 不记本相位。

> **EN —** Locked design: blind extraction (the query never contaminates entity extraction), three-state verdicts where alarms require hard evidence, a slate-then-LLM-choice extractor whose every output must re-anchor through evidence.AlignValue, a deterministic match ladder with an LLM referee only for the confusable gray zone, document-level primary entities, and strictly additive wire contracts. Honest limit: homonym subjects are out of scope in v1.

### 15.1 包结构与依赖

```text
verify/eav/                  # package eav — 传输中立判断核心
  eav.go                     # 包文档、资源上限、错误、核心类型
  normalize.go               # 规范化：宽度折叠/大小写/标点/法律后缀/冠词
  match.go                   # 确定性比对阶梯（纯函数，无 LLM）
  slate.go                   # 候选板收割（title/h1/og/JSON-LD/slug/词频）
  extract.go                 # EntityExtractor 盲抽取边界 + 锚定校验
  referee.go                 # Referee 灰区同指裁决边界
  judge.go                   # Judge 编排：收割→盲抽取→阶梯→referee→Judgment
  *_test.go                  # 与实现文件一一对应的表驱动测试
  testdata/
    harvest/*.html           # 候选板收割用完整 HTML 样张（少量）
    golden/*.jsonl           # 跨域标注集（收割后快照，不存全 HTML）
    recordings/*.json        # LLM 录制回放（离线评测全链路）
scripts/eavcorpus/           # 标注集构建器（E-5，独立 main，不进生产二进制）
```

**依赖方向（锁定）**：`eav` 只依赖 `evidence`（锚定）、`goquery/cascadia`（已有依赖，读 og/JSON-LD）与标准库。**不依赖 `llm` / `models` / `verify` / `consensus`**——LLM 适配器与 wire 类型都在接线层（E-6），方向同 `compiler.TruthExtractor`：密钥、provider、修复、HTTP 全在调用方 adapter。

### 15.2 核心类型与接口签名（`eav.go`，E-1 落地）

```go
// 资源上限（沿用各包 Max* 惯例；Subject 上限与 models.MaxAnswerSubjectBytes 数值对齐但不引包）
const (
    MaxSubjectBytes    = 1200      // 300 runes × 4
    MaxHintBytes       = 512       // 与 MaxAnswerPredicateBytes 对齐
    MaxDocumentBytes   = 4 << 20   // 与 verify.maximumPageBytes 对齐
    MaxHeadWindowBytes = 8 << 10   // 送 LLM 的头窗：title + cleaned 前缀
    MaxCandidates      = 24        // 候选板上限
    MaxAliases         = 8
    MaxSecondary       = 8
    MaxEntityBytes     = 512       // 单个实体表面形上限
    MaxQuoteBytes      = 1 << 10
)

var (
    ErrInvalidInput  = errors.New("eav: invalid input")
    ErrNotConfigured = errors.New("eav: judge is not configured")
)

// Kind 是开放网络的粗粒度实体类型。
type Kind string
const (
    KindOrganization Kind = "organization"
    KindPerson       Kind = "person"
    KindProduct      Kind = "product"
    KindPlace        Kind = "place"
    KindEvent        Kind = "event"
    KindWork         Kind = "work"      // 论文/影视/书目等作品
    KindSubstance    Kind = "substance" // 药品/化学品——论文原始域
    KindOther        Kind = "other"
)

// Subject 是查询侧实体。Hint 是可选消歧上下文（/answer 传 predicate），
// 只进 referee 提示，绝不进盲抽取。
type Subject struct {
    Name string
    Hint string
}

// Document 是一份待判文档。Cleaned 必填；RawHTML 供候选板读 og/JSON-LD，可空。
type Document struct {
    URL     string
    Title   string
    Cleaned string
    RawHTML string
}

// Candidate 是候选板一项。Quote 必须逐字出现在 Title/Cleaned 中。
type Candidate struct {
    Surface string
    Signal  string // "title" | "h1" | "og:title" | "og:site_name" | "jsonld" | "slug" | "frequency"
    Quote   string
}

// Entity 是文档主实体及其同文档别名（ticker、简称、曾用名）。
type Entity struct {
    Name    string
    Kind    Kind
    Aliases []string
    Quote   string // 证明主实体身份的最短原文引用
}

// DocumentEntities 是盲抽取结果。Primary 为 nil = 无法确定唯一主实体
// （列表页/比较页/论坛聚合），这是合法输出而非错误。
type DocumentEntities struct {
    Primary   *Entity
    Secondary []Entity
}

// Verdict 三态。报警只有 entity_mismatch 一种。
type Verdict string
const (
    VerdictMatch     Verdict = "entity_match"
    VerdictMismatch  Verdict = "entity_mismatch"
    VerdictUncertain Verdict = "entity_uncertain"
)

// MatchTier 记录裁决在哪一级达成，供响应与评测分层。
type MatchTier string
const (
    TierExact   MatchTier = "exact"
    TierAlias   MatchTier = "alias"
    TierSuffix  MatchTier = "suffix"  // 仅法律后缀/冠词差异
    TierFloor   MatchTier = "floor"   // 相似度低于地板 → mismatch
    TierReferee MatchTier = "referee" // 灰区同指裁决
    TierNone    MatchTier = ""        // uncertain
)

// MatchResult 是确定性阶梯（不含 referee）的输出。
type MatchResult struct {
    Verdict    Verdict
    Tier       MatchTier
    Similarity float64 // 诊断用；契约以 Verdict 为准
}

// Judgment 是完整裁决。Evidence 是 DocEntity.Quote 在 Cleaned 中的锚点
// （复用 evidence.AlignValue），mismatch/match 时必非 unlocated。
type Judgment struct {
    Verdict    Verdict
    Tier       MatchTier
    Subject    string           // 规范化后的查询实体
    DocEntity  *Entity          // uncertain 时可为 nil
    Evidence   *evidence.Anchor
    Similarity float64
}

// ── 边界接口（传输中立，adapter 归接线层）─────────────────────────────

// EntityExtractor 是盲抽取边界：输入永不包含 Subject。
type EntityExtractor interface {
    ExtractEntities(ctx context.Context, doc Document, slate []Candidate) (DocumentEntities, error)
}
type EntityExtractorFunc func(context.Context, Document, []Candidate) (DocumentEntities, error)

// Referee 对灰区做同指裁决。Different 必须携带区分性原文引用。
type Referee interface {
    SameReferent(ctx context.Context, subject Subject, entity Entity, doc Document) (RefereeVerdict, error)
}
type RefereeAnswer string
const (
    RefereeSame      RefereeAnswer = "same"
    RefereeDifferent RefereeAnswer = "different"
    RefereeUnsure    RefereeAnswer = "unsure"
)
type RefereeVerdict struct {
    Answer RefereeAnswer
    Quote  string // Answer==Different 时必填，且必须锚回文档
}

// ── 纯函数层（E-1/E-2，无 LLM，全部可独立单测）────────────────────────

func Normalize(surface string) string                    // 宽度折叠/小写/标点/空白，方向同 evidence.normalizeText
func StripLegalSuffix(normalized string) (string, bool)  // inc/corp/ltd/gmbh/株式会社/有限公司…
func Match(subject Subject, entity Entity) MatchResult   // 确定性阶梯
func HarvestCandidates(doc Document) []Candidate         // 候选板收割

// ── 编排（E-4）───────────────────────────────────────────────────────

type Config struct {
    Extractor EntityExtractor
    Referee   Referee // 可为 nil：灰区一律 uncertain
}
func NewJudge(config Config) (*Judge, error)
func (j *Judge) JudgeDocument(ctx context.Context, subject Subject, doc Document) (Judgment, error)
```

### 15.3 确定性比对阶梯（`match.go` 语义，E-1 的测试即规格）

```text
n(s)  = Normalize(subject.Name)
n(e)  = Normalize(entity.Name)，别名同样规范化
base* = StripLegalSuffix 后的形式

1. n(s) == n(e)                                → match / exact
2. n(s) == 任一 n(alias)                        → match / alias
3. base(s) == base(e) 且余量仅为法律后缀/冠词      → match / suffix
   （"apple inc" vs "apple" ✓；"apple bank" vs "apple" ✗——余量 "bank" 不在后缀白名单）
4. sim(s,e) < simFloor(=0.30)                  → mismatch / floor   ← 唯一的确定性报警出口
5. 其余（灰区：词形接近但非别名验证）              → referee 开：same→match、different→mismatch、unsure→uncertain
                                                  referee 关：uncertain
```

- `sim` = token 集 Jaccard 与字符 trigram Jaccard 取大者（trigram 覆盖中文等无空格文本）；两者都在规范化形式上算。
- 阈值常量集中在 `match.go` 顶部并写明「由 E-5 标注集校准，改动必须过 golden 门」。
- 对抗样例进 E-1 表测试：Apple Inc/Apple Bank、AMD/ARM、Metformin/Metformin HCl ER、Georgia/Georgia、小米/红米、iPhone 15/iPhone 15 Pro。

### 任务卡 E-1 · 核心类型 + 规范化 + 确定性比对 ✅

**状态**：✅ 已提交 `612fd50 feat(eav): normalize and match entity surface forms`。⟳ 与规划的差异：(a) 边界接口（EntityExtractor/Referee）未随 `eav.go` 落地——按单关注原则归 E-3/E-4 的文件，E-1 只落数据类型/常量/错误；(b) 阶梯在 floor 前新增三个**只降不升**（只能把报警降为 uncertain、不能反向）的守卫：near-typo 编辑距离门（d≤2 才计相似度，长名不再靠稀释比率混进灰区）、initialism 守卫（"IBM" vs "International Business Machines" 永不确定性报警）、containment 守卫（子串关系进灰区，全 CJK 最短 2 字、其余 3 字）；(c) 超限输入（subject/实体/别名过长、别名数超上限）一律拒判为 uncertain，不做部分裁决。对抗样例实测：AMD/ARM、小米/红米、湖南/湖北、Metformin/Metformin HCl ER 全部落灰区；Apple Inc/Samsung Electronics、宁德时代/比亚迪等确凿异体落 floor 报警；Georgia/Georgia 同形异指按 v1 边界判 match（测试内注明）。

**交付什么**：`eav.go` + `normalize.go` + `match.go` 与全部表驱动测试。§15.2 的类型/常量/错误 + §15.3 的阶梯语义。纯函数，零 IO，零新依赖。
**怎么做**：法律后缀表覆盖 en（inc/corp/co/ltd/llc/plc/ag/gmbh/sa/nv/oyj）+ zh/ja（有限公司/股份有限公司/集团/株式会社/合同会社）；冠词只剥前导 "the "。规范化方向与 `evidence.normalizeText` 一致（宽度折叠、小写、空白折叠），但**保留字母数字外的内部符号语义**（"HCl ER" 的空格切词决定 token 差异可见）。
**测试**：每一级阶梯至少 4 正 4 反；对抗样例全绿；`Normalize` 幂等性（`Normalize(Normalize(x))==Normalize(x)`）。
**验收**：`go test -race ./verify/eav/` 绿；无 LLM、无网络。
**提交**：`feat(eav): normalize and match entity surface forms`

### 任务卡 E-2 · 候选板收割 ✅

**状态**：✅ 已提交 `b5be194 feat(eav): harvest deterministic entity candidates`。五类样张全等断言（整板逐项含 Signal/Quote）+ 不变量测试（锚定/去重/上限/确定性跑两遍）。基准 52µs/页（news 样张），验收 <5ms 余量两个量级。⟳ 与规划的差异：(a) Signal 字面量导出为包契约（`SignalTitle` 等，供 E-3 提示词与响应词表复用）；(b) title 分隔符在卡列三种之外补了带空格 em dash、全角｜、裸 `|` 与 `_`（中文门户标题惯例）；(c) 锚定检查加了 **ASCII 大小写不敏感回退并采纳文档原始大小写**——slug "framework-laptop-16" 由此还原成文档里的 "Framework Laptop 16"，非 ASCII 仍严格逐字；(d) JSON-LD 只沿 `@graph`/`mainEntity`/`about` 边走 + 根层 typed 节点，publisher/offers/itemListElement 永不漏进候选（列表页保证即由此而来），按命名键访问、不 range map，保确定性；(e) 词频信号实现为：拉丁大写词连跑（允许 of/de 类连接词与尾随数字，"Bank of America"/"iPhone 15" 存活，单词需 ≥3 次 + 停用词表，词组 ≥2 次）+ 中文 Han 2–6 字 n-gram（频次×长度计分、贪心去重叠、跳过法律后缀碎片）；(f) 超限字段视为缺席，不部分处理。
**怎么做**：信号优先级 title 分段（按 ` | `、` – `、` - ` 剥站名）> h1 > og:title/og:site_name > JSON-LD（`mainEntity`/`about`/Product|Organization|Person 的 `name`，goquery 解析 `script[type="application/ld+json"]`，畸形 JSON 静默跳过）> URL slug 还原 > cleaned 头窗高频大写 n-gram/中文连续名词串。**每个候选的 Quote 必须逐字出现在 Title 或 Cleaned**（`strings.Contains` 级检查），不满足即丢弃；去重按 `Normalize` 后表面形合并、保留最高优先级 Signal；上限 `MaxCandidates`。
**测试**：每类样张断言候选板含预期主实体表面形；列表页样张断言不产出唯一压倒性候选（为 Primary=nil 留通路）。
**验收**：收割 P95 < 5ms/页（纯解析，无网络）。
**提交**：`feat(eav): harvest deterministic entity candidates`

### 任务卡 E-3 · 盲抽取边界 + 锚定校验 ⬜

**交付什么**：`extract.go`：`EntityExtractor` 接口 + `AnchoredExtraction` 校验包装——不管 extractor 实现是什么，输出一律过三道闸：(1) `Primary.Name`/每个 `Alias`/`Quote` 长度与计数上限；(2) `Quote` 用 `evidence.AlignValue` 锚回 `Cleaned`，`unlocated` ⇒ 丢弃该实体；(3) Primary 被丢弃 ⇒ 整体降级为 `DocumentEntities{Primary: nil}`（→ uncertain），**绝不报错升级**。
**怎么做**：抽取输入只有 `Document`（头窗裁剪到 `MaxHeadWindowBytes`）+ 候选板。LLM 提示词模板与 strict JSON schema（`{"primary":{"name","kind","aliases"},"reason_if_none"}`，name 必须从候选板 Quote 中选）以常量形式放 `extract.go`，供接线层 adapter 复用——**adapter 本体（`llm.Client` 接线、BYOK/managed key、修复重试）在 E-6，不进本包**。
**测试**：fake extractor 注入：合法输出通过；幻觉名（不在文档中）被锚定闸拦下；超限被拒；LLM error → Primary=nil 而非 error 上抛（error 只在 ctx 取消时上抛）。
**提交**：`feat(eav): extract the primary document entity blind`

### 任务卡 E-4 · Judge 编排 + referee 灰区裁决 ⬜

**交付什么**：`judge.go` + `referee.go`：`NewJudge`/`JudgeDocument` 按 §15.3 编排；referee strict schema（`{"answer":"same|different|unsure","quote"}`）与提示词常量（输入含 Subject.Hint 作消歧上下文）；`RefereeVerdict.Answer==Different` 时 `Quote` 必须锚回文档，锚不上 ⇒ 降级 unsure。
**怎么做**：裁决优先级固定：盲抽取 Primary=nil ⇒ uncertain 直接返回；阶梯 1–4 命中即返回；灰区才碰 referee；referee nil/error/unsure ⇒ uncertain。`Judgment.Evidence` 在 match/mismatch 时必须存在（来自 Primary.Quote 或 referee 区分性 Quote 的锚点）。**误报纪律以断言写死在测试里：mismatch 只可能出自 Tier ∈ {floor, referee}。**
**测试**：全路径表测试（fake extractor + fake referee 的笛卡尔组合）；ctx 取消在每个边界立即返回。
**提交**：`feat(eav): judge subject attribution three ways`

### 任务卡 E-5 · 跨域标注集 + 评测门 ⬜

**交付什么**：`scripts/eavcorpus/`（构建器）+ `testdata/golden/*.jsonl`（≥300 行、6 域）+ `testdata/recordings/`（LLM 录制）+ `golden_test.go`（指标门）。
**标注集怎么起（锁定方法：真实文档 × 同类扰动配对，不手写文档）**：
1. **同类清单挖兄弟**：6 个域各取一份公开类目清单——公司（US+CN 上市公司名录）、药品（FDA/NMPA 常用药，对齐论文原始域）、产品（手机/相机型号）、人物（同名近名公众人物）、地名（Georgia/Jordan/湖南-湖北类）、事件（年度峰会/条约版本）。
2. **真实抓取**：构建器用现有 scrape 栈每域抓 ~10–15 个真实页面，**fixture 存收割后快照**（`{url,title,cleaned≤8KB,slate}` JSONL），不存全 HTML——收割本身由 E-2 的 HTML 样张单测覆盖。
3. **程序化配对**：正例 =（A 的页, subject=A）与（A 的页, subject=A 的别名/ticker/简称）；负例 =（B 的页, subject=A），其中 B 为 A 的同类兄弟；**hard 负例** = 兄弟中 `Normalize` 后编辑距离 ≤0.35 或 token 重叠 ≥0.5 的词形混淆对（AMD/ARM 类），打 `hard:true`。
4. **人工审计**：随机 10% + 全部 hard 对逐行过目改标；行 schema `{subject,hint?,doc_ref,label:"match|mismatch",hard,domain,note}`。
5. **LLM 录制回放**：录制 adapter 把（提示词 sha256 → 响应）写入 `recordings/`；golden 测试用回放 fake 跑**全链路**（收割→盲抽取→阶梯→referee），离线、确定性、免 key；重录用 `PURIFY_EAV_RECORD=1` + BYOK env 手动触发。
**指标定义（写进 `golden_test.go`，即 PLAN.md §5.3 验收的可执行形式）**：
- 报警精度 P = 判 mismatch 且标 mismatch / 判 mismatch，**门 > 0.90**
- 报警召回 R = 判 mismatch 且标 mismatch / 标 mismatch（uncertain 计入漏报，从严），**门 > 0.90**（全链路回放模式）
- 干净误报率 = 标 match 判 mismatch / 标 match，**门 < 0.02**
- uncertain 率无门但必须打印（诚实成本可见）；另按 domain × hard 分层打印。
- 纯确定性模式（referee 关）单独跑：只门干净误报 < 0.02 与 P > 0.90，不门召回（灰区全 uncertain，召回天然低——这就是 referee 存在的证明）。
**提交**：`test(eav): add cross-domain attribution golden set`（构建器另卡 `chore(scripts): build eav corpus fixtures`）

### 任务卡 E-6 · 接线：/extract → /answer 消费 ⬜

**交付什么**（三个单关注提交，全部 additive）：
1. `feat(models): carry expected subject and entity attribution` —— `ExtractRequest.ExpectedSubject *SubjectSpec{Name,Hint}`（`json:"expected_subject,omitempty"`，Name 必填 ≤ MaxAnswerSubjectBytes）；`MultiExtractSource.Entity *EntityAttribution{Verdict,Name,Kind,Quote,Tier}`（omitempty）；`AnswerEvidence.EntityVerdict string`（omitempty）；新 unknown reason `AnswerUnknownEntityMismatch = "entity_mismatch"`。
2. `feat(extract): judge per-source entity attribution` —— 挂点 `extract/multi.go` `extractMultiSource`（per-source 成功抽取 + evidence 校验之后）：`ExpectedSubject` 非空且 EAV 启用时，用 Artifact 的 cleaned/raw/title 组 `eav.Document` 判一次，verdict 落 `MultiExtractSource.Entity`。生产 adapter 在此层落地：`llm.Client` + E-3 提示词常量 + managed key 注入（模式照 P2-4 `88c3471`）；**盲抽取结果按 SnapshotID 进程内 LRU 缓存**（内容寻址 ⇒ 键完美；bound 照 `llm.schemaCache` 双上限模式）；referee 依赖 subject 不缓存。config 新键照 §2.5 `envOr`：`PURIFY_EAV_ENABLED`（默认 false）、`PURIFY_EAV_REFEREE_ENABLED`（默认 true）、`PURIFY_EAV_CACHE_ENTRIES`（默认 128）。判定失败/超时 ⇒ 该源 Entity=uncertain，**绝不 fail 整个 extract**。
3. `feat(answer): withhold beliefs on entity mismatch` —— `answer/service.go`：`extractRequest` 带上 `ExpectedSubject{Name: spec.Subject, Hint: spec.Predicate}`；在 `allowedConsensusSupports`（service.go:159）之后、`decide`（:175）之前，**从 allowed 集剔除 verdict==mismatch 的源**（错实体的支持不是支持）；uncertain 保留计数但在 `AnswerEvidence.EntityVerdict` 标注。剔除数 >0 且 decide 因此落 unknown/insufficient 时，reason 覆写为 `entity_mismatch`，closest.note 说明被剔除的实体名。
**成本口径**：+1 次小 LLM 调用/源（头窗 ≤8KB ≈ ~2k tokens）+ 灰区才有的 referee 调用；8 源上限即最多 16 次；snapshot LRU 使 /watch 复访与重复源趋零成本。
**测试**：extract 层 fake judge 注入验证 per-source verdict 落位与失败降级；answer 层表测试覆盖「剔除后仍 known / 剔除致 unknown(entity_mismatch) / 全 uncertain 不剔除」三形态。

### 15.4 提交序列与依赖

```text
E-1  feat(eav): normalize and match entity surface forms      # 纯函数内核
E-2  feat(eav): harvest deterministic entity candidates       # 依赖 E-1（Normalize 去重）
E-3  feat(eav): extract the primary document entity blind     # 依赖 E-1/E-2 类型
E-4  feat(eav): judge subject attribution three ways          # 依赖 E-1..E-3
E-5a chore(scripts): build eav corpus fixtures                # 依赖 E-2（收割快照格式）
E-5b test(eav): add cross-domain attribution golden set       # 依赖 E-4 + E-5a；P/R/FP 门从此生效
E-6a feat(models): carry expected subject and entity attribution
E-6b feat(extract): judge per-source entity attribution       # 依赖 E-4 + E-6a
E-6c feat(answer): withhold beliefs on entity mismatch        # 依赖 E-6b
```

每张卡照 §13 纪律：测试先行、单关注提交、`go test -race ./...` + vet + build 全绿、公开契约 additive-only。E-5b 的 golden 门并入 §10 准确率 CI（那张待办卡落地时直接引用本门）。

### 15.5 本相位非目标（防漂移）

- ❌ claim 级逐 anchor 归因（v1 文档级）
- ❌ 同形异指判别（记 backlog：FactSpec 类型化实体）
- ❌ 裁决落 ledger 持久化 / 喂 L2 重排——那是 PLAN.md §6 步 4 的焊接工作，接口留好即可
- ❌ /verify 端点带 expected_subject 复验（等步 4 一并考虑，避免 verify 包现在就依赖 eav）
- ❌ 共指消解、跨文档实体图、知识库对齐（Wikidata linking）——全部 YAGNI，标注集证明需要再说

> **EN —** Phase 8 lands EAV as `verify/eav/`: a pure normalize+match ladder (E-1), deterministic candidate harvesting (E-2), a blind anchored LLM extractor boundary (E-3), a three-verdict judge with a gray-zone referee (E-4), a 300+-row six-domain golden set built by sibling-category perturbation with recorded-LLM replay (E-5), and additive wiring through /extract into /answer where mismatched sources stop counting as support (E-6). Alarms only ever come from the similarity floor or the referee; everything unprovable stays uncertain.
