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
| `PURIFY_EAV_ENABLED` | `false` | 开启 process-owned 逐源实体归因 | E-6 |
| `PURIFY_EAV_REFEREE_ENABLED` | `true` | EAV 开启时启用灰区 referee | E-6 |
| `PURIFY_EAV_CACHE_ENTRIES` | `128` | 成功盲抽取的进程内 LRU 条目上限 | E-6 |
| `PURIFY_EAV_LLM_API_KEY` | 空 | managed EAV 专用 credential，不作为请求 BYOK fallback | E-6 |
| `PURIFY_EAV_LLM_MODEL` | `gpt-4o-mini` | managed 实体归因模型 | E-6 |
| `PURIFY_EAV_LLM_BASE_URL` | `https://api.openai.com/v1` | managed EAV OpenAI-compatible base URL | E-6 |
| `PURIFY_EAV_LLM_ALLOW_PRIVATE` | `false` | 仅允许运营者配置的 managed EAV 端点访问私网；请求 BYOK 永远公网 only | E-8 |
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

> **状态**：**Phase 8 + EAV 校准代码收口（2026-08-11）**。进度：E-1 ✅（`612fd50`）　E-2 ✅（`b5be194`）　E-3 ✅（`f2c6329`）　E-4 ✅（`e233bf6`）　E-5 ✅（`737a008`+`3eefe31`，种子集 48 行）　E-6 ✅（`060ff7e`+`1384592`+`a979cb7`）　E-7 ✅（`18411d7`）　E-8 ✅（`c60ec5f`，整块审查补强 `22c379e`，Compose 接线 `1a2c601`）。图例沿用：✅ 已提交 · 🚧 进行中 · ⬜ 未开始 · ⟳ 与规划不同（以实码为准）。
>
> **真实语料基线（`3a19cfe`）**：78 文档（en/zh Wikipedia 真实抓取）× 345 行审计标签，以 Mac Ollama 代理的 `gpt-oss:120b-cloud` 录制并晋级 golden。首份 full 成绩为 P=1.000 / R=0.935 / FP=0 / U=0.062 / silent=0，hard R=0.975；det-only P=1.000 / FP=0 / R=0.687。原 21 条 uncertain 中，18 条来自 4 个 extraction 文档因 quote 洗掉脚注/注音而被锚定闸整体丢弃，另 3 条来自 referee unsure；旧文案“4 文档涉 21 行”已按实况纠正。
>
> **E-7 校准**：`ExtractionSystemPrompt` 现同时要求 shortest/proves、连续子串、逐字符复制、回传前自检，并逐字保留 “including footnote markers, pronunciation guides, and unusual spacing; never clean it up”。首次 frozen-prompt 全量运行的诚实成绩是 P=1.000 / R=0.987 / FP=0 / U=0.009 / silent=0、hard R=0.950、extraction raw-exact 76/78；随后对**预先声明的 5 文档 / 22 标签**做一次文档级成对 repair（每个文档整行 extraction + 该文档全部 referee 一起替换，其他 73/57 行保持首次全量原样），晋级为 curated regression replay：P=1.000 / R=0.996 / FP=0 / U=0.003 / silent=0、hard R=0.975、det-only R=0.730，78/78 **extraction** quote 均为原文 exact substring 并通过 production anchoring。这不是第二份无偏全量成绩；hard 的单条残余也发生了身份交换：Michael B. Jordan 漏报已修，但 `iPhone 16 × products/iphone-15` 因 referee quote 自行加空格而变 uncertain，故不得表述为 hard 逐行无回归或所有 referee quote 都 exact。
>
> **E-8 网络边界**：`PURIFY_EAV_LLM_ALLOW_PRIVATE` 默认 false，只控制运营者固定配置的 managed EAV 独立 policy/client；request BYOK、Search、relay、compiler、webhook 等共享 outbound policy 永远公网 only。loopback managed 成功 + 同进程 BYOK 拨号前拒绝/零凭据泄漏已有测试。真机冒烟此前已验配置/启动、单源拒参、无 Search key 的干净降级和死 provider 超时；剩余 `/answer` happy path 仍需运行实例 + Search provider key，未 push、未 deploy。
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
    recordings/*.jsonl       # LLM 录制回放（离线评测全链路）
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

### 任务卡 E-3 · 盲抽取边界 + 锚定校验 ✅

**状态**：✅ 已提交 `f2c6329 feat(eav): extract the primary document entity blind`。⟳ 与规划的差异：(a) 锚定闸比卡面更严——**Name 与每个 Alias 也必须落位**（Title∪Cleaned，ASCII 大小写不敏感、采纳文档原始大小写），不只 Quote：别名等值在 E-1 是 match 级信号，幻觉别名会把 mismatch 洗成 match，必须闸掉；(b) Quote 缺失时回退为已落位的 Name（Name 已锚定，回退不减弱保证），Quote 超限仍整体丢弃；锚定成功后采纳 `evidence` 返回的文档原文窗口（fuzzy 窗口超配额时保留原 Quote）；(c) 错误策略三分而非二分：provider/数据错误 → 降级空结果，`ErrNotConfigured`（含 typed-nil extractor）与 ctx 取消 → 上抛；(d) 短路不花钱：空候选板/空或超限 Cleaned 直接返回空结果、不调用 extractor；盲输入裁剪在边界强制执行（extractor 只见头窗、永不见 RawHTML、slate 截到上限），闸门则跑在**全量** Cleaned 上（测试用头窗外的 quote 锁死此语义）；(e) schema 里 quote 为必填字段（卡面草图漏写），`eav` 测试锁 schema maxItems==MaxAliases、enum==Kind 字面量；(f) 附带 `DecodeExtractionReply`（16KB 上限、null 宽容、尾垃圾拒收）与 `BuildExtractionInput`（确定性、URL/标题截断、结构性盲——没有 subject 参数可泄露），E-6 的 adapter 因此薄到只剩 llm 调用。
**怎么做**：抽取输入只有 `Document`（头窗裁剪到 `MaxHeadWindowBytes`）+ 候选板。LLM 提示词模板与 strict JSON schema（`{"primary":{"name","kind","aliases"},"reason_if_none"}`，name 必须从候选板 Quote 中选）以常量形式放 `extract.go`，供接线层 adapter 复用——**adapter 本体（`llm.Client` 接线、BYOK/managed key、修复重试）在 E-6，不进本包**。
**测试**：fake extractor 注入：合法输出通过；幻觉名（不在文档中）被锚定闸拦下；超限被拒；LLM error → Primary=nil 而非 error 上抛（error 只在 ctx 取消时上抛）。
**提交**：`feat(eav): extract the primary document entity blind`

### 任务卡 E-4 · Judge 编排 + referee 灰区裁决 ✅

**状态**：✅ 已提交 `e233bf6 feat(eav): judge subject attribution three ways`。误报纪律已断言写死（mismatch 仅出自 floor|referee；decided 裁决必带 Evidence）。⟳ 与规划的差异：(a) **decided 裁决的证据锚定失败 ⇒ 整体降级 uncertain**——不只 referee 的区分性 quote，ladder 命中的 match/mismatch 也一样（primary quote 理论上已过闸必锚上，此为防御性收口：无证据的裁决不出门）；(b) referee 只见头窗 + 已过闸实体（永不见 RawHTML），Hint 截断到 MaxHintBytes 后传入；错误三分与 extractor 相同（provider 错 → uncertain，ErrNotConfigured/ctx → 上抛）；(c) typed-nil referee 视为未配置而非信任（NewJudge 收编为 nil）；(d) 不可用输入（空/超限 subject、空/超限 Cleaned）在触达任何依赖**之前**判 uncertain，extractor 零调用（测试计数器作证）；(e) `DecodeRefereeReply` 未知 answer 值收敛为 unsure（只能朝不报警方向坍缩），大小写宽容；`BuildRefereeInput` 空段省略、确定性、全字段截断。Judgment 的 Anchor 不带 SnapshotID/FetchedAt——由接线层（E-6）用自己的观测盖章，已写进类型注释。
**怎么做**：裁决优先级固定：盲抽取 Primary=nil ⇒ uncertain 直接返回；阶梯 1–4 命中即返回；灰区才碰 referee；referee nil/error/unsure ⇒ uncertain。`Judgment.Evidence` 在 match/mismatch 时必须存在（来自 Primary.Quote 或 referee 区分性 Quote 的锚点）。**误报纪律以断言写死在测试里：mismatch 只可能出自 Tier ∈ {floor, referee}。**
**测试**：全路径表测试（fake extractor + fake referee 的笛卡尔组合）；ctx 取消在每个边界立即返回。
**提交**：`feat(eav): judge subject attribution three ways`

### 任务卡 E-5 · 跨域标注集 + 评测门 ✅（种子集）

**状态**：✅ 已提交 `737a008 chore(scripts): build eav corpus fixtures` + `3eefe31 test(eav): add cross-domain attribution golden set`。⟳ 与规划的差异与诚实边界：(a) **本次落地为 48 行手工种子集**（6 域 × 12 文档，同类扰动法配对：26 clean / 21 mismatch / 1 waived），≥300 行扩容用 `scripts/eavcorpus` 三模式（fetch 真实抓取+清洗+收割快照、pair 兄弟配对出草稿标签、record 真实 LLM 跑全链路并录制）在联网环境执行；(b) **种子录制是手工合成的「合格抽取器」回复**——它测的是给定合格抽取下确定性机器（阶梯+守卫+锚定闸+referee 协议）的 P/R/FP，真实 LLM 数字必须 `-mode record` 重录后再看；(c) 回放按 doc URL/主体键检索、不依赖 slate 哈希，离线跑的是**真正的 `JudgeDocument` 全链路**（收割在测试时对无 RawHTML 的快照重跑，抽取回放对键命中，缺录制 = 语料洞 = 测试失败）；(d) 新增 `waived` 行机制：同形异指已知限制（Georgia 州 vs 国）以 label=mismatch + waived=true 入库，**排除在门外但每次打印**——限制被执行地记录，而不是被撒谎地绕过；(e) referee 录制的 subject 键强制存 Normalize 形，loader 校验。**种子集实测：full 模式 P=1.000 / R=1.000 / FP=0 / U=0；确定性模式 P=1.000 / FP=0 / R=0.524（easy 切片 R=1.0，hard 切片 R=0、全 uncertain）——precision 由确定性内核扛、recall 由 referee 挣，正是设计形状。**
**标注集怎么起（锁定方法：真实文档 × 同类扰动配对，不手写文档）**：
1. **同类清单挖兄弟**：6 个域各取一份公开类目清单——公司（US+CN 上市公司名录）、药品（FDA/NMPA 常用药，对齐论文原始域）、产品（手机/相机型号）、人物（同名近名公众人物）、地名（Georgia/Jordan/湖南-湖北类）、事件（年度峰会/条约版本）。
2. **真实抓取**：构建器用现有 scrape 栈每域抓 ~10–15 个真实页面，**fixture 存收割后快照**（`{url,title,cleaned≤8KB,slate}` JSONL），不存全 HTML——收割本身由 E-2 的 HTML 样张单测覆盖。
3. **程序化配对**：正例 =（A 的页, subject=A）与（A 的页, subject=A 的别名/ticker/简称）；负例 =（B 的页, subject=A），其中 B 为 A 的同类兄弟；**hard 负例** = 兄弟中 `Normalize` 后编辑距离 ≤0.35 或 token 重叠 ≥0.5 的词形混淆对（AMD/ARM 类），打 `hard:true`。
4. **人工审计**：随机 10% + 全部 hard 对逐行过目改标；行 schema `{subject,hint?,doc_ref,label:"match|mismatch",hard,domain,note}`。
5. **LLM 录制回放**：实码按文档 ID（由 URL 映射）索引 extraction reply，referee 再按 `doc ID + Normalize(subject)` 索引；recording schema **不存 prompt/model/run digest**。因此任何 prompt 变化都必须在空目录主动重录，并把 `extract.jsonl` + `referee.jsonl` 成对晋级，不能把旧录制的绿灯冒充新 prompt 成绩。golden 测试用回放 fake 跑**全链路**（收割→盲抽取→阶梯→referee），离线、确定性、免 key；真实重录使用 `EAVCORPUS_API_KEY/MODEL/BASE_URL` + `go run ./scripts/eavcorpus -mode record -docs ... -labels ... -out ...`。
**指标定义（写进 `golden_test.go`，即 PLAN.md §5.3 验收的可执行形式）**：
- 报警精度 P = 判 mismatch 且标 mismatch / 判 mismatch，**门 > 0.90**
- 报警召回 R = 判 mismatch 且标 mismatch / 标 mismatch（uncertain 计入漏报，从严），**门 > 0.90**（全链路回放模式）
- 干净误报率 = 标 match 判 mismatch / 标 match，**门 < 0.02**
- uncertain 率无门但必须打印（诚实成本可见）；另按 domain × hard 分层打印。
- 纯确定性模式（referee 关）单独跑：只门干净误报 < 0.02 与 P > 0.90，不门召回（灰区全 uncertain，召回天然低——这就是 referee 存在的证明）。
**提交**：`test(eav): add cross-domain attribution golden set`（构建器另卡 `chore(scripts): build eav corpus fixtures`）

### 任务卡 E-6 · 接线：/extract → /answer 消费 ✅

**状态**：✅ 三提交收口：`060ff7e feat(models)` + `1384592 feat(extract)` + `a979cb7 feat(answer)`。⟳ 与规划的关键差异（有意为之）：(a) **剔除点从 answer 的 allowedSupports 前移到 extract 的共识入口**——answer 的支持校验把「未知支持」视为硬错误，事后剔除会变成错误路径；在 `extractMultiSource` 成功收尾处判定、mismatch 源以新状态 `entity_mismatch`（+新错误码 `ENTITY_MISMATCH`）排除出 `valid` 集，共识从一开始就没见过错实体源，/extract 直接用户同样受益；判定失败/超时 → Entity=uncertain 且绝不 fail 源（20s 子超时）。(b) 缓存按**内容哈希**（URL+标题+头窗 sha256）而非 SnapshotID 落键——内容寻址语义相同、不污染 eav API；缓存在 cmd adapter（成功才缓存、取出深拷贝、LRU 双界）。(c) 配置为 `PURIFY_EAV_ENABLED/REFEREE_ENABLED/CACHE_ENTRIES` + `PURIFY_EAV_LLM_*` 独立三键，provider 校验与 compiler 共享抽出的助手，main 启动即验。(d) answer 消费三形态全测：剔除致短缺 → reason 覆写 `entity_mismatch`（closest 定长备注，实体名在逐源 Entity 字段里）；全军覆没 → NoValidSource 路径覆写；胜出证据逐条标注 `entity_verdict`（结构上只可能 match/uncertain——mismatch 进不了共识）。单源 /extract 显式拒收 expected_subject。投影校验器同步收编新 reason 与新字段。

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

---

## 16. 交接 · 2026-08-11 起 Codex 接续开发

> **给 Codex 的单页入口。** 读完本节 + `AGENTS.md` 即可开工；战略问题回 `PLAN.md`（LOCKED，三层结构与非目标不许推翻）。
> **状态快照**：原交接 waypoint `c7189c2`，当前开发 waypoint `dae246d`（分支 `codex/search-api-v1`）；P1 N_eff v1 五卡已于 2026-08-11 完成至 `94f9f88`，整块审查补强至 `d213376`；P2 EAV 两张校准卡已完成至 `c60ec5f`，整块审查补强至 `22c379e`，Compose 配置闭合至 `1a2c601`；P3 已完成 R-1 设计定案至 `8c83af3`、R-2 pure rerank core `c8e72f8`、R-3 strict runtime/config `9f837ee` + `c385830`（admission 边界澄清 `9697a3f`、网络审查补强 `e649afc`）、R-4 内部 relevance orchestration `dfc89c7`，以及 R-5 relevance 公开契约与显式 service switch `ea7a87e` + `9e83c40` + `dae246d`。Phase 0–8 与 P1/P2 代码均收口；当前晋级的 curated regression replay 成绩为 P=1.000 / R=0.996 / FP=0 / U=0.003 / silent=0、hard R=0.975（完整录制口径与残余见 §15）。R-5 收口时全仓 `go test ./...`、`go test -race ./...`、`go vet ./...`、`go build ./...` 与 `git diff --check` 全绿。

### 16.1 优先队列

**P1 · N_eff v1（PLAN.md §6 步 3，落 `consensus/`）** —— 出处血缘独立。**实况先于计划**：在原交接 waypoint `c7189c2`，v0 的 `independentComponents` 已比 PLAN.md §5.4 描述的强——用 union-find 按「同 eTLD+1 根域 ∪ 全页 SimText simhash 距离 ≤ `independenceDistance`(3)」折叠，跨域**完全**镜像已能折。当前 v1 的入口已升级为 `buildIndependencePlan` / `sourcePairFoldReason`，`independentComponents` 只保留为数值兼容包装。v1 的真实增量按卡执行：

- **N-1 · 实况盘点 + 设计定案** ✅（`97490db`）：已通读 `consensus/merge.go`/`materialize.go`、两组测试、`simhash/` 全家及生产接线；v0 边界与 N-2…N-5 实施细则见 §16.2–§16.3。⟳ 关键实况：`Anchor.Quote` 通常只是抽取标量，不是支撑句；内容核/措辞血缘必须从 cleaned content + 已验证 `TextRange` 重建有界邻域。
- **N-2 · 内容核指纹** ✅（`9a3455e`）：在 v0 边的并集上新增「支撑字段锚点邻域」的规范化 shingle 指纹；不替换同根域/legacy 全页 SimText。en/zh 同稿异站真实经过 production fingerprint 折到 1，独立 hard negatives 不误折。
- **N-3 · 措辞血缘** ✅（`c78010b`）：相同 path/value 的锚点句段存在足够长逐字包含 ⇒ 视为转载衍生，入 union；95/96/97 rune、CJK、传递链、超长 fragment 与 source-global materialization 均已锁门。
- **N-4 · 折叠原因导出（additive）** ✅（`e17086c`，整块审查补强 `d213376`）：`Support`/`Agreement` 增加 omitempty 字段暴露折叠证据（`fold_reason: same_root | near_duplicate | quote_lineage`）；deterministic forest、mixed witness、REST/MCP/Answer 投影与 32 MiB 预算均已锁门，MCP multi decoder 同时拒绝普通/Unicode-escaped 重复 JSON key，公开契约只加不改。
- **N-5 · 构造集评测门** ✅（`94f9f88`）：`consensus/testdata/neff/` 已有 33 个 source / 12 个 case，覆盖六类、en/zh、1+7 镜像、8 独立源、95-rune、lineage、mixed、compat 与 bridge；严格 loader、v0/v1 有界排列和 exact reason 门已并入普通 `go test`。

**P2 · EAV 校准迭代（小卡）** ✅：
- **E-7 · 提示词逐字强化 + 重录对比** ✅（`18411d7`）：保留 shortest/proves 语义并强化逐字复制；golden 现逐一锁住 78/78 extraction quote 为 cleaned 原文子串且通过 production anchoring。首次 frozen-prompt 全量与随后预声明 5 文档 / 22 标签的 paired repair 分开记账；晋级 replay 为 P=1.000 / R=0.996 / FP=0 / U=0.003 / silent=0、hard R=0.975，详见 §15。改 prompt/`match.go` 阈值仍**必须过 golden 门**。
- **E-8 · managed LLM 私网豁免配置** ✅（`c60ec5f`，整块审查补强 `22c379e`，Compose 接线 `1a2c601`）：新增 `PURIFY_EAV_LLM_ALLOW_PRIVATE`（默认 false），只给**运营者固定配置的** managed EAV 独立 policy/client 开私网；request BYOK 与 Search/relay/compiler/webhook 等共享 egress 仍是公网 only。loopback、默认拒绝、同进程隔离与零凭据泄漏均有回归门；LLM response-format capability cache 也已改为 per-client、per-model 且 128 项有界，request/managed 不再共享降级状态；主 Compose 路径显式转发全部 7 个 EAV 键。

**P3 · 步 4 重排器 + 信任排序** 🚧：PLAN.md §4.3。R-1 实况盘点与设计定案已写入 §17；R-2 pure rerank core `c8e72f8`、R-3 strict vLLM runtime/config `9f837ee` + `c385830`、R-4 internal relevance orchestration `dfc89c7` 与 R-5 relevance 公开契约 `ea7a87e` + `9e83c40` + `dae246d` 均已落地，R-3 卡后网络审查补强为 `e649afc`。省略/显式 `provider` 永久保持旧排序与 wire；显式 `relevance` 已具 20-candidate fan-out、结果级 score、degraded fallback 及 HTTP/MCP strict contract，但生产 scorer factory/registry 仍故意不存在，enabled 配置在解析 endpoint 或网络动作前固定 fail closed，须等 R-6a authenticated deployment handoff 与 R-6 manifest 同过。`trust` 请求形状、默认值和最坏计费已锁，但任何成功响应在 R-8 前 fail closed；`Entity`/`fold_reason` 仍只表示 transport 字段存在，**不等于 Search 已有逐结果信任分**。下一步是 R-6 pinned Qwen recordings / relevance golden / scorecard；其后 R-6a 与 R-7/R-8 分别闭合部署认证和信任排序。

**P4 · 旧账（№ 不阻塞上面）**：§10 准确率 CI（golden 门已现成，接 CI 即可）；commons 冷启动 ≥30 垂类（要运行实例 + key）；P7 自适应租约。

### 16.2 N_eff v1 设计定案（N-1，先读防漂移）

#### 16.2.1 v0 的真实边界

1. **输入与去重**：`Merge` 接受 1–8 个 source；先 canonicalize final URL，同 URL 仅在规范化 JSON、`Basis`、`Receipts`、`SimText` 全等时去重，否则 `ErrDuplicateSourceConflict`。Root 不信 caller，域名取小写 eTLD+1，literal IP 取 `Unmap` 后地址。
2. **独立 component 是全局一次算完**：source 按 canonical URL 排序后，`independentComponents` 对任意 pair 在「同 Root」或「两者 `SimText != 0` 且 Hamming distance ≤3」时 union，取传递闭包。component 不随 path/value 改变，同一份结果同时供字段共识与 materialization 的 node kind、key presence、array length、scalar vote 使用。
3. **N_eff 的字段名有历史债**：`Agreement.Pages` 是该 canonical value 的唯一页面数；`IndependentRoots` 实际是这些页面覆盖的 component 数，并非简单根域数。winner/conflict 严格按 `(IndependentRoots DESC, Pages DESC)` 排序；同分显式 ambiguous。
4. **生产 `SimText` 是 cleaned 全页词袋**：`extract/multi.go` 对 `artifact.Public.Content` 调 `simhash.Fingerprint`。该函数只是 `strings.Fields` token 的 FNV-64a 多数投票：不 lowercase、不做 Unicode/标点规范化、不保留词序、没有 shingles；`0` 是不可用哨兵。cleaner 常能留下正文，但 readability/pruning 失败回退时仍可能混入导航、页脚和站点模板，导致「同一通稿 + 不同 chrome」距离 >3 而漏折。
5. **现有测试没有覆盖真实缺口**：镜像用例注入手写 uint64，只证明 DSU/排序，不经过生产 `cleaned → Fingerprint → Merge`。已有传递链测试还锁定了 single-linkage：A≈B、B≈C 即使 A 与 C 不近也会同 component。
6. **现有 evidence 不能直接当内容核**：`evidence.AlignAll` 的 exact/normalized `Anchor.Quote` 通常等于标量值，fuzzy 也只在值附近约 ±2 token。直接 fingerprint/contains 会把不同报道共同出现的价格、日期、姓名甚至 `true` 误当转载，也抓不到 B 包含 A 长段落的场景。

#### 16.2.2 七项锁定决策

1. **v1 仍是 source-global component**。任一字段给出足够强的衍生证据，就保守地把整页视为非独立；字段共识与 materialization 永远消费同一份 component plan。⟳ 不做一半 per-field、一半 global 的混合语义；claim/path 级独立性留 v1.5。代价是某字段转载可能让该页其他原创字段也减权，但这是低估独立性而非制造虚假高置信度。
2. **新信号只加不替换 v0**：

   ```text
   same_root
   ∪ legacy whole-page SimText distance ≤ 3
   ∪ content-core near-duplicate
   ∪ quote-lineage
   → one source component
   ```

   N-2/N-3 阶段，未提供新输入的 caller 必须与 `c7189c2` 行为和 JSON bytes 一致；legacy 与 content-core 两种相似边在公开契约均映射为 `near_duplicate`。N-4 落地后旧字段的数值/排序/语义不变，但已有 v0 折叠的响应会 additive 出 reason；只有未发生折叠的响应仍 byte-identical。
3. **新增的内部输入是 cleaned text，不是短 quote**：给 `consensus.SourceResult` additive 增加非 wire 的可选 `CleanedText string`，空串明确定义为“旧 caller 未提供”，由 `extract/multi.go` 注入现成 `artifact.Public.Content`。非空时 `len(CleanedText)` 每源 ≤4 MiB、必须合法 UTF-8；结合 `MaxSources=8`，调用内总引用量自然 ≤32 MiB，不另设不可独立触发的 aggregate text cap。只有 `Quote!=""`、`end>start` 且 Method∈`{exact,normalized,fuzzy,compiled}` 的 Basis 才是 eligible anchor；其他 Basis 保留原共识证据但跳过增强，绝不能用 `[0:0]` 从页首造 core。eligible anchor 的 range 越界或 `CleanedText[start:end] != Quote` 返回 `ErrInvalidInput`，单源文本越界返回 `ErrResourceLimit`。字符串只在 `Merge` 调用期只读借用、不 clone、不进入结果；`sourcesEqual` 比较其值。缺失才兼容回 v0；派生复杂度不新增更窄的输入错误，而由下一条的确定性采样收界。
4. **N-2 使用固定锚点窗 + 确定采样 shingle set**：eligible anchors 先按 `(sha256(path), path, TextRange.start)` 排序，最多选 256 个；同 schema 的跨站 source 因而优先选择同一批字段。每个入选 anchor 取最多 1 KiB 的 byte window：令 `center = start + (end-start)/2`，先取 `[center-512, center+512)`，越文档边缘时向另一侧平移以尽量保持 `min(1024,len(CleanedText))` bytes，最后把起点前移、终点后退到最近 UTF-8 rune boundary；quote 自身 >1 KiB 时同样只取其中点窗。窗口保持 per-path 独立，shingle 绝不跨窗；不做拼接后跨界特征，窗口总输入天然 ≤256 KiB。

   tokenizer 逐 rune 做 evidence 同向的全角 ASCII/空格折叠与 Unicode lowercase；任何非 `Letter|Number` rune 都是 separator，绝不把 `foo-bar` 拼成 `foobar`。每窗同时生成连续 3-token shingles，以及每个单独 alnum token 内的 5-rune shingles；混合文字直接取两类并集，不猜语言。编码先写不相交 type tag（word/rune），word family 再对三个 token 做 length-prefix，杜绝连接与跨族碰撞。shingle 以 `(sha256(encoded), encoded)` 为稳定 key，用 bounded max-heap + retained-key map 流式维护最小的 4,096 个 unique key；阈值只会下降，被淘汰项不会重新进入，因而无需持有无界全量 set。新 `FingerprintShingles` helper 直接把每个 retained shingle 当一个原子做同向 FNV-64a 位投票，不再经 `strings.Fields` 拆散，并返回 `{fingerprint, retained_shingles, normalized_alnum_runes, valid}`；`normalized_alnum_runes` 是全部入选窗口规范化后 `Letter|Number` rune 的未饱和总数（窗口输入有 256 KiB 硬界），`valid = retained_shingles >= 24`，合法 fingerprint 即使为 0 也不能当缺失。0 eligible anchor 或不足 24 个 retained shingles 只是“无 core 信号”，不报错且仍可走 same-root/legacy 边；现有 `Fingerprint` 与 legacy `SimText==0` 哨兵语义不改。
5. **N-2 起始门槛独立命名、独立校准**：两边内容核均 `valid`，且用 uint64 交叉乘锁定 `min(normalized_alnum_runes)*100 >= max(normalized_alnum_runes)*70`、core Hamming distance ≤3 时才建 `near_duplicate` 边。长度保护绝不能用会在 4,096 饱和的 retained count。不得借机放宽 legacy `independenceDistance=3`；任何阈值/采样变化必须用 N-5 构造集同时证明镜像召回与独立源零误折。
6. **N-3 使用可变句段 fragment 做同 claim 逐字包含**：只对 N-2 确定性选中的最多 256 个 eligible anchors，各保留三种“完整包含该 `[start,end)`”的原文候选：所在单行（`\n` 边界）、所在句子（`.?!。！？` 边界并包含终止符）、所在段落（空行边界，空行只允许 space/tab/CR）。若 anchor 跨越某类边界则不生成该类；候选以 byte range 只读引用 `CleanedText`，比较时统一 CRLF→LF、trim 外围 Unicode whitespace，内部字节不改，exact 去重。单 fragment 必须 ≤1 KiB 且含至少 96 个 Unicode `Letter|Number` runes；数量天然 ≤768（3×256），不另造比现有 10,000-leaf 契约更窄的 hard cap。

   只在两 source 的「相同 evidence path + 相同 canonical scalar value」索引桶内比较，每个 anchor 最多 3×3 个候选；存在 A fragment 是 B fragment 的逐字子串（相等也算）或反向包含，就建 `quote_lineage` 边。边一旦成立仍是 source-global。v1 不 lowercase、不去标点、不找模糊最长公共子串，也不判谁先发布。共同导航/免责声明若不包含该字段 anchor 天然不参与；anchor 内完全相同且达门槛的长 boilerplate 按定义会折，是 v1 的已知限制，N-5 只把“相似但非逐字包含”设为必须不折的 hard negative。
7. **资源与确定性是验收项**：8 source 最多 28 个 pair；单次 aggregate `Merge` 内 core/fragment/token/fingerprint 每源只预计算一次，pair loop 禁止每 anchor 重扫 4 MiB 文本。production 现有 singleton `Merge` 必须执行全部 hard admission（size、UTF-8、eligible range/quote）使坏 source 在 aggregate 前被逐源淘汰；因派生复杂度只采样、不报错，singleton 可跳过仅供 pair comparison 的 core/fragment 构造。若后续缓存 prepared signal，只能复用等价的已验证输入，不能跳过 admission。≤5 source 穷举全排列；6–8 source 只跑 canonical/reverse/rotations + 固定 seed 的 32 个样本。benchmark 分开记录「预分配 cleaned 输入上的 core/fragment 派生」与「预计算信号上的 28-pair plan」，首个 N-2 实现据实建立回归基线；墙钟/alloc 不先写不可验证的 CI 数字，结构上限与构造集必须进 `go test`。

#### 16.2.3 fold reason 语义（N-4 前置定案）

- `independentComponents` 升级为内部 `independencePlan{components, forest}`：建边时保留原因；DSU 的 rank parent 只是实现细节，严禁拿它冒充可审计血缘。
- 每个 source pair 若同时命中多信号，先按 `same_root > quote_lineage > near_duplicate` 只保留最高优先级原因；再按 reason 优先级与两端 canonical URL 排序跑稳定 Kruskal，得到无向 forest。每棵树以 canonical URL 字典序最小页为代表，从代表按邻接 URL 字典序做 BFS 定向；每个非代表页有且只有一条 parent edge。
- 公开枚举严格三项：`same_root | near_duplicate | quote_lineage`。`Support.fold_reason,omitempty` 是 source-global 属性，记录该页 parent edge 的原因；代表页省略。某 value group 即使 `Pages==IndependentRoots`，其中 Support 仍可能因组外页面而带全局 reason，消费者不得据此反推该候选发生折叠。
- `Agreement.fold_reason,omitempty` 才描述该 value group 的实际折叠：对每个与该组相交且交集至少 2 页的全局 component，取 forest 中连接这些组内页的最小子树，组外中间节点/边也必须计入 witness；仅当 `Pages>IndependentRoots` 且全部 witness edge 只有同一种原因时输出该原因。`Pages==IndependentRoots` + 省略 = 未折叠；`Pages>IndependentRoots` + 有值 = 单一原因折叠；`Pages>IndependentRoots` + 省略 = mixed reasons。`Pages`、Supports 数量与证据不因折叠而减少，只改变 `IndependentRoots`。
- `Materialization` 只继续消费 `components`，不把 fold metadata 塞进 caller 的 typed `data`。N-4 只导出信号，不在同卡修改 winner、answer confidence 或重排策略。
- `models.MultiExtract*` 维持独立 transport mirror；multi REST 投影/编码预算、MCP strict decoder/枚举校验与 consensus 手算 `MaxOutputBytes` 预检必须同步。Extract HTTP handler 是受信 service 响应的编码路径，不为 N-4 新造 decoder。Agreement 被 `AnswerBelief`/`AnswerCandidate` 复用，故同时更新 `answer/service.go` 枚举校验、`api/handler/answer.go` 响应校验，以及 MCP Answer 的 strict decoder/agreement allowlist。Support reason 在 N-4 不映射到 `AnswerEvidence`，Search/MCP-search 也不变；未来 confidence 卡再消费。旧 fail-closed MCP 不接受新字段，因此受控部署必须锁步升级；wire 对普通 JSON client 仍是 additive。

### 16.3 N_eff v1 实施卡（N-2…N-5）

#### 任务卡 N-2 · 支撑内容核指纹 ✅（`9a3455e`）

**交付什么**：`SourceResult.CleanedText` 的有界 admission/只读借用/equality；固定锚点窗构造器；`simhash/` 新增将 shingle 作为原子的、返回显式 valid/count 的规范化指纹 pure helper；全局 component builder 在 v0 边并集上加入 core near-duplicate（N-4 才把其返回值扩成带 forest 的 `independencePlan`）。

**测试先行**：真实调用生产 core builder 与 fingerprint，覆盖 en/zh「同正文 + 不同 nav/footer」使 legacy distance >3、core 折到 1；相同 scalar/短 quote 但独立正文保持 N_eff=N；空 `CleanedText` 精确走 v0、0 eligible anchor/<24 retained shingles 保留 v0 边；非法 UTF-8、单源文本越硬上限、eligible quote/range 不一致分别报确定错误，空 quote/零 range/unlocated anchor 则稳定跳过；>256 anchors 与 >4,096 unique shingles 断 deterministic selection、bounded state 和排列稳定。固定窗覆盖文档边缘、超长 quote、UTF-8 边界；mixed-script tokenizer、separator、不跨窗、type tag、length-prefix/set 去重。`FingerprintShingles` unit test 直接断 valid 只由 retained count 决定，比较器再注入 synthetic `{fingerprint:0, valid:true}` 证明合法零 hash 可参与；distance 3/4 同样用 synthetic descriptor 精确锁，两个均饱和 4,096 retained 但 normalized rune 长度差越过 70% 的 case 必须拒折。真实文本 fixture 只断实际正负关系。同 URL 不同 cleaned text 报 duplicate conflict；多字段 materialization 与 Fields 使用同一 component set；按 §16.2.2 的有界 permutation 集断 byte-stable。

**验收**：同稿异站 N_eff 精确为 1，真异源零误折；旧 caller 不填 `CleanedText` 时现有测试与编码不变；`go test -race ./consensus ./simhash ./extract` 绿。

**提交**：`9a3455e feat(consensus): fingerprint anchored content cores`

#### 任务卡 N-3 · 长措辞血缘 ✅（`c78010b`）

**交付什么**：从 N-2 已 admission 的 cleaned text/TextRange 派生 line/sentence/paragraph fragments，按 same path + canonical value 做有界、逐字、对称 containment，给全局 component builder 加 `quote_lineage` 边；不引入时间方向、外部知识库或 LLM。

**测试先行**：95/96/97 个 `Letter|Number` runes 边界；line/sentence/paragraph 精确切片与跨界跳过；A⊂B、B⊂A、A⊂B⊂C 传递链；UTF-8/CJK/mixed；同 path 不同 value、不同行 path、共同短值不折；导航/免责声明在 anchor 外不参与，相似但非逐字包含的 anchor 内 boilerplate 不折；完全相同且达门槛的 anchor 内 boilerplate 明确锁为已知限制/正折叠。混合 same-root/core/lineage 图与有界 permutations 稳定；最大 8 source × 256 anchor × 3 fragment 不越结构预算，>1 KiB fragment 稳定跳过且不影响 v0/N-2 边。

**验收**：全页与 core distance 均 >3 时，长逐字转载仍折到 1；所有可判定 hard negatives 保持独立；超长 fragment 只禁用该候选，仍保留 same-root/legacy/N-2 边，且资源使用受 §16.2.2 的 anchor/window/fragment 上限约束。

**提交**：`c78010b feat(consensus): fold verbatim quote lineage`

#### 任务卡 N-4 · 折叠原因 additive 导出 ✅（`e17086c`）

**交付什么**：`consensus.FoldReason`、`Support.fold_reason,omitempty`、witness-subtree 条件式 `Agreement.fold_reason,omitempty`；同步 `models.MultiExtractSupport/Agreement`、extract 投影、consensus 手算预算、multi REST 编码预算与 MCP strict decoder，以及 Answer service/handler/MCP 的 Agreement enum/allowlist。N-4 不新增 Extract REST decoder、`AnswerEvidence` 或 Search 字段。

**测试先行**：三原因单独与 mixed component；同 pair 多信号优先级；Kruskal + lexical representative + BFS parent；value group 经组外节点桥接的 witness subtree；Agreement 三态与 Support 的 source-global 反例；ambiguous conflicts；omitempty；旧无折叠 JSON；`Pages`/Supports/N_eff 不变量；`measuredOutputSize == json.Marshal`、consensus 与 multi response 两个 32 MiB N/N+1；Extract REST valid round-trip；multi MCP、Answer REST/MCP round-trip 与非法 reason 拒收。

**验收**：每个非空原因均可由内部 forest 证据重放；混合原因不伪造 Agreement 单值；N-4 本身只加 metadata，不改 N-2/N-3 已算出的 components、materialization 或 confidence 公式。⟳ MCP 已有的 EAV strict-validator 漂移（`entity_verdict`/`entity_mismatch`）须另作单关注修复，不能借 N-4 混提。

**提交**：`e17086c feat(consensus): expose source fold reasons`

#### 任务卡 N-5 · 构造集评测门 ✅（`94f9f88`）

**交付什么**：`consensus/testdata/neff/sources.jsonl` 行 schema 为 `{id,url,data,cleaned,basis}`，保存真实 cleaned + `Basis.{quote,text_range,method}`，不预存最终 hash；fixture method 必须是 §16.2.2 eligible enum，loader 必须始终用 production `simhash.Fingerprint(cleaned)` 派生 legacy `SimText`。`cases.jsonl` 严格行 schema 为 `{id,class,language,sources,use_cleaned_text,hard,note,expect[]}`，其中 `sources` 必须非空、ID 唯一，`expect` 必须非空；required bool `use_cleaned_text` 决定本次 Merge 是否注入 `CleanedText`（`compat` 必须为 false，其余 class 必须为 true）。`class ∈ {core_mirror,lineage,independent,mixed,compat,bridge}`，`language ∈ {en,zh,mixed}`，`hard=true` 表示阈值邻界/桥接/boilerplate 等对抗样本；`expect[] = {path,value,pages,independent_roots,want_v0_independent_roots?,support_reasons,agreement_reason}`。`value` 保留 JSON scalar 类型；`support_reasons` 是 required canonical URL→非空枚举的 exact map（代表/无 reason URL 不出现，完全无 reason 时为 `{}`）；`agreement_reason` 是 required enum-or-null，`null` 精确要求 omitempty。`want_v0_independent_roots` 全局可选，但 `core_mirror`/`lineage` 每条 expectation 必填且必须严格大于 `independent_roots`，防止“真实增量”空门。

**构造切片**：en/zh 同稿异 chrome；1 origin + 7 mirrors；8 真独立源；同主题同值 hard negative；95-rune 短片段拒折；长 lineage；混合「3-copy cluster + 3 independent ⇒ N_eff=4」；缺新字段兼容；mixed-signal bridge；有界 permutations。MaxSources=8，文档里的「N」不得写成实际不可达的 10；3/4 精确距离边界留 unit test，golden 断真实 fixture 的诊断 relation。

**门（确定算法，不用近似数）**：镜像 expectation 逐行 `independent_roots==1`，召回 100%；独立集逐行 `independent_roots==pages`，误折 0；mixed case 精确相等；每条 expectation 都精确断 `support_reasons` 和 nullable `agreement_reason`。`core_mirror` fixture 必须没有达 N-3 门槛的候选 fragment，且每条 expectation 都是 `agreement_reason="near_duplicate"`、非空 support reason map 的所有值也都是 `near_duplicate`；`lineage` fixture 必须诊断 legacy/core distance 均 >3，且 reason 同理精确为 `quote_lineage`，防止两类实现互相代打而空过。有 `want_v0_independent_roots` 时，用同一 source 清空 `CleanedText`、保留 production legacy `SimText` 重跑；core/lineage 的 required 且严格变小断言明示 v1 真实增量。每次打印 class × language × hard 分层成绩。至少 12 cases，六个 class 均非空，`core_mirror` 必有 en+zh；机器定义的 hard-negative bucket 必非空：`hard=true && class=independent && len(sources)>=2`，至少一条 expectation `pages>=2`，且所有 expectation 均 `independent_roots==pages`、`support_reasons={}`、`agreement_reason=null`。fixture unknown field、空/缺引用、重复 ID、越预算、source 未被任何 case 引用或 required bucket 空洞一律测试失败。

**验收**：门并入普通 `go test ./...`，随后接 §10 CI；阈值或 tokenizer 任何改动都必须过本门。benchmark 只记报告，不作为时间门。

**提交**：`94f9f88 test(consensus): gate effective source independence`

#### 16.3.1 提交序列与非目标

```text
N-1  97490db  docs(plan): lock effective-source independence v1
N-2  9a3455e  feat(consensus): fingerprint anchored content cores
N-3  c78010b  feat(consensus): fold verbatim quote lineage
N-4  e17086c  feat(consensus): expose source fold reasons
N-5  94f9f88  test(consensus): gate effective source independence
REV   d213376  fix(mcp): reject duplicate multi response fields
```

每卡都先红测试后实现，且 `go test ./...`、`go test -race ./...`、`go vet ./...`、`go build ./...` 与 `git diff --check` 全绿才 commit。**非目标**：方向性转载图/发布时间先后、claim 级不同 component、改 answer 置信度公式、步 4 重排、ledger 持久化、Wikidata/外部查重、提高 `MaxSources`。

> **收口状态（2026-08-12）**：N-1…N-5 五卡全部完成；生产增量按内容核、逐字血缘、可审计折叠原因三个单关注提交落地，构造集以 33 source / 12 case 对六类信号做 exact gate。分卡审计全部 PASS；随后对 `c7189c2..c12fbdc` 做整块审查，发现 MCP multi strict decoder 可被重复 `fold_reason` 的 last-wins 语义绕过，已由红测试复现并在 `d213376` 递归拒绝 exact/Unicode-escaped duplicate key，发现者复审 PASS。P2 也已完成；P3 的 R-1…R-5 已收口，下一步为 §17 的 R-6 pinned Qwen relevance replay 与 scorecard。

> **EN —** N_eff v1 preserves document-global components and adds bounded, anchor-centered content-core similarity plus exact long-fragment lineage as conservative union edges. Missing enhanced input retains v0 compatibility; malformed or hard-size-invalid input fails explicitly, while richer derived inputs are sampled deterministically within fixed bounds. Sampling may miss an enhanced fold and thus overcount independence relative to an unbounded ideal, but never removes the v0 signals. N-2/N-3 may change winners, materialization outcomes, and confidence through the new N_eff, while their schemas/formulas stay intact; N-4 itself only adds fold metadata from a deterministic forest.

### 16.4 P2 实施与审查

```text
E-7  18411d7  fix(eav): preserve verbatim evidence quotes
E-8  c60ec5f  feat(eav): allow private managed llm endpoints
REV  22c379e  fix(llm): isolate response format capability cache
DEP  1a2c601  fix(deploy): pass managed eav configuration
```

E-7 先以最终冻结提示词做一次空目录全量重录，再对预先声明的 5 文档 / 22 标签做一次完整文档组 paired repair；两阶段成绩、晋级规则和 hard residual 均在 §15 留痕。E-8 在独立 worktree 测试先行实现，应用到主线后重跑合并门；managed EAV 私网授权没有进入任何 request/BYOK 配置或共享 outbound policy。两卡分项审查均 PASS。随后对 `6cb3783..c60ec5f` 做整块审查，发现 package-global、仅按 BaseURL 缓存的 `rfSupport` 仍可让 request BYOK 与 managed EAV 互相污染 response-format 降级状态；红测复现后，`22c379e` 将其收为 per-client 指针缓存、按 `(BaseURL, model)` 隔离并以 128 项 FIFO 封顶。文档机械审查又发现 `.env` 的 EAV 键未进入主 Compose 容器，`1a2c601` 以安全默认值逐项显式映射。发现者与交叉审查复跑均 PASS；最终全仓 test/race/vet/build/diff-check 与 Compose 默认/覆盖解析门全绿。

### 16.5 运行环境备忘

- **重录**：`EAVCORPUS_API_KEY=ollama EAVCORPUS_MODEL=gpt-oss:120b-cloud EAVCORPUS_BASE_URL=http://127.0.0.1:11434/v1`（Mac 需先 `ollama serve`；record 模式走普通 HTTP client，不受服务器公网白名单限制）。
- **本机 managed EAV**：如需让服务本身连接私网 Ollama/vLLM，显式设 `PURIFY_EAV_LLM_ALLOW_PRIVATE=true`；这不会放宽请求 BYOK。
- **服务器冒烟/生产待配**（非代码）：部署侧仍需一把公网 OpenAI-compatible key（若不使用自托管 managed endpoint）和 Brave key，才能点亮 `/answer` happy path。VPS 纪律见 AGENTS.md/部署备忘：严禁动 `/opt/purify`、`/opt/lithium`。
- **已知格式漂移**：`models/response.go` 存在先于本相位的 gofmt 漂移（注释对齐），顺手修请单独 chore commit。

### 16.6 纪律（照旧，一行不减）

单关注提交 · 测试先行 · `go test ./...` + `-race` + vet + build 全绿后才 commit · `git diff --check` · 不带任何 AI 署名尾注 · 公开契约 additive-only · 不 push/merge/tag/deploy 除非明确决定 · PLAN.md 三层结构与非目标不可推翻 · MASTERPLAN 状态标记（✅/🚧/⬜/⟳）随实况回写。

> **EN —** Handoff from waypoint `c7189c2`: P1 N_eff v1 and P2 EAV calibration are complete. N_eff now adds bounded content-core fingerprints, quote lineage, and additive fold-reason export on top of the existing same-root/whole-page-simhash folds. EAV now enforces verbatim extraction quotes through the golden replay gate and can opt only its operator-managed client into private networking while request BYOK remains public-only. P3's R-1 design, R-2 pure core, R-3 isolated strict runtime/config, R-4 internal relevance orchestration, and R-5 explicit relevance contract are complete; R-6 pinned relevance replay/scorecard is next, while production activation still requires the separate R-6a deployment admission.

---

## 17. P3 · 0.6B relevance reranker + trust-aware ranking（R-1 设计定案）

> **状态（2026-08-11）**：R-1 只锁实况、契约、资源、安全、评测与拆卡；本卡不改生产排序、不加公开字段、不下载模型、不启动 sidecar。v1 默认始终是 `provider`，relevance/trust 只能显式 opt-in；真实 0.6B recording 门未过前不得在生产启用 relevance capability，trust oracle 门未过前不得开放 `trust`。

### 17.1 先纠正五个实况

1. **当前不是 provider 聚合器，也没有重排器**。`search.Service` 只持有一个 `Provider`；生产顺序是 provider 原序 → domain / canonical URL 过滤 → 可选 `title+snippet` SimHash component 折叠（provider 最早成员胜）→ `limit` → 最多前 5 条 enrichment。P3 不顺手造多 provider federation，自建索引仍是 PLAN 的明确非目标。
2. **公开 `limit` 目前同时限制 provider fan-out**。`limit=5` 只请求 5 条，任何 scorer 都不可能把 provider rank 6–20 拉进 top 5。opt-in relevance/trust 必须把内部候选池与公开输出数分离；默认 provider 模式继续传原 `limit`，保持旧调用、cache key、排序与成本。
3. **`SearchResult.Score` 是 provider score**。它为 nil 就表示 provider 未提供（当前 Brave 即如此），不得覆盖、补造或重解释为 cross-encoder 分。baseline cache 也明确只存 provider-neutral 的 title/URL/snippet/provider score/time，不存 ranking 输出、artifact、cleaned text、schema、subject 或任何凭据。
4. **EAV 与 N_eff 现在都来不及影响 Search 候选**。EAV 只在带显式 `ExpectedSubject` 的 multi extraction 内对成功提取页运行；N_eff 又依赖 final URL、全页 SimText、CleanedText 与已定位 Basis。`/answer` 先选最多 8 个 Search URL，随后才 `ExtractMulti`，所以现有 `MultiExtractSource.Entity` / Agreement 并不是 Search 输入；generic query 也不能被静默猜成实体 subject。
5. **`fold_reason` 不是逐页 N_eff 分，也不足以找 component**。`Agreement.IndependentRoots` 属于某个 path/value cohort；`Support.fold_reason` 只记录 canonical-URL forest 中非根页的 parent-edge 原因，不含 parent/component ID。空值可能是 lexical representative、真 singleton、组外 bridge 或无 support，绝不能获“独立”奖励；lexical representative 更不能冒充最相关页面。P3 必须从 consensus 同一份 independence plan 导出窄的 component analysis，Search 不复制 DSU，也不从公开字段反推。

### 17.2 公共模式与兼容边界

新增 additive 请求枚举 `ranking`：

```text
provider   现有行为；省略值与显式 provider 完全等价
relevance  20 候选 metadata cross-encoder 重排
trust      relevance + 显式 subject + 有界 artifact/EAV/N_eff group-first 排序
```

- `SearchRequest.ranking,omitempty` 省略默认 `provider`。`provider` 继续使用公开 `limit` 作为 provider fan-out；`relevance|trust` 固定向 provider 请求最多 20 条，再输出 caller 的 `limit`。
- `SearchRequest.expected_subject,omitempty` 复用 `models.SubjectSpec`；**仅** `trust` 可带且必须带。进入 clone/capability/provider 之前先锁：Name trim 后非空、合法 UTF-8、无 control、≤`eav.MaxSubjectBytes`(1,200 bytes)；Hint 合法 UTF-8、无 control、≤`eav.MaxHintBytes`(512 bytes)。N/N+1、normalization-empty 与 direct Go caller 都必须拒绝；`provider|relevance` 带 subject、`trust` 缺 subject、unknown/null/case-smuggled enum 均 `INVALID_INPUT`，不从 query/title/domain 推断主体。
- `trust` 的公开输出上限锁为 5；省略 `limit` 时默认 5，显式 6–20 拒绝。它在 metadata relevance 排序后最多评估 8 个候选（新增独立常量 `MaxSearchTrustCandidates=8`，与 `consensus.MaxSources` 相等），因此能用 rank 6–8 的独立页替换 top-5 镜像；只评估原 top 5 会改变次序却不能降低 top-5 镜像占比，禁止这种空实现。Defaults 必须先解析 mode：`provider|relevance` 缺 limit→10、`trust`→5；默认 outer timeout 同理前两者→30s、trust→60s（仍允许显式 1–120s）。MCP 为了 additive compatibility **保留**现有 schema 的 `limit=10`/`timeout=30` default annotation，但 description 补充 mode-aware 规则，server payload 按 raw presence 先解析 ranking、绝不把 annotation 物化为请求值。所以省略仍服务端得 trust=5/60；某客户端若真的显式发 `limit:10`，那是 trust 的越界输入应拒绝，不能为它放宽上限。
- mode-aware default 只能有一个 pure resolver，models `Defaults`、service preparation、HTTP rate/outer-context 都先调它，不得在 handler 预先把0写死成30。MCP `searchPayload` 须保留 limit/timeout 的 raw presence：省略时 outbound JSON 仍 omitempty，但本地 `context.WithTimeout` 使用 resolver 得到的 effective 30s/60s，不能用将被省略的0建立立即取消的 context；显式值原样发送且本地同值。HTTP omitted/explicit、service direct caller、MCP local deadline/outbound body 四层对 provider/relevance/trust 的 exact 门不可少。
- 未配置显式模式所需 capability 时，在 provider 调用前返回 `SEARCH_UNAVAILABLE`。capability 已配置但 scorer 超时/panic/坏响应时，整批 score 作废，**跳过 trust**，并完整回到旧 pipeline：metadata DSU 取 earliest-provider winner、provider order、再 truncate；opt-in summary 为 `status=degraded/reason=reranker_failed`，没有任何伪造 relevance score。outer context 真取消仍返回整体 `TIMEOUT`。
- domain/URL 过滤后零候选时 scorer/fetch/judge/analyzer 全部零调用：relevance 返空 results + `applied,candidate_count=0,rerank_ms=0`；trust 按无可消费 independence 返空 results + `degraded/trust_unavailable`，四个 trust counter 均为0、rerank/trust timing 都显式0。HTTP/MCP 空结果 presence 门锁住这一形状，不依赖 vLLM empty-documents 行为。
- trust 的 artifact/analyzer 局部失败可形成 `status=partial`：只消费已证实的独立性，无法 analysis 的页不计 effective component；一个可用 independence signal 都没有时退回 relevance 顺序并标 `degraded/trust_unavailable`。EAV provider/referee error 继续按现纪律产出 `entity_uncertain`，另带 operational degradation 供 partial accounting，绝不升级成 mismatch。默认 provider 响应不出现任何新字段，旧 JSON bytes、Answer 的 source 选择、MCP strict response 与 `BypassProviderCache` 语义均须精确不变。

### 17.3 候选流水线（顺序锁定）

```text
provider baseline (provider: N；relevance/trust: 20)
  → normalize + domain filter + exact canonical-URL dedup
  → relevance batch score all remaining candidates（opt-in only）
  → optional title+snippet SimHash components
       provider mode: earliest provider member wins（旧行为）
       relevance/trust: highest relevance wins，随后 provider rank / URL tie-break
  → relevance total order
  → trust only: fetch top min(8,N)，EAV + independence analysis，group-first
  → public truncate（trust ≤5；others ≤20）+ contiguous Rank
  → requested content/verify/schema enrichment，复用已有 artifact
```

- normalization 即使丢掉非法 URL 也不得重编 provider 次序：`baselineResult` additive 保留原始 `ProviderResult.Rank`，cache clone / size accounting 同步更新但 key 不变；所有 tie-break 与 opt-in `provider_rank` 用这个原始 rank，不得用过滤后 slice index+1 冒充。默认 wire 仍不增字段。
- metadata SimHash 仍在完整候选图上取传递闭包，绝不能先 truncate；`deduplicate=true` 时 relevance/trust 取每 component 的最高 relevance member，`deduplicate=false` 时跳过该折叠并把全部 canonical candidates 按 relevance 排序。opt-in scorer 先给全部候选打分，再决定 component winner，避免 provider 最早镜像吞掉更相关原页。
- trust pool 明确分支：`deduplicate=true` 取 metadata-component winners 的 relevance top 8；`false` 直接取未折叠 relevance top 8。cheap metadata component 大小不冒充 N_eff。`candidate_count` 是 domain/exact-URL 后实际送 scorer 的页数；`attempted_pages` 是 top-8 实抓数；`evaluated_pages` 是去 stale/final-alias 后真正进入 independence analysis 的唯一 final URL 数；`effective_sources≤evaluated_pages` 只陈述该集合，不能暗示覆盖 provider 全 20。
- fetch 完成后先验证 status/final identity，并在 EAV/analyzer **之前**做全 outcome 的 effective-identity coalesce，继续满足 MCP 的全结果 identity 唯一不变量。404/410 先按现语义从最终 results 删除并计 `DroppedStale`。其余 outcome 的 effective identity 是 validated canonical final URL；完全没有 final identity 的 fetch error 才回退 requested canonical URL。每个 identity 若有至少一个可分析2xx，失败项无权当 winner，成功项按 `relevance DESC → original provider rank ASC → requested canonical URL ASC` 选一个，其余成功/失败都只计 `Deduplicated`、不计 `failed_pages/evaluated`；若全是失败，则按同一全序只留一个 operational unknown，计一次 `failed_pages`，最终返回时带 `stage=trust`，其余失败计 `Deduplicated`。只有留下的2xx进 EAV/analyzer；不抓 rank 9+ backfill。所以“高分404/低分200同 final”和“高分500/低分200同 final”都必须保留200而不让失败项吞掉成功；500+500同 identity 只留最高 relevance unknown；无 final 的 fetch-error requested URL 与另一成功 final URL 碰撞时仍由成功项胜出。同 final URL 的两个成功但不同 snippet/Basis 也只会有一个进入 consensus，不触发 `ErrDuplicateSourceConflict`。
- `trust + include_content/verify/schema` 对同一 URL 只抓一次，同一个 bounded immutable artifact projection 先供 trust，再供最终 top-5 enrichment；禁止再调一次 `ExtractMulti` 造成双 fetch/双 extract。
- 抽出窄 `PublicArtifactFetcher`，不为了 trust 伪依赖 receipt signer；R-7 在现有 `ArtifactService` 内扩出**直接返回 bounded Search projection**的同一 fetch/sanitize 路径，不在 caller 长期 retained 一个已物化无界 ancillary slices 的 full Artifact。R-7 还必须在 EAV orchestration **未吞 error 之前**新增 additive detailed API：保留 extraction provider/referee 的 completed/error/panic stage，再投影出旧 `AnchoredExtraction`/`JudgeDocument` 行为。共享 typed seam 输出 `{attribution, operational_error, failure_kind, stage}`，但**不自作主张映射 panic**：Extract 继续 20s judge 预算，普通 provider/referee error 投 uncertain，panic 必须重抛/等价送入现有 `extractMultiSource` recover，仍是整页 extraction_failed/排除；Search 才在 8s parent context 内把 panic/error 投 uncertain + per-page partial。verdict/quote 校验与 observation stamp 仍只有一份。Extract 与 Search 不能在已吞掉 error 的 `SourceJudge` 外层伪称能恢复 completion，也不能复制 `judgeSourceAttribution`；它们复用同一 process-owned judge/client/cache、绝不再建第二 runtime。Answer 始终发省略/`provider` ranking；即使 reranker enabled/unavailable也不得被调用，捕获的 SearchRequest 与最终 URL 序列须与 P3 前 exact 相同，随后仍只做自己的一次 `ExtractMulti`。
- trust 资源边界不借 outer timeout 碰运气：全进程最多 2 个并行 trust request；每请求本地 worker 上限4，并与现有 Search enrichment 共享进程级 4-slot artifact-source semaphore，所以普通 Search+trust 的 source work 合计≤4。ExtractMulti/Answer 保留现有独立 sourceSlots=4；P3 不新建第三个 pool，所以跨 Search+Extract 的 artifact work 上界仍是现存 8，另以一个共享 managed-EAV judge semaphore 把两路 LLM judge 总并发压在4。默认 outer=60s 时整个 trust phase 还有独立 deadline `min(40s, outer_deadline-5s)`，保留至少5s 给降级排序/编码；scorer 5s、单页 fetch 8s、Search judge（含可能 referee）8s 再取 phase/context 剩余值。两个 trust request 共享4 worker 时未完成页会在 phase deadline 变 partial，不拖到 outer `TIMEOUT`；等 slot 可 cancel，late result 丢弃，panic/error 只标记该页。需有 2/3 trust request、Search 4/5 source、跨 Search/Extract 8/9 artifact、共享 judge 4/5、phase/outer deadline、cancel/panic/leak 门。
- bounded projection 必须在 ArtifactService 的 fetch/sanitize worker **交付 caller 之前**构造：worker 内部短暂可见的 full `extract.Artifact` 不得跨 handoff retained，Links/Images/Quality 等 ancillary fields 在同一 worker 内释放；只返回 immutable **bounded Search artifact projection**。投影仅保留 final URL/status/snapshot/fetchedAt 等≤32 KiB metadata 及 cleaned/raw 各≤4 MiB；EAV、snippet alignment、最终 content/verify/schema 都消费这一投影，不为 top-5 再 fetch。每请求 text≤64 MiB + metadata≤256 KiB，total≤65 MiB，2 个 active trust request≤130 MiB；超任一单页/聚合界只使该页 operational failure。该 projection 与 8 MiB text / 256 KiB metadata / 65 MiB total 的 N/N+1、丢 ancillary 后单-fetch heavy 回归必须同卡锁住；不能用 full Artifact 的无界 slice 宣称 64 MiB 上限。

### 17.4 基础 scorer 契约与预算

`search/rerank/` 暴露 provider-neutral pure boundary；reference adapter 首版只实现 vLLM-compatible `/v1/rerank`：

```text
Score(ctx, {
  query,
  candidates: [{stable_id, provider_rank, text}]
}) -> [{stable_id, relevance_score}]
```

- 内部 `stable_id = lowerhex(SHA-256("rerank-candidate-v1\0" || canonicalURL UTF-8 bytes))`，不得使用数组位置或 Go map 序。adapter 只把它与 input index 建立当次映射；vLLM wire 不传/不回 stable ID，且 response `results` 本来可按 score 排序，所以绝不信返回顺序。
- pinned vLLM 请求 exact 对象是 `{model,query,documents,top_n}`，`top_n=N`；响应 root exact 形状是 `{id,model,usage,results:[{index,document,relevance_score}]}`，v0.23.0 nested exact 形状是 `usage:{prompt_tokens,total_tokens}` 与 `document:{text,multi_modal:null}`，两者字段都必现。必须校验 response model 与 profile served-model ID 相同、`id`≤256 bytes、两个 token count 均为 0..1,000,000 且相等、`document.text` 逐字回显输入且 `multi_modal` 必为 null；result 数量必须为 N、index 唯一且刚好覆盖 `[0,N)`。unknown/duplicate/Unicode-duplicate/case-smuggled/trailing/null/missing/extra 一律整批失败。score 必须 finite 且在 `[0,1]`；该分数只是 model-relative relevance，不宣称校准概率。
- relevance total order 固定为 `relevance_score DESC → original provider rank ASC → canonical URL ASC`；不得用 float epsilon 造“近似 tie”，不得修改 caller slice。零分合法，NaN/Inf/<0/>1、wrong count、duplicate index、late success 全批失败。
- 模型输入只含现有 `normalizeProviderResults` 校验/归一后的 title/snippet，不再 lower/去标点/改写，也不含 URL/hostname、cleaned content、凭据或 caller prompt。精确构造为 `titlePart=UTF8Prefix(title,1536 bytes)`，`document=titlePart+"\n"+UTF8Prefix(snippet,6144-1-len(titlePart))`；换行永远存在且计入 6 KiB。query 用 `prepareRequest` 已归一的 query，沿用 400 rune / 50 word 上限；N≤20；aggregate documents≤120 KiB，最终 canonical JSON request≤256 KiB、response≤1 MiB，N/N+1 均有门。input digest 在此裁剪后的 canonical request 上计算；JSON escape expansion 超限就整批降级，不放宽预算。
- v1 只接受编译时认证 profile `qwen3-reranker-0.6b-v1`：exact model `Qwen/Qwen3-Reranker-0.6B`、HF revision `e61197ed45024b0ed8a2d74b80b4d909f1255473`、normalized `[0,1]` score domain。固定 instruction 逐字为 `Given a web search query, retrieve relevant passages that answer the query`，它来自 server-side `qwen3_reranker.jinja`而不是 API body。recording manifest 必须同时固定 profile/instruction text+version/template SHA/vLLM image digest+version/HF revision/served-model ID/`hf_overrides`/`max-model-len`；只有 R-6 晋级的 manifest 才能进入编译时 allowlist。startup 对**可观测的** profile/config/endpoint 与 committed manifest ID exact fail closed，response 再校验 served-model ID；镜像、权重、模板和 argv 的真实性由项目控制 sidecar 的 deployment admission/attestation 门负责，不能谎称标准 API 可远程证明。任一认证 tuple 变化都必须重录、重审并更新 allowlist；不接受“改一个任意 MODEL 环境变量即沿用旧成绩”。
- scorer child timeout 默认 5s、配置最大 10s且不得超过 outer deadline；进程共享 4 slots，第 5 个等待可 cancel。panic、4xx/5xx、malformed/oversize body 只出稳定 domain error，不泄 endpoint、model、document 或 key。

### 17.5 真正的 trust input 与 component analysis

新增只读 `consensus.AnalyzeIndependence(ctx, []SourceResult)`（最终命名可等价，但语义不可缩）：与 `Merge` 共用 URL/Data/Basis/CleanedText admission、`prepareSource` 与 `buildIndependencePlan`，返回按 canonical URL 的 `{component representative/id, component_size, parent_url, parent_reason}` 及 effective component count；不复制/重写 same-root、legacy SimText、content-core、quote-lineage 的任一算法。shared prepare/core/lineage/pair loops 新增 context-aware 路径，在每 source、anchor scan 有界 chunk 与 pair 边界检查 cancel；`Merge` 用 `context.Background()`/等价 no-cancel 路径保持旧契约。Search 必须同步等 analyzer 退出，cancel 后返回稳定 context error 并释放 projection，禁止“丢掉 late goroutine 但它继续持有65 MiB”。它是内部分析投影，不改变 `Merge` wire、winner 或 materialization；定向 cancel/race/leak 门必须证明结束后 active analyzer=0。

Search 对每个成功 artifact 构造诚实的 analysis source：

1. URL 用 validated final URL；`SimText=simhash.Fingerprint(cleaned)`；`CleanedText=cleaned`。
2. 总有内部标量 `page=true`，但它**没有 Basis**，所以只让同一分析 cohort 成形，不能凭空触发 core/lineage。
3. snippet 是唯一可选增强 anchor，且 admission 局部 fail-soft：只对非空、合法 UTF-8、≤8 KiB 的 canonical provider snippet 调 `evidence.AlignValueContext(ctx,snippet,cleaned,"")`，经共享的 unsigned align/validate/stamp helper 复核 quote/range/method，selector 清空或限在4 KiB，并从同一 artifact 填 SnapshotID/FetchedAt，才加 `snippet=<actual snippet>` + Basis。>8 KiB、empty/unlocated、selector/range/observation 非法只省略该 anchor，不污染整批，`page` + same-root/full-page legacy 仍有效；8 KiB N/N+1、4 KiB selector N/N+1 与 stamp exact 都是门。不同 snippet 不伪造共同 value，不用 raw HTML selector 再扫 4 MiB 文档。
4. independence 与 EAV 是完全分离的两轴：**所有成功 artifact 都先做 verdict-neutral analysis**，不把 expected subject、entity verdict 或 EAV Evidence 伪造成 consensus claim/Basis。EAV 只在 fusion 阶段把 explicit mismatch 降到末尾；match/uncertain/error 不改 component 图、不加分，operational error 只触发 partial accounting。这避免“同内容一页 match、一页 judge timeout”被假拆成两个 singleton。
5. analyzer 输出的 lexical representative 只供稳定 component identity/forest audit。Search 对每个 component 自己选择最高 relevance 的非 mismatch member，绝不拿 lexical root 当排序赢家。公开 `component_id = lowerhex(SHA-256("search-component-v1\0" || 对 sorted unique canonical final URLs 逐个追加 uvarint(byte_len)+UTF-8 bytes))`；长度前缀消除 URL 边界碰撞，不泄新信息，也不把数组位置当身份。

### 17.6 trust fusion：不用伪精确乘法

PLAN.md 的 `relevance × N_eff × EAV` 是产品方向，不是三个已校准概率。v1 明确**不**制造不可解释的 composite float，也不把 component size 越大反向奖励。采用约束式 group-first：

1. 对成功 independence analysis 的候选，先排除 explicit mismatch，再按 component 分组；每组 relevance total order 第一名是 Search leader。EAV match/uncertain 不改组与组内次序。
2. 最终 bucket 全序锁死为：`analyzed non-mismatch component leaders → operationally unknown/unanalysed neutral candidates → analyzed non-mismatch non-leaders → explicit mismatches`；每个 bucket 内均用 relevance total order。unknown 不获“新 component”身份、不计 effective source，但也不会被一串已知镜像无条件压住；只有 mismatch 是明确负证据。
3. 在线不存在 gold “relevant”标签或隐式 score threshold。只要有至少 5 个已分析、非 mismatch component，top 5 必须一组一页；不足才按上述次序补 unknown、duplicate、mismatch。“relevant component”只出现在独立 gold 评测中，不作运行时分支。
4. 若 `evaluated_pages<2`、analyzer 整批失败或没有任何可消费的 independence 结果，不把未知当独立：整个 trust 层降级为 relevance 顺序，保留已得 EAV mismatch 诊断但不用它改排序，状态为 `degraded/trust_unavailable`。
5. mixed reasons、组外 bridge、same-root/near-duplicate/quote-lineage 都只决定 component；不按 reason 任意设置不同罚分。component 的最高 relevance leader 即使在 consensus lexical forest 中有非空 parent reason，也仍可当 Search leader。

### 17.7 additive wire（仅 opt-in 出现）

- `SearchResult.Score` 原样保留。新增 `SearchResult.Ranking,omitempty`：`provider_rank`、可选 `relevance_score`，trust 时再带可选 `entity` 与 `independence{component_id,component_size,component_leader,fold_reasons[]}`。`fold_reasons` 是 component forest 的去重枚举集，wire 唯一顺序锁为 consensus priority `same_root → quote_lineage → near_duplicate`，permutation/golden/exact JSON 均按此；它不冒充某页父关系，singleton 省略。新增 `SearchResultError.stage=trust`，不得用 fetch/extract 假装。
- `SearchResponse.Ranking,omitempty` 对每个显式 relevance/trust 请求**必须存在且非 null**：`mode`、`status ∈ {applied,partial,degraded}`、`candidate_count`，trust 另有 `attempted_pages/evaluated_pages/effective_sources/failed_pages`。relevance 只能 `applied|degraded`（scorer 是 all-or-nothing）；`partial` 只用于 trust 有可用 analysis 但至少一页 operational failure。`degraded_reason ∈ {reranker_failed,trust_unavailable}` 仅 status=degraded 必需，其余状态禁止。计数口径分别是送 scorer 的 domain/URL-filtered 候选、实际发起 fetch 的 top-8、final-URL coalesce 后进 analyzer 的页、至少含一个非 mismatch member 的 component 数、以及 fetch/judge/analyzer 任一 operational failure 的去重页数；alias loser 只计 `Deduplicated`，所有计数均不得重复累加同一页。
- presence matrix 是 wire 契约：(1) relevance applied 时每个 result 的 Ranking 必含 original provider_rank+relevance_score，entity/independence 禁止；(2) trust applied/partial 时每个 result 仍必含 rank+score，judge 有结果则 entity 存在，页进 analyzer 则 independence 存在，二轴互不依赖；返回的 operationally unknown 没有 independence，并带 `stage=trust`；(3) `degraded/reranker_failed` 时所有 result Ranking 都禁止，不补造0分/provider_rank；(4) `degraded/trust_unavailable` 时 relevance 已成功，所以 result 保留 rank+score，entity/independence 只作已完成诊断、不参与排序。null、missing、empty 必须按此矩阵区分，MCP exact validator 不靠 Go 零值猜。
- `SearchTimingInfo` additive 增指针 `rerank_ms,omitempty` / `trust_ms,omitempty`：显式 relevance/trust 必有 rerank_ms（可为0），trust 必有 trust_ms（可为0）；provider 两者禁止。feature-off 其他指针/omitempty 都为空，旧 exact JSON 不变。
- `SearchResponse.Partial` 继续严格等于**最终返回 results** 中是否存在 `Errors`；全局 scorer fallback 不滥用它。top-8 中未返回的 rank 6–8 失败只使 Ranking `status=partial`、增 `failed_pages`，不凭空设 Response.Partial；失败候选若最终被返回，才带 `stage=trust` 并令 Partial=true。其他 enrichment error 可使 applied 响应 Partial=true，两个状态不互相代替。
- models、HTTP handler、MCP tool schema/argument allowlist/payload、strict response decoder/exact validators 必须同卡锁步更新。HTTP 在引入 `ranking` 成本开关前先补 recursive duplicate-key、Unicode-escaped duplicate 与 exact-case allowlist，堵住 Go decoder 的 last-wins/case-insensitive smuggling。
- Search 32 MiB preflight、service encoder、handler fallback 与 MCP body limit都计入新字段并做完整 response N/N+1。新 pointer/map/slice 必须在 cache/result clone 路径深拷；旧 MCP 客户端不能请求新模式，默认响应仍是旧 shape。

### 17.8 cache、费率与 capability gate

- baseline cache 继续是唯一 Search cache：1 minute / 256 entries / 16 MiB，存 pre-ranking provider baseline。rerank/trust 每次从 deep clone 重算；未来若加 ranking cache，必须另卡、独立有界，并把 scorer/model/instruction/algorithm version、candidate digest、subject/mode 纳入 key，绝不存 credential/artifact/cleaned。
- `BypassProviderCache` 只绕 provider baseline；不绕过也不复用 scorer/trust 输出。`relevance|trust` 的 provider candidate limit 固定 20，故不同 public output limit 可合法共享同一 baseline key；provider mode仍按旧 limit 分 key。
- 当前 `MaxSearchRequestCost=22` 被 router 与 `cmd/purify` managed Search runtime 同时当“是否注册/构造整条 Search”的 capability 门。R-5 必须新增 `MinSearchRequestCost=1` 并在**这两处同时**解耦：burst=0 仍禁用，burst≥1 就可构造/注册 baseline route；请求的实际计费由 limiter 单独拒绝。因此旧 burst=22 部署仍能跑所有旧请求，昂贵 trust 不会让整个 `/search` 消失；0/1/22/58/59 两个 capability call site 都有 exact 门。
- 锁定最坏预收单位：candidate fan-out 仍 `ceil(candidate_limit/10)`（opt-in=2）；relevance batch `+2`；trust 每页 `+5`（fetch 1 + EAV extraction LLM 2 + 可能 referee LLM 2）、最多 8 页即 `+40`。该 `+40` 不因 EAV/referee cache hit 或本次未进 gray zone 而减少，因为 admission 在执行之前；trust 已含 fetch，`include_content` 不重复收费。verify/schema 仍按最终最多 5 条分别 `+1/+2`，因此 relevance 全组合最大 24，trust 全组合最大 **59**。每种组合、cache hit/miss、referee on/off、低于 59 的 limiter 都要 exact 测试；后续只能按实测向上调整，不能无记录减费。Compose/.env/README 必须在 R-5 同卡更新 59 的运营含义，不拖到 optional R-9。

### 17.9 runtime / network / deploy 边界

- 0.6B 模型不塞进现有 Purify 镜像。当前 Go+Chromium 容器只有 2 CPU/4 GiB、无 Python/CUDA；Qwen 0.6B BF16 权重约 1.2 GB，连同 runtime/KV/Chromium 共驻没有可信余量。transport 形态允许独立 vLLM sidecar 或 managed HTTPS，但 v1 reference-certified capability 只开 pinned sidecar；默认 Compose 不拉模型、不 runtime download、不用 floating `latest`。
- 新 `RerankConfig` 全部 process-owned：`PURIFY_RERANK_ENABLED=false`、`PURIFY_RERANK_ENDPOINT`、`..._API_KEY`、`..._PROFILE=qwen3-reranker-0.6b-v1`、`..._ALLOW_PRIVATE=false`、`..._TIMEOUT_SECONDS=5`。disabled 时完全 inert，stale/invalid 其他字段不造 runtime；enabled 时 endpoint/key/profile 必填，profile 必须存在于由 R-6 committed manifest 生成的编译时 allowlist，startup 只核对它能观测的 config/endpoint/profile，不能拿 operator 字符串冒充完整 deployment 证明。endpoint≤16 KiB、key≤16 KiB、profile≤128 bytes，均须 UTF-8、无 control/首尾空白；timeout 是 1–10 的整数。endpoint 必须 absolute、无 userinfo/query/fragment，path exact `/v1/rerank`；首版不接受请求 BYOK，不把 endpoint/profile/key/document 写响应、cache、日志或 error。这些 N/N+1 与 disabled-inert 都在 config 门里。
- 公网 endpoint **必须 HTTPS**。HTTP 仅在 `ALLOW_PRIVATE=true` 且实际解析/锁定的所有地址均为 loopback/private operator sidecar 时允许，绝不让一个 bool 顺带允许公网明文 key。reranker 使用独立 immutable publicnet policy + hardened client；默认拒 literal/private/mixed DNS/rebinding，HTTPS 强制 TLS≥1.2、全部 redirect 拒绝、ambient HTTP(S)_PROXY 忽略，只认显式 `PURIFY_PROXY`。`ALLOW_PRIVATE` 只放宽该 operator-managed reranker，不能复用或修改 EAV、request LLM、Search provider、relay/compiler/webhook 的 policy。
- reference runtime 只认证 `linux/amd64`：vLLM `v0.23.0`、image child `vllm/vllm-openai@sha256:3a1e7f5904e1a1192a02aa0086ceaffc33985d7044c7bb25b3a43d61bdbe3ac0`（multi-arch index 仅作 provenance：`sha256:6d8429e38e3747723ca07ee1b17972e09bb9c51c4032b266f24fb1cc3b22ed8f`）。required argv exact 为 `vllm serve Qwen/Qwen3-Reranker-0.6B --revision e61197ed45024b0ed8a2d74b80b4d909f1255473 --tokenizer-revision e61197ed45024b0ed8a2d74b80b4d909f1255473 --served-model-name Qwen/Qwen3-Reranker-0.6B --runner pooling --max-model-len 8192 --no-enable-prefix-caching --hf-overrides '{"architectures":["Qwen3ForSequenceClassification"],"classifier_from_token":["no","yes"],"is_original_qwen3_reranker":true}' --chat-template /run/purify/qwen3_reranker.jinja`，**禁止**把 key 放进 argv。reference profile 显式禁用 vLLM prefix cache，避免 replay/scorecard 的重复 query 借 KV 命中伪造 cold latency。sidecar 只从 deployment secret store 注入 `VLLM_API_KEY`；Purify 的 API key 配置引用同一 secret，adapter 固定发 `Authorization: Bearer <key>`，两边都不把 secret 写入 Compose literal。挂载 template SHA-256 必须是 `e1ee98e69aab7b2da366edf1c50efcef37e34b4a0c50fb816336213e68d9047a`；production model snapshot 预置且 `HF_HUB_OFFLINE=1`，不在启动时下载 moving main。arm64/其他平台必须独立录制、定 digest 与重过 R-6，不能借 multi-arch tag 冒充已认证。
- R-3 先提交人工审过的 pure reference descriptor、exact `env-policy` 与 redacted digest 算法：每个允许 name 预声明为 public 或 secret、required/optional 与值约束；unknown/duplicate name 必须在读取/序列化 value 前拒绝，新增 name 先走独立 policy review。policy 从空环境构造目标 child env，不继承 pinned image 的 build/usage `VLLM_*`；reference 只允许 required `HF_HUB_OFFLINE=1`、`VLLM_NO_USAGE_STATS=1` 与 secret `VLLM_API_KEY`，其他 `VLLM_*`、`HF_*`、`TRANSFORMERS_*`、`TOKENIZERS_*`（包括 `VLLM_USE_FASTOKENS`）都不在当前 allowlist。missing/wrong/duplicate telemetry opt-out fail closed；key须满足1..16 KiB、UTF-8、无control/首尾空白，并与 Purify 端引用同一 immutable secret ref，无法比较ref时只在内存 constant-time 比值。redacted env按name字节序排序，唯一secret仅记 `<present>`，digest锁为 `lowerhex(SHA-256("rerank-env-v1\0" || each(uvarint(len(name+"="+redactedValue)) || name+"="+redactedValue)))`；真实key不入argv/digest/recording/log/仓库。
- **R-3 审计纠偏**：仓内尚未选定 Docker Engine/containerd/OCI hook 等实际 deployment backend，也没有 runtime-authenticated spec/PID/secret-ref/route/egress evidence channel；因此本卡禁止做一个读取 operator JSON、manifest path、argv/env 或布尔网络声明便返回成功的“认证工具”。R-3 的 production certified-profile/deployment registry 必须保持空；enabled config 在解析endpoint、DNS或HTTP前固定 `profile_unavailable`，只交付 strict adapter/runtime 与上述 pure descriptor/policy。positive admission 另列前置卡 R-6a：先锁具体backend，再由 supervisor 自己 create/start/kill child（或由不可替换的 authenticated FD/hook 调用），从runtime读取 actual image/platform/argv/post-scrub env/mount digest与网络状态，验证同源secret、no-host-publish/route isolation/default-deny egress；任一不符必须在endpoint可达前abort/kill，且绕过supervisor不能开放Purify capability。只有 R-6a 与 R-6 manifest 晋级同时完成才生成编译时 registry entry。Purify进程只验证其可观测config/profile/served-model子集，不冒充容器controller；fixture若与nested wire不合先修设计卡，不允许adapter自由猜。
- 标准 `/v1/rerank` 运行时只能观测 served model ID，无法证明远端实际 HF revision/template/image/hf_overrides。因此 v1 的 `reference-certified` capability 只授予项目控制的上述 pinned sidecar；managed HTTPS 在 v1 只保留 transport 扩展点、**不实现/不启用生产 profile**，除非后续独立卡定义可验证的 immutable deployment attestation。`PROFILE` 不是可盲信的 operator assertion，运行时 response model exact 校验也不被夸大成权重证明。
- Purify client 只调已验证的 `/v1/rerank`，但这**不等于**关掉 vLLM 同端口的其他路由：官方明确 `--api-key` 只保护 `/v1`/`/v2`/`/inference`，`/rerank`、`/score` 及操作端点仍可未鉴权。因此 production capability 启用前必须证明 sidecar 只绑 loopback/可信私网且无 host publish，或由 firewall/reverse proxy exact allowlist `/v1/rerank` 并拒绝其余路由；sidecar network namespace 还须 default-deny outbound（离线 snapshot 不需要 DNS/Internet），即使 telemetry opt-out 回归也不能向外发送。路由隔离、egress deny 与 telemetry env 都是 R-3/R-6 安全门，不是 R-9 可选文档。
- Qwen 官方 model card锁定其 Apache-2.0、0.6B、100+ languages 与 instruction-aware 身份。官方入口：<https://huggingface.co/Qwen/Qwen3-Reranker-0.6B>、<https://docs.vllm.ai/en/latest/examples/pooling/score/>、<https://docs.vllm.ai/en/latest/serving/online_serving/>、<https://docs.vllm.ai/en/latest/usage/security/>。默认 Compose 不加模型；R-3 的 Purify 配置键须在同卡进 Compose/.env，R-9 只负责可选 sidecar profile、SBOM/归属与运维说明。

### 17.10 评测门（不是“看起来变好”）

**A. pure orchestration / ordinary `go test`**

- fake scorer 按 stable candidate ID 回分，不按 slice index；identity/reverse/rotations/固定 seed permutations 全部 exact order、Rank 与 input immutability。覆盖 tie/zero/CJK裁剪、panic/cancel/timeout/late success、slot 4/5、wrong count/index/float/range。
- candidate=20/output=N 的 N/N+1；domain/exact URL先过滤；原 provider rank 穿过过滤/cache；relevance winner替代 provider winner；metadata SimHash 传递图仍在 limit 前完成；`deduplicate=false` 不丢页。scorer 整批失败必须回旧 earliest-provider DSU/order 且 per-result Ranking 全空。默认 mode 对 provider query limit、cache hit、order、Score pointer 与 exact JSON 全回归。
- trust fake 必过：EAV match/mismatch/uncertain/error 与 independence 两轴隔离（同内容 match/timeout 仍同 component）；same-root/legacy/core/lineage/mixed bridge 与 production component/reason exact；最高 relevance leader ≠ lexical URL root；8 KiB snippet、4 KiB selector、stamp N/N+1；unlocated/超界 snippet 不造 core/lineage也不毒整批、不同 snippet 不造 lineage；top-8 可用 rank 6–8 替换 top-5 mirror；`deduplicate=false` 的全部成员仍可进 top-8；404/410、同 final URL 异 snippet、500+200、500+500、无-final fetch error 与 success-final collision 的 identity/coalesce exact门；known leaders → unknown → duplicates → mismatch exact；隐藏 rank6–8 失败只改 ranking status/counter；partial/no-signal fallback；trust+heavy 单 fetch。
- 资源/wire 构造门：trust request slots 2/3、Search source 4/5、跨 Search/Extract artifact 8/9、共享 managed judge 4/5、outer/trust-phase/worker cancel、panic/late result/goroutine leak；projection 单页 text 8 MiB/metadata 32 KiB、每请求 text 64 MiB+metadata 256 KiB/total 65 MiB、两请求130 MiB 都有 N/N+1，并断 Links/Images/Quality 不被 retained。applied/partial/degraded 的 required/forbidden/null 矩阵、hidden-vs-returned Partial 不变式、candidate/attempted/evaluated/effective/failed 计数 exact。Answer fake 必须捕获省略/`provider` SearchRequest、证明 reranker 在 enabled/unavailable 两种情形都零调用、URL 序列 exact 不变，随后仍只做一次 `ExtractMulti`。

**B. pinned 0.6B relevance replay**

- `docs.jsonl` / `labels.jsonl` / `recordings.jsonl` 分离，逐行 exact allowlist，拒 BOM/空行/非 UTF-8/duplicate/Unicode-duplicate/case-smuggle/unknown/trailing/null。三文件 case ID exact one-to-one join，拒 orphan/unused；case/query ID 非空唯一，candidate ID 唯一、provider_rank 必须刚好是 1..N，N=10..20，grade 是 0..3 整数且 IDCG@5>0。单行≤256 KiB、单文件≤16 MiB、单 string≤64 KiB、JSON depth/数组均有界。recording 保存完整认证 manifest、input digest、exact candidate-score key set、usage/latency；不存任意 fixture hash 来代替 production input builder。
- NDCG 公式 exact 锁为 `DCG@5=Σ(i=1..min(5,N)) (2^grade_i-1)/log2(i+1)`，IDCG 是 grade 降序的同式、IDCG=0 拒绝，`NDCG=DCG/IDCG`；macro 是 case 等权算术平均，gate 前不 round。不依赖 response 自称顺序，仍按 score + 产品 tie-break 重建。
- 首个 construction gate 至少 24 queries / 240 candidates，`language∈{en,zh}`、`bucket∈{lexical,semantic,entity_collision,numeric_recency,long_noisy,prompt_injection}` 为严格枚举，每个 language×bucket 至少一个非空 case。每 case 必填 `hard` boolean；loader 另从结构推导 `objective_hard = (provider top5 至少一个 grade=0 distractor && rank6..N 至少一个 grade≥2 candidate)`，并强制 `hard == objective_hard`，不能把客观 hard case 标 false 逃门，note 不参与判断。六个 bucket 每类至少一个 hard case、en/zh hard 各≥2，因此 hard aggregate 与逐 bucket 分母都不会空过。macro `ΔNDCG@5≥+0.05`；每 language 与**每个 hard bucket** delta≥0，hard aggregate≥+0.03；≥80% case non-regression；任一 case drop≤0.15。hits/total 用整数门，分母为0不得作100%。
- 24-case 只称构造准入，不能写“统计显著”。要宣称显著须≥50个独立 judged queries，用 seed `0x5055524946595231`、10,000 次 **paired bootstrap** 对 per-case delta 重采样：case 先按 ID byte-lexical 排序；自该 seed 起用 uint64 overflow 的 SplitMix64（`state+=0x9e3779b97f4a7c15; z=state; z=(z^(z>>30))*0xbf58476d1ce4e5b9; z=(z^(z>>27))*0x94d049bb133111eb; z^=z>>31`）连续出数，每轮有放回抽 `n` 次、index=`z%n`，记录该轮均值。10,000 个均值升序后以 nearest-rank 2.5 percentile，即 zero-based `[249]`，作为 95% CI lower bound，必须 >0；不把 randomization p-value 叫 lower bound。普通测试只 replay，不联网；record 命令另跑，跨硬件门 order/NDCG 而不门 bit-exact float。

**C. independently-labelled trust golden**

- gold `component_id` / entity verdict 必须人工或独立标注，绝不能拿待测 production N_eff/EAV 输出反标自己；fusion 也只能读 production analysis，不得注入 gold component。loader 强制每 case 的 pool candidate ID 非空唯一，grade/entity verdict/component_id 三字段都 raw-present、non-null，并与 pool 做 exact one-to-one key join，拒 missing/orphan；grade 必须逐候选为 0..3 整数，verdict 必须 exact 属于 `{entity_match,entity_mismatch,entity_uncertain}`，oracle `component_id` 必须匹配 `[0-9a-f]{64}`。relevance baseline 与 trust 两个 result ID 集各自必须唯一且为 pool 子集，每个 result 的 grade/verdict/component lookup 都必须成功。每 case 至少一个 grade>0 且至少一个 `grade>0 && verdict!=entity_mismatch` component，按 §17.10-B 同一公式计算的 `IDCG@5>0`，relevant-component recall 分母也必须>0；NaN/Inf/缺值/零分母直接拒绝，绝不跳过后再算 macro。所有 trust oracle case 机器强制 `deduplicate=false`，并对信号 case exact 断言 production component partition/reasons == 独立 oracle，避免旧 metadata DSU 先折掉 1+7 mirror 而让 trust 假绿。
- 严格 bucket 覆盖 `neutral|mirror_1_7|independent_8|mixed|same_root|near_duplicate|quote_lineage|mismatch|uncertain|combined|all_mirror`，en/zh 与 `N_eff_only|EAV_only|combined` 的要求分桶均非空；每 case pool/results 非空、分母可机器验证，不允许 singleton/无 expectation 空壳。
- 指标口径锁死：`K=min(5,len(results))`，`unique_component@5=topK 中 oracle component 去重数`，`mirror@5=(K-unique_component@5)/K`（K=0拒绝）；`relevant-component recall@5=顶部 grade>0 且 verdict!=entity_mismatch 的去重 component数 / trust pool 中同类 component数`（分母0拒绝）；`mismatch@5` 是 topK `entity_mismatch` 数。所有 aggregate 是 case 等权 macro。
- neutral 只要求 **Results URL+Rank 顺序** exact 等于 relevance-only，不要求含不同 mode/status 的整包 JSON bytes。全体 macro `Δunique_component@5≥+0.25`、`Δmirror@5≤-0.05`、relevant-component recall@5 不降、NDCG@5 下降≤0.02；有≥5个 relevant non-mismatch oracle components 时 unique=5/mirror=0，有≥5个 non-mismatch alternatives 时 mismatch@5=0。各信号隔离桶还须 exact order，不只看总分。

**D. scorecard / safety**

- scorecard 对同一 query/candidate 集合先做每条路径各10次 warmup（丢弃），再做100个 cold-cache / `BypassProviderCache` measured pair；sidecar 必须由 attestation 证明带 `--no-enable-prefix-caching`，否则本轮无效，不能把第2轮起的 KV hit 叫 cold。case 按 ID byte-lexical 排序，pair `i` 用 case `i mod n`；A=新鲜 provider baseline、B=同一 baseline input 的 rerank batch，偶数 pair 固定 A→B、奇数 pair B→A。不得按结果挑顺序或重试；provider cache-hit 路径另记但不进入 `rerank_ms < provider_ms` 比值，避免 provider_ms=0 空门/死门。p50/p95 用100个原始样本升序后的 nearest-rank（zero-based `[49]`/`[94]`）；记录 hardware/GPU/region/runtime image digest/model/template/revision/provider 版本与全部原始样本，不只报一个中位数。
- PLAN 的“增量成本低于 provider”只对基础 0.6B relevance batch验收：上述 paired 环境 median rerank_ms < median fresh provider_ms，且推理计费 < provider 调用计费。managed 成本保存当日官方价格快照；local 成本用明确的 GPU 每小时价 × 实测 batch 耗时/吞吐摊销公式。trust 是显式重型模式，单独出 fetch/EAV/token/retained-byte/并发成绩，不伪称便宜。
- config/network/credential leak、request/response/body 预算、HTTP/MCP strict JSON、rate combinations、32 MiB response、race/vet/build 都是发布门。没有 GPU/sidecar 的 CI 只跑 fake + recordings，不因此跳过契约测试。

### 17.11 实施卡与提交边界

```text
R-1  ✅ 7b40111 / review hardening 8c83af3：实况盘点 + 本设计定案（docs only）
R-2  ✅ c8e72f8：pure rerank core + fake executor + NDCG math（无 HTTP / 无 wire）
R-3  ✅ 9f837ee + c385830 / admission design 9697a3f / review fix e649afc：strict vLLM adapter + config + 独立 runtime + pure deploy policy；无 production factory（不接 Search）
R-4  ✅ dfc89c7：内部 rankCandidates；candidate-20 → relevance → metadata dedup → truncate（package seam/tests，无公开 request 字段）
R-5  ✅ ea7a87e + 9e83c40 + dae246d：relevance models / HTTP / MCP + Search service switch + strict duplicate guard + rate/capability 接线
R-6  pinned Qwen recordings + relevance golden + latency/token/cost scorecard
R-6a actual deployment backend/supervisor admission；与 R-6 manifest 同过才生成 certified registry entry
R-7  consensus independence analysis + shared artifact/EAV trust seam（不公开 trust wire）
R-8  group-first trust fusion + public wire + independent oracle golden
R-9  optional sidecar/profile、SBOM/Apache attribution 与运维文档（未授权不 deploy）
```

每卡单关注：先红测试、实现后定向 + 全仓 `go test ./...`、`go test -race ./...`、`go vet ./...`、`go build ./...`、`git diff --check` 全绿才 commit；卡后做只读审查，发现项另提 review-fix commit。R-6 不通过就不在生产启用 relevance capability；R-8 不通过就不开放 trust；即使两门都过，v1 请求默认仍为 `provider`。

> **R-2 收口（2026-08-12）**：`c8e72f8 feat(search): add pure rerank core` 只新增 `search/rerank/` 六个实现/测试文件。stable candidate ID、UTF-8 有界文档、exact score-set join、`score DESC → provider rank ASC → canonical URL ASC` 总序、ctx/panic/error 收敛与 NDCG@5 均由 pure fake 锁门；没有接 Search service、models、HTTP/MCP、config、cache 或公开 wire。提交前全仓 test/race/vet/build/diff-check 全绿，卡后三路只读审查均 PASS，无 review-fix。

> **R-3 收口（2026-08-12）**：`9697a3f` 先锁定诚实 admission 边界；`9f837ee feat(search): add strict rerank runtime` 只新增 strict `/v1/rerank` codec、全进程 4-slot/5s runtime、独立 DNS pin/TLS/redirect/proxy policy，以及 pure reference descriptor/env-policy；`c385830 feat(search): configure managed reranker` 再加入 default-off process config、startup early gate 与 Compose/.env/README 接线。request/response 256 KiB/1 MiB、recursive exact JSON、score/index/model/echo、HTTP private-only/public HTTPS、mixed/rebind、timeout/cancel/panic/Close 与 secret redaction均有门。R-3 没有 production constructor、registry entry、sidecar、Search service、models、公开 HTTP/MCP wire 或 cache；enabled 配置在 R-6a/R-6 前固定 `profile_unavailable`。runtime 提交另在 detached worktree 单独 test/vet/build 通过；合并快照全仓 test/race/vet/build/diff-check 全绿。卡后审查发现公共地址分类器遗漏新的 IANA special-purpose 段，并会把 deprecated/unallocated IPv6 及嵌入私网 IPv4 的 NAT64 地址送入底层 dial；红门复现后，`e649afc fix(publicnet): reject non-public address ranges` 将 IPv6 收紧到 IANA 已分配公网集合、保留明确 globally-reachable 例外并递归验证 NAT64 IPv4，`publicnet` DNS 与 rerank DNS/literal 门均锁底层零拨号。两路复审 PASS，修复后全仓门再次全绿。

> **R-4 收口（2026-08-12）**：`dfc89c7 feat(search): add internal relevance orchestration` 只改 `search/cache.go`、`search/service.go` 并新增 package-private `search/relevance.go` 及其测试。baseline cache additive 保留 original provider rank，并把单条固定预算从 64 调为 72 bytes；内部 seam 依次执行 domain filter、exact canonical URL folding、完整 ≤20 候选 relevance scoring、完整 title+snippet SimHash 传递 component、以 `relevance_score DESC → provider rank ASC → canonical URL ASC` 选择 component winner/全局排序，最后才 truncate 并重编连续 rank。exact URL survivor 显式取最低 original provider rank且对输入排列稳定；provider `Score` 不改义，内部 `rankCandidates` outcome 深拷贝 `baselineResult`，不把 relevance 写入 baseline cache。scorer error/panic/坏 score 仍是 atomic error，公开 degraded fallback 留给 R-5；默认 `Service.Search` 未接 scorer，provider query limit/order、baseline cache key/TTL/capacity/共享策略、公开 JSON 与 Answer 边界均未改变。提交前全仓 test/race/vet/build/diff-check 全绿，卡后两路独立只读审查均 PASS，无 review-fix。

> **R-5 收口（2026-08-12）**：`ea7a87e feat(search): expose relevance orchestration` 将 ranking/subject 的 mode-aware defaults、显式 `WithReranker`、20-candidate provider fan-out、结果级 `{provider_rank,relevance_score}` 与 request-global applied/degraded summary 接入 Service；scorer error/panic/坏 score 在 parent ctx 仍存活时回退旧 provider DSU/order/truncate，provider `Score` 与 baseline cache 不改义，outer cancel 仍整体 TIMEOUT，省略/显式 `provider` 及 Answer URL/单次 ExtractMulti 边界保持不变。`9e83c40 feat(api): expose relevance search contract` 同步 HTTP strict request/response correlation、mode-aware outer deadline、custom encoder 的 pre-encode canonical freeze、最坏 59 单位计费与 `MinSearchRequestCost=1` 双 capability gate；`dae246d feat(mcp): support relevance search contract` 同步 MCP raw omission/effective deadline、recursive duplicate/case/required-null guard、relevance presence/总序、生产 extraction schema/evidence/receipt/stage-flow 约束与真实有界 32 MiB N/N+1。`trust` 只公开请求 enum、expected subject、5/60 defaults 与最高 59-unit admission，Service 在 provider/cache 前返回 `SEARCH_UNAVAILABLE`，任何 HTTP/MCP trust success 都 fail closed，R-8 才增加成功 wire。R-5 不生成 production scorer factory/registry，不改 R-3 的 R-6a+R-6 双门。冻结快照全仓 test/race/vet/build/diff-check 与 gofmt 全绿；整组三提交终态复审 PASS，其中 `9e83c40`、`dae246d` 独立复审 PASS，而 `ea7a87e` 的 core 语义虽 PASS，其 JSON-tagged request 字段须与紧随的 `9e83c40` HTTP 计费、deadline 与 strict guard 作为同一原子交付审查，不把 `ea7a87e` 单独视为可部署 waypoint。

**明确非目标**：多 provider federation、自建索引、把模型/Python塞进 Purify 主容器、公开 reranker BYOK、从 query 猜 subject、抓20页做 trust、把 provider `Score` 改义、用 `fold_reason==""` 奖励独立、把 lexical forest root 当最佳页、claim级 component、方向性首发判定、修改 Answer confidence、缓存 artifact/cleaned/credential、未授权 push/deploy。

> **EN —** P3 separates a cheap metadata reranker from an explicit heavy trust mode. Default Search remains byte-for-byte provider ordered. Relevance mode scores a bounded 20-candidate pool without overwriting provider scores. Trust mode requires an explicit subject, fetches at most eight candidates once, judges entity mismatch, reuses the production independence graph, and ranks the highest-scored member of each provenance component before duplicates. No scalar “trust probability” is invented; every signal stays auditable and every deployment capability remains process-owned and opt-in.
