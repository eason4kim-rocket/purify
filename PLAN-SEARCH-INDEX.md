# 自建搜索索引 —— 「找」这一层

## Context

三层栈的实况是 **抓 ✅ / 找 ❌ / 核 ✅**:`scrape` `crawl` `discovery` `extract` `verify` `consensus` `receipts` 都已收口并有回归成绩,唯独「找」完全依赖 Brave —— 也就是唯一的发现来源在别人手里。

自建索引一步解三个问题:

1. **供给独立** —— 没有上游条款能掐住 `/search`
2. **R-6 语料阻塞消失** —— 自己爬的语料自己拥有,可持久化、可评测,不受 Brave ToS §3(b)(i)/(xiii) 约束
3. **差异化落在数据结构上** —— 索引里每页带 `snapshot_id`,别人的索引没有这一列

**范围纪律(用户明确要求)**:代码质量要好,但这是 MVP,**不要过度工程**。本仓的默认风格(全量 fail-closed、1:1 测试比、字节级契约)在这一层要收敛到「有界 + 不被封 + 数据不坏」即可。下面有明确的不做清单。

## 已定决策

| 决策 | 选择 | 影响 |
|---|---|---|
| 语言 | **中英都要** | 双 FTS 表 + 入库语言路由 |
| 索引抓取 | **轻量 HTTP**,证据仍走现有 enrichment | 索引要广、证据要深,两段本来就是分开的 |
| Provider | **只留本地索引**,移除 Brave 接线 | 零上游依赖;覆盖缺口暴露出来才知道该爬哪 |

**关键认知:两段式不是新架构。** `search/service.go` 现在就是 `provider.Search()` 出候选 → `enrichResults()` 才 fetch/verify/extract。把 `BraveProvider` 换成 `LocalIndexProvider`,**enrichment 一行不改**。

## 架构

```
seeds.json (50–200 个种子域名)
      │
      ▼
discovery.Service          ← 已存在,sitemap + robots + 首页链接,单站上限 10 万 URL
      │
      ▼
frontier 表 (SQLite)       ← 新增:待抓 URL 队列
      │
      ▼
indexer 循环               ← 新增:轻量 HTTP + robots + 每域限速
      │  cleaner.Clean()   ← 已存在
      ▼
pages + pages_fts_{en,zh}  ← 新增:FTS5 索引
      │
      ▼
LocalIndexProvider         ← 新增:实现 search.Provider(两个方法)
      │
      ▼
search.Service ──────────► enrichResults() → fetch/verify/extract/snapshot  ← 已存在,不动
```

## 复用什么(严禁重写)

| 组件 | 路径 | 用途 |
|---|---|---|
| `discovery.Service` | `discovery/service.go:110` | sitemap/robots/首页 URL 发现,已有界、去重、排序 |
| SQLite 开库范式 | `ledger/db.go:138` | WAL / busy_timeout / txlock=immediate / migrations / 0600 / 绝对路径解析 —— **照抄** |
| 安全 HTTP 传输 | `discovery/client.go:26` | `publicnet.NewPolicy` + 重定向校验的构造模式 |
| 正文清洗 | `cleaner/` | HTML → cleaned text |
| Provider 契约 | `search/provider.go:49` | `Name() string` + `Search(ctx, ProviderQuery) ([]ProviderResult, error)` |
| FTS5 能力 | `modernc.org/sqlite v1.56.0` | 已实测:external content、`bm25()` 字段加权、`snippet()`、`trigram`(`lib/sqlite.go:33713`) |

## 新增什么

```
searchindex/
  schema.go     DDL 常量
  store.go      Open/Close/迁移/Upsert/Query(照 ledger/db.go 范式)
  lang.go       CJK 字符占比判语言(~15 行,不引依赖)
  frontier.go   URL 队列:Enqueue / Lease / Complete / Fail

indexer/
  seeds.go      读 seeds.json
  robots.go     最小 robots.txt 解析(User-agent 组 / Disallow / Allow / Crawl-delay)
  fetch.go      轻量 HTTP 抓取,条件请求,单页字节上限
  run.go        discover → frontier → fetch → clean → store 循环

search/localindex.go       LocalIndexProvider
cmd/purify-index/main.go   索引器独立二进制(不跟 API 进程抢资源)
cmd/purify/search.go       换掉 Brave 接线
config/config.go           新 env
```

## Schema

```sql
PRAGMA journal_mode=WAL;

CREATE TABLE pages(
  id           INTEGER PRIMARY KEY,
  url          TEXT NOT NULL UNIQUE,   -- canonical URL
  root         TEXT NOT NULL,          -- eTLD+1:限速 / 去重 / 喂 consensus 独立性
  title        TEXT NOT NULL DEFAULT '',
  body         TEXT NOT NULL DEFAULT '',  -- cleaned text,原文只存这一份
  lang         TEXT NOT NULL,          -- 'en' | 'zh'
  fetched_at   INTEGER NOT NULL,
  content_hash TEXT NOT NULL,          -- 去重 + 判断要不要重抓
  etag         TEXT NOT NULL DEFAULT '',
  last_mod     TEXT NOT NULL DEFAULT '',
  needs_render INTEGER NOT NULL DEFAULT 0  -- 轻量抓正文过短 → 标记待精抓
);
CREATE INDEX pages_root ON pages(root);
CREATE INDEX pages_hash ON pages(content_hash);
CREATE INDEX pages_render ON pages(needs_render) WHERE needs_render = 1;

-- 两张 FTS 表都用 external content 指向同一个 pages.body,正文不重复存储。
-- 入库时按 lang 只写入其中一张,每页只被索引一次。
CREATE VIRTUAL TABLE pages_fts_en USING fts5(
  title, body, content='pages', content_rowid='id', tokenize='unicode61');
CREATE VIRTUAL TABLE pages_fts_zh USING fts5(
  title, body, content='pages', content_rowid='id', tokenize='trigram');

CREATE TABLE frontier(
  url        TEXT PRIMARY KEY,
  root       TEXT NOT NULL,
  state      TEXT NOT NULL,   -- pending | leased | done | failed
  leased_at  INTEGER NOT NULL DEFAULT 0,
  attempts   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX frontier_pick ON frontier(state, root);
```

**语言路由**:`lang.go` 数正文里 CJK 表意字符占比,超阈值判 `zh`,否则 `en`。不引语言检测库。

**查询**:两张表各取 top-N,按**名次位置**合并(bm25 分数跨 tokenizer 不可比,严禁直接比大小)。查询语言也用同一个 `lang.go` 判,命中的那张表优先。

参考查询(已实测):

```sql
SELECT p.url, p.title, p.root, p.fetched_at,
       bm25(pages_fts_en, 5.0, 1.0) AS score,
       snippet(pages_fts_en, 1, '', '', '…', 24) AS snip
FROM pages_fts_en JOIN pages p ON p.id = pages_fts_en.rowid
WHERE pages_fts_en MATCH ? ORDER BY score LIMIT ?;
```

## LocalIndexProvider

```go
// search/localindex.go
type LocalIndexProvider struct{ store *searchindex.Store }

func (p *LocalIndexProvider) Name() string { return "local-index" }

func (p *LocalIndexProvider) Search(ctx context.Context, q search.ProviderQuery) ([]search.ProviderResult, error)
```

- `Rank` 从 1 开始按合并后名次填
- `Score` 填 BM25(接口注释禁止的是**编造**分数,BM25 是真实可比分数,可以填)
- `Snippet` 直接用 `snippet()` 的产出,不自己写摘要
- 遵守 `MaxProviderResults = 20` 及各项字节上限
- `PublishedAt` MVP 留 nil

接线:`cmd/purify/search.go:91` 起,把 `BraveKey` 判空 + `NewBraveProvider` 换成索引库路径判存在 + `NewLocalIndexProvider`;无索引库时保持现有 fail-closed 语义(`/search` 不可用,其余路由照常起)。

## 必须做对的(不是过度工程,是不被封 / 数据不坏)

1. **robots.txt 必须遵守**,含 `Crawl-delay`
2. **每域名并发 1**,域间可并行;域内请求间隔 ≥ crawl-delay(缺省 1s)
3. **User-Agent 标明身份和联系方式**
4. **条件请求**(`If-None-Match` / `If-Modified-Since`),304 直接跳过
5. **批量事务写入**(每 N 页一个事务),否则爬虫写会阻塞查询
6. **有界**:单页字节上限、单域页数上限、总页数上限、每请求超时
7. **`publicnet` 策略照用**,SSRF 防线不能因为是自家爬虫就绕过

## 明确不做(MVP 边界)

- ❌ 增量重抓 / RSS 订阅 —— 阶段 2
- ❌ 任何分布式、多机、分片
- ❌ 种子管理 API / 后台 —— 就读 `seeds.json`
- ❌ 独立的调度器 / 任务框架 —— 一个 goroutine + frontier 表足够
- ❌ 查询结果缓存 —— `search` 已有 baseline cache
- ❌ 索引层的 fail-closed 认证门、TOCTOU 冻结、字节级契约冻结 —— 这是索引不是证据链
- ❌ `needs_render` 的精抓回填 —— 先只打标记,阶段 2 再补
- ❌ 重排器 / trust 接线 —— 那是 R-6 之后的事,与本计划无关

## 提交序列(Codex 风格单关注,测试先行)

```
1  feat(searchindex): add the sqlite page store
     schema + Open/迁移 + Upsert + 去重;照 ledger/db.go 范式
2  feat(searchindex): add bilingual full-text query
     lang.go 语言判定 + 双表路由 + 名次合并 + Query
3  feat(indexer): add the bounded page fetcher
     安全 HTTP + robots + 每域限速 + 条件请求 + 字节上限
4  feat(indexer): add the seed-to-frontier loop
     seeds.json + 复用 discovery.Service + frontier 队列
5  feat(indexer): run the crawl-to-index loop
     runner + cmd/purify-index + 批量事务
6  feat(search): serve results from the local index
     LocalIndexProvider + cmd/purify/search.go 接线 + 移除 Brave
```

## 验证

**单元层**:每个提交自带测试;`go test ./... && go test -race ./searchindex/... ./indexer/... ./search/...`;`go build ./... && go vet ./... && gofmt -l .`

**端到端(本机)**:
1. `seeds.json` 放 2–3 个小站
2. `go run ./cmd/purify-index -seeds seeds.json -out ./data/index.db -max-pages 500`
3. 断言:`pages` 行数 > 0、中英文页各自进了对的 FTS 表、`content_hash` 无重复
4. 起 API,`curl '/search?q=...'` 拿到本地结果
5. 带 `include_content=true` 再打一次,确认 enrichment 仍正常出 verify/snapshot

**VPS(第一批,目标 10 万页)**:
1. 确认 `/opt/purify`、`/opt/lithium` 未受影响
2. 跑索引器,记录:页/秒、磁盘占用、`pages` 行数、robots 拒绝数、失败率
3. 用真实 query 打 `/search`,人工看头部 20 条的相关性
4. 记录覆盖缺口(搜不到的东西)→ 反馈成下一批种子域名

## 还缺的两个输入(不阻塞开工)

1. **垂类** —— 决定 `seeds.json` 里放哪些域名。代码与垂类无关,可先写完再填种子。
2. **VPS 规格**(`nproc` / `free -g` / `df -h`)—— 决定第一批爬多少页。

---

# 附:2026-08-13 调研后的四项定案

调研见对话记录。以下是已落地的决定和**换方案的触发条件**——写成数字,避免以后靠感觉争论。

## 1 · 引擎:留在 SQLite FTS5

| 选项 | 结论 |
|---|---|
| **SQLite FTS5** | 10k–100k 文档很快;百万级可用。**天花板是磁盘不是引擎** |
| Tantivy(Rust) | 约为 Lucene 的 2 倍速,Quickwit/ParadeDB/Turso 都基于它。要跨语言接线 |
| Bleve(Go) | 唯一纯 Go 选项,但实测索引性能差,**不采用** |

**换引擎的触发条件(满足任一即评估 Tantivy):**

- 查询 P95 > 100ms
- 单机装不下(见下方容量表)
- 需要 Lucene 级查询能力(短语邻近、复杂布尔、分面)

在此之前不讨论换引擎。

## 2 · 存储:zstd 正文 + contentless 索引

同一批 86 页真实语料实测:

| 布局 | 占正文的 | 倍数 |
|---|---|---|
| 存明文正文(旧) | 149% | — |
| **zstd 正文 + 无内容索引** | **78%** | **省 1.9 倍** |

拆解:倒排索引本体只占正文 **39%**,明文正文占了整库 70%。

**容量表(41 GB 可用盘):**

| 每页正文 | 可容纳 |
|---|---|
| 18.6 KB(大文档页) | 约 285 万页 |
| 10 KB(典型) | 约 500 万页 |

`Store.Reindex()` 从压缩正文重建两张 FTS 表——**换分词器只需一次本地重跑,不必重爬**。这是保留正文副本的唯一理由。

## 3 · 中文:词典分词(gse),不用字符 bigram

依据:BEIR 及分词文献显示 bigram「难以区分高频搭配和真词,召回率和精确率通常都低于现代分词方法」;jieba/gse 的**搜索引擎模式**对长词二次切分,专为倒排索引设计。

- 索引侧 `CutSearch`(`内存容量` → `内存` / `容量` / `内存容量`)
- 查询侧 `Cut`(精确,索引已含子词,再切只添噪声)
- 词典编译进二进制,**首次遇到中文才加载**(+130 MB 常驻、331ms),纯英文部署不付代价
- 加载失败时中文**fail closed**——回退到别的分词器会让一个索引里出现两套不兼容的词流

## 4 · 索引供给:请求路径反哺

`enrichment` 本来就在抓取并清洗每一个返回的页面,然后把文本丢掉。`search.NewIndexFeeder` 把这些页排队写进索引。

**这是本项目相对竞品唯一的结构性优势**:Exa 的嵌入模型训练用了 144 张 H200 一个多月;Brave 靠自家浏览器的 Web Discovery Project 让用户替它爬。而 purify 的抓取**已经在请求路径上跑着了**。

- 队列有界,满则丢弃;批量写;错误吞掉——**索引是服务请求的副作用,绝不能改变请求结果**
- 错误页、过薄页、以及公开搜索路径本就会拒绝的地址,一律过滤
- `PURIFY_SEARCH_INDEX_FEED` **默认关闭**:调用方查过的 URL 变成可搜索内容,是运营者的决定;开启前应在条款中写明并提供关闭方式

## 5 · GPU 的触发条件

**现在不买。** BEIR 基准显示 BM25 在跨域零样本场景打败多数稠密模型,且**在标识符、缩写、罕见词、冷启动语料上占优**——正是 agent 搜技术文档的形态。GPU 修不了空索引。

**触发条件:当「正确答案在前 50 但不在前 5」的比例超过 20% 时**,买 GPU 上重排器。在此之前瓶颈是语料规模不是排序质量。

## 6 · 预发布 schema 约定

schema 就地修改、不做迁移。**任何 schema 或分词器变更 = 删掉索引文件重建。** 首个真实客户之后此约定作废,改为正式迁移。
