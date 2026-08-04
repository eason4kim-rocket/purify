# Purify 通用抓取实施计划

状态：仅规划，尚未开始实施
集成分支：`codex/scrape-v1`
最后更新：2026-08-04

## 1. 目标与执行顺序

先完成 Purify 的整个通用抓取体系，再启动 Search API。

第一阶段面向公开网页，质量优先，覆盖：

- 静态网页；
- 文章和博客；
- 技术文档；
- 新闻页面；
- 公开 SPA。

执行顺序固定为：

1. 建立自动化测试和 CI 基线；
2. 做稳单页 `/api/v1/scrape`；
3. 完善参数一致性、清洗和缓存；
4. 统一 Batch、Crawl、Map 和 Extract；
5. 统一 REST、SSE 和 MCP 行为；
6. 完成真实站点验收并恢复托管 API；
7. 上述工作全部完成后，再开始 Search API。

## 2. 当前已确认的问题

### 2.1 页面过早提取

浏览器路径在 `Navigate` 后只等待 DOM 短暂稳定，没有可靠等待文档主体完成解析。

本地验证结果：

- `https://go.dev/doc/effective_go` 默认请求返回 `success: true`，但正文长度为 0；
- 同一个请求增加等待 `body` 后，正文超过 10 万字符；
- 说明当前主要问题是页面加载尚未完成便开始提取，而不是网页本身无法抓取。

### 2.2 传输成功被误判为内容成功

现有多引擎调度把第一个没有传输错误的结果当作成功，没有检查：

- 页面是否存在完整的 `body`；
- 可见正文是否为空；
- 是否只抓到了 `<head>`；
- 是否为浏览器错误页或挑战页；
- 清洗结果是否真正可用。

因此会出现 HTTP 状态正常、接口也返回成功，但正文为空或严重残缺的情况。

### 2.3 请求参数在不同路径行为不一致

当前部分参数经过 dispatcher、普通浏览器、CDP 或 Batch 路径时会被遗漏，包括：

- `wait_for_network_idle`；
- `proxy_url`；
- cookies；
- `remove_overlays`；
- `block_ads`；
- 部分浏览器动作与引擎信息。

### 2.4 缓存不能准确区分输出

缓存键当前只包含 URL、输出格式和提取模式，没有覆盖 selector、include/exclude tags、headers、cookies、actions 等会改变结果的字段。

缓存还直接返回内部响应指针，命中状态和 timing 的修改可能污染共享对象并产生并发问题。

### 2.5 测试和 CI 缺失

- 除 `simhash` 外，其余主要包覆盖率为 0%；
- GitHub Actions 目前只有 tag release 和 Docker workflow；
- 没有 PR 测试、浏览器回归、接口测试或并发测试；
- Hosted API 当前 `/api/v1/health` 返回 502。

## 3. 阶段一：建立测试底座

### 3.1 确定性测试站点

建立本地 fixture server，提供以下页面：

- 完整静态 HTML；
- `<head>` 中包含延迟脚本、随后才出现 `body` 的页面；
- 延迟插入正文的 SPA；
- 301/302 多级重定向；
- 404、429、500 响应；
- 包含相对链接和图片的页面；
- 需要特定 header 或 cookie 才返回正文的页面；
- 文章、文档、导航密集、表格、代码块和中文页面；
- 大响应与超时页面。

### 3.2 CI 基线

新增 pull request 和 branch push workflow，依次运行：

```bash
go test -race -count=1 ./...
go vet ./...
go build ./...
git diff --check
```

浏览器集成测试使用固定 Chromium 版本，普通单元测试不得依赖公网或真实凭据。

### 3.3 第一笔实施提交

```text
test(scrape): establish deterministic scraping baseline
```

该提交只增加测试基础设施和当前失败用例，不修改生产逻辑。

## 4. 阶段二：做稳单页 Scrape API

### 4.1 统一抓取服务

增加统一的 `scrape.Service`，负责：

1. 应用和验证请求默认值；
2. 调用多层 fetch engine；
3. 判断原始页面完整性；
4. 执行内容提取和格式转换；
5. 判断清洗结果质量；
6. 读写缓存；
7. 组装统一响应和错误。

HTTP JSON、SSE、Batch、Crawl、Extract 和 MCP 最终都复用该服务，不再各自复制抓取流程。

### 4.2 浏览器等待顺序

普通浏览器和 CDP 使用一致的生命周期：

```text
Navigate
→ 等待 document/body 可用
→ 等待 load 或 readyState 完成
→ 根据 wait_for_network_idle 进行有界网络/DOM 等待
→ remove overlays
→ 执行 actions
→ 提取 HTML、标题、最终 URL 和状态码
```

所有等待共享请求总超时，不允许单个等待突破用户的 `timeout`。

### 4.3 质量门控引擎升级

引擎顺序：

```text
普通 HTTP
→ 普通浏览器
→ stealth 浏览器
```

规则：

- HTTP 首先启动；
- HTTP 内容完整且质量达标时直接返回；
- HTTP 内容不完整、需要 JS 或失败时升级普通浏览器；
- 只有普通浏览器失败、识别到挑战页或内容仍不可用时才启用 stealth；
- 不再让三个重型引擎无条件同时竞速；
- 所有引擎都不合格时返回 `CONTENT_UNUSABLE`，禁止空正文假成功。

### 4.4 内容提取顺序

- 默认或显式 `readability`：readability → pruning → raw；
- `auto`：比较 readability 和 pruning，选择质量更高者，必要时回退 raw；
- `pruning`：pruning → raw；
- `raw`：只执行 raw，不改变用户明确选择。

每次自动回退都记录实际使用的提取模式。

### 4.5 原子提交顺序

```text
fix(scrape): wait for complete document before extraction
feat(scrape): add quality-gated engine escalation
refactor(scrape): centralize scrape orchestration
feat(cleaner): add adaptive extraction fallback
```

## 5. 公共 API 变化

现有请求字段、认证方式、输出格式和主要响应字段保持兼容。

`ScrapeResponse` 新增向后兼容的 `quality`：

```json
{
  "quality": {
    "score": 0.92,
    "status": "good",
    "warnings": [],
    "extract_mode_used": "pruning",
    "fetch_attempts": [
      {
        "engine": "http",
        "outcome": "rejected",
        "reason": "incomplete_document",
        "duration_ms": 820
      },
      {
        "engine": "rod",
        "outcome": "selected",
        "duration_ms": 2160
      }
    ]
  }
}
```

默认质量等级：

- `score >= 0.85`：`good`；
- `0.70 <= score < 0.85`：`degraded`，可以成功返回，同时提供 warning；
- `score < 0.70`：当前候选不可用，继续升级引擎；
- 所有候选均低于 0.70：返回 `success: false` 和 `CONTENT_UNUSABLE`。

以下情况直接判为不可用，不单纯依赖字符长度：

- 缺少文档主体；
- 正文为空；
- 文档仍处于加载状态；
- 浏览器内部错误页；
- 明显的挑战页代替了目标内容。

## 6. 阶段三：参数、缓存和生命周期

### 6.1 参数一致性

确保所有引擎路径一致支持：

- headers 和 cookies；
- request timeout；
- per-request proxy；
- stealth；
- overlay removal 和 ad blocking；
- browser actions；
- CDP；
- include/exclude tags 和 CSS selector。

`engine_used`、`status_code`、`final_url` 和 `fetch_method` 在 HTTP、浏览器、CDP、缓存与 SSE 路径必须完整填充。

### 6.2 最终 URL

清洗、相对链接、图片和 metadata 的基准 URL 使用最终跳转 URL，而不是原始请求 URL。

### 6.3 缓存

- 缓存键包含所有会改变输出的字段；
- map 和 slice 使用稳定序列化；
- 缓存内部保存不可变副本；
- Get 返回独立副本；
- cache hit 只修改本次响应的 timing 和状态；
- 并发和 TTL 行为必须有测试。

### 6.4 浏览器池

- 保持固定最大并发，避免过早引入复杂的动态扩容；
- 页面连续失败、达到使用次数或生命周期上限后退休并重建；
- 关闭服务时停止所有后台 goroutine 并清理 Chrome；
- 移除没有接入实际 Rod page pool 的旧自适应池代码和无效配置；
- 移除已被 engine package 替代的旧 HTTP fetcher。

对应提交：

```text
fix(scrape): honor options across every fetch engine
fix(cache): key and clone complete scrape responses
refactor(scrape): retire unhealthy browser pages
refactor(scrape): remove unused fetch and pool paths
```

## 7. 阶段四：Batch、Crawl、Map 和 Extract

### 7.1 Batch

- 复用 `scrape.Service`；
- 保留输入 URL 顺序；
- 使用全局有界 worker pool，避免每个请求各自创建大量 goroutine；
- 单个 URL 失败不丢弃其他成功结果；
- job 状态和 results 使用锁保护；
- webhook 使用任务完成时的一致快照。

### 7.2 Crawl

- 使用线程安全的内存 job manager；
- 任务容量有上限，完成任务按 TTL 回收；
- 服务重启后任务不恢复，并在文档中明确；
- 服务关闭时取消仍在运行的任务；
- 精确执行 `max_pages` 和 `max_depth`；
- URL 去 fragment、规范默认端口并保留 query；
- 使用公共后缀规则判断 base domain；
- 结果去重并保持稳定顺序。

### 7.3 Map

- sitemap index 递归深度、文件数量、单文件大小和总 URL 数均有上限；
- robots.txt 中的 sitemap 地址正确解析；
- sitemap、robots 和首页链接复用统一 HTTP 配置；
- 结果规范化、去重并排序；
- 部分来源失败时仍返回其他来源发现的 URL，并提供 warning。

### 7.4 Extract

- `/extract` 复用 `scrape.Service`；
- 使用假 OpenAI-compatible server 测试成功、认证失败、限流、无 choices 和非法 JSON；
- 默认测试不得调用真实 LLM 或读取真实 API key。

对应提交：

```text
refactor(batch): use shared scrape service and job manager
refactor(crawl): add bounded race-free crawl execution
fix(map): bound and normalize url discovery
refactor(extract): reuse canonical scrape service
```

## 8. 阶段五：SSE、MCP 和文档

- SSE 调用同一个 scrape service，并输出结构化 started、attempt、navigated、completed 和 error 事件；
- JSON 与 SSE 的最终响应、错误代码和质量信息一致；
- 五个 MCP 工具不再维护独立业务逻辑；
- Batch/Crawl 的 scrape options 与单页模型共享定义，JSON 保持兼容；
- README 补齐所有实际生效的环境变量、参数、错误和限制；
- benchmark 输出质量分、引擎路径、成功率和延迟分位数。

对应提交：

```text
refactor(sse): stream canonical scrape service events
refactor(mcp): align tools with canonical api behavior
docs(scrape): document quality fallbacks and limits
```

## 9. 验收标准

### 9.1 自动化测试

- `go test -race -count=1 ./...` 通过；
- `go vet ./...` 通过；
- `go build ./...` 通过；
- 浏览器 fixture 回归全部通过；
- 不允许出现 `success: true` 且正文为空；
- timeout 实际结束时间不得明显超过请求 timeout；
- 10 并发、累计 100 次请求无数据竞争和页面泄漏；
- 请求完成后 active page 回到 0。

### 9.2 真实网页矩阵

维护 25 个公开站点的固定验收矩阵，覆盖五类页面，每个站点记录：

- URL 和分类；
- 预期 title 关键词；
- 预期正文关键词；
- 最小合理内容范围；
- 最大允许延迟。

通过标准：

- 至少 23/25 个站点正文可用；
- 静态页面 p95 不超过 3 秒；
- 浏览器页面 p95 不超过 10 秒；
- 失败结果必须给出明确错误或 warning，不能假成功。

公网矩阵作为 release/nightly 验收，不放入每次 PR 的确定性测试。

## 10. 托管上线

代码和测试全部通过后：

1. 本地构建并运行 Docker 镜像；
2. 检查 Chrome 启动、健康检查和 graceful shutdown；
3. 部署 release candidate；
4. 修复 `purify.verifly.pro` 当前上游 502；
5. 验证 `/health` 在 2 秒内返回 200；
6. 使用认证请求验证静态页、Go 文档、GitHub、新闻页和公开 SPA；
7. 验收通过后发布 `v0.2.0`。

## 11. 双电脑协作方式

分支安排：

- 集成分支：`codex/scrape-v1`；
- 电脑 A：`codex/scrape-runtime`；
- 电脑 B：`codex/scrape-quality`。

第一轮：

- 电脑 A：浏览器等待、engine dispatcher、参数一致性和 scrape service；
- 电脑 B：fixture server、cleaner、质量评分和 25 站 benchmark。

第二轮：

- 电脑 A：Batch、Crawl 和 job manager；
- 电脑 B：Map、MCP、SSE 和文档。

协作规则：

- 两台电脑不同时直接提交到集成分支；
- 每笔提交只解决一个可验证问题；
- 各自分支通过测试后再合入 `codex/scrape-v1`；
- 每次合并前执行 race、vet、build 和 diff check；
- 通用抓取全部验收前，不开始 Search 功能代码。

## 12. 完成定义

只有同时满足以下条件，通用抓取阶段才算完成：

- 单页、Batch、Crawl、Map、Extract、SSE 和 MCP 共享同一抓取主链路；
- 所有已公开的抓取参数实际生效；
- 不再出现空正文假成功；
- 25 站验收达到 90%；
- 并发、缓存和后台任务通过 race 测试；
- Hosted API 恢复并通过认证烟雾测试；
- README、API 示例与实际行为一致；
- `v0.2.0` 发布完成。

完成后，Search API 从该稳定版本建立新分支继续开发。
