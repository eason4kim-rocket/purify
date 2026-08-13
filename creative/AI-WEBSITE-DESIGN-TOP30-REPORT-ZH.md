# AI 公司官网与高评分设计网站 Top 30 研究报告

> 调研快照：2026-08-09<br />
> 研究对象：15 个 AI / AI 基础设施官网 + 15 个可核实获奖或高评分网站<br />
> 研究目的：识别高水平网站在品牌、首屏、信息架构、产品证明、动效、转化、信任、性能和无障碍方面的共同规律，并转化为 Purify 可执行的官网方案。

## 阅读导航

- **先做决策：** 第 0、7、8、9、17、18、19 节；
- **看 30 站逐一拆解：** 第 2、3、4 节；
- **找设计规律与避坑：** 第 5、6 节；
- **进入页面与组件设计：** 第 10、11、12、13 节；
- **进入上线标准：** 第 14、15、16 节；
- **复核依据：** 第 20 节。

## 0. 先读结论

本次研究最重要的结论不是“现在流行什么颜色或动效”，而是以下八点：

1. **最强 AI 官网正在变成产品入口。** OpenAI、Perplexity、Lovable 直接把输入框放在 Hero；Cursor、ElevenLabs 用可操作演示证明产品，而不是让用户先读六屏营销文案。
2. **高分站往往只使用一套强视觉语法。** Mistral 的像素模块、Dropbox Brand 的网格与色块、Midjourney 的 CRT 字符、Anthropic 的暖色编辑出版感，都能从任意局部被识别。
3. **商业站与实验站不能用同一把尺子。** AI 官网首先要解释价值、建立信任并促成使用；获奖站常以情绪、探索和技术创新为第一目标。因此本报告将两类分别排名，不制造一个虚假的“全球绝对 Top 30”。
4. **产品证明已经替代通用 3D。** 高水平网站展示真实工作流、结果、声音、代码、数据和界面。没有真实功能连接的液态球、星云和玻璃卡片，正在快速失去说服力。
5. **B2B 信任证据前移到前 1–1.5 屏。** 客户、使用规模、安全、部署方式、方法与基准不再只放页脚。
6. **好动效解释状态，坏动效只制造等待。** Cursor 的任务状态、ElevenLabs 的播放状态、Exa/Tavily 的查询过程、Mistral 的模块组合都在解释产品；无限跑马灯、滚动劫持和无暂停视频则是高风险模式。
7. **高级感来自取舍，不来自素材数量。** 大留白、精准字阶、真实内容、清晰节奏和一致组件，往往比同时使用 3D、粒子、视频、玻璃和渐变更高级。
8. **Purify 当前不缺视觉辨识度，缺产品真相、可运行演示和转化闭环。** 最优方案是保留现有 Evidence Realism 视觉资产，把首页从“愿景影片优先”改成“真实 URL Demo、输出、基准和快速开始优先”。

---

## 1. 研究口径与限制

### 1.1 样本构成

本报告使用两个样本池：

- **A 组：15 个 AI / AI 基础设施网站。** 依据行业代表性、设计完成度、产品类型差异和对 Purify 的参考价值筛选；由本次调研按统一量表评分。
- **B 组：15 个高评分设计网站。** 优先采用 CSS Design Awards 2025 WOTY、Awwwards SOTD/SOTM、FWA 等可核实记录；外部奖项分数保留原口径。

这种拆分很重要。A 组的分数是本次研究的编辑审计分；B 组的外部分数或奖项来自主办方。两者不能直接相加，也不能被理解为全球网站的客观总排名。

### 1.2 AI 官网评分模型（100 分）

| 维度 | 权重 | 具体判断 |
|---|---:|---|
| 首屏定位与信息清晰度 | 15 | 5–10 秒内能否说明产品、对象和结果 |
| 品牌与视觉辨识度 | 15 | 色彩、字体、图形、影像、布局是否形成独特系统 |
| 产品证明能力 | 15 | 是否展示真实输入、过程、结果、界面或可用 Demo |
| 导航与信息架构 | 10 | 顶层分类、产品线和用户路径是否清楚 |
| CTA 与转化 | 10 | 主动作是否明确，自助与企业路径是否合理 |
| 信任证据 | 10 | 客户、数据、案例、研究、安全、部署、方法 |
| 动效与交互 | 10 | 动效是否解释状态、提供反馈并服务理解 |
| 响应式、无障碍、性能 | 15 | 语义、键盘、替代内容、媒体控制与负载风险 |

### 1.3 核验方式

- 用用户指定的电脑插件在真实桌面浏览器中核验了 Anthropic、Runway、Exa、Tavily、Dropbox Brand 等代表站的首屏与滚动状态。
- 逐站读取官网当前结构和内容，辅以官方品牌页、Trust Center、客户页和研究页。
- 奖项依据优先来自 [CSS Design Awards 2025 WOTY 结果](https://www.cssdesignawards.com/blog/2025-website-of-the-year-winners/430/)、[CSSDA 评审机制](https://www.cssdesignawards.com/about)、Awwwards 与 FWA 官方记录。
- 性能观察是定性风险评估，不是对 30 个网站统一环境下的 Lighthouse 实验室跑分。不能把“媒体多”直接等同于“性能差”。

### 1.4 质量底线

本报告建议以 [WCAG 2.2](https://www.w3.org/TR/WCAG22/) AA 为无障碍目标。性能以当前 Core Web Vitals 为外部验收线：移动和桌面分别看 p75，LCP ≤ 2.5s、INP ≤ 200ms、CLS ≤ 0.1，依据 [web.dev 官方说明](https://web.dev/articles/vitals)。

---

## 2. A 组：AI / AI 基础设施官网 Top 15

### 2.1 排名总表

| 排名 | 网站 | 定位 | 品牌 | 产品证明 | IA | 转化 | 信任 | 交互 | A11y/性能 | 总分 |
|---:|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | [Cursor](https://cursor.com/) | 15 | 14 | 15 | 8 | 10 | 10 | 10 | 10 | **92** |
| 2 | [OpenAI](https://openai.com/) | 14 | 14 | 15 | 9 | 10 | 9 | 9 | 11 | **91** |
| 3 | [ElevenLabs](https://elevenlabs.io/) | 14 | 14 | 15 | 9 | 9 | 9 | 9 | 11 | **90** |
| 4 | [Mistral](https://mistral.ai/) | 14 | 15 | 12 | 9 | 9 | 10 | 9 | 11 | **89** |
| 4 | [Vercel AI](https://vercel.com/ai) | 14 | 15 | 13 | 9 | 8 | 9 | 9 | 12 | **89** |
| 4 | [Runway](https://runway.com/) | 14 | 14 | 15 | 9 | 10 | 9 | 10 | 8 | **89** |
| 7 | [Hugging Face](https://huggingface.co/) | 13 | 12 | 15 | 10 | 8 | 10 | 7 | 11 | **86** |
| 8 | [Harvey](https://www.harvey.ai/) | 14 | 15 | 10 | 9 | 8 | 10 | 9 | 10 | **85** |
| 8 | [Cohere](https://cohere.com/) | 15 | 13 | 12 | 9 | 9 | 10 | 7 | 10 | **85** |
| 10 | [Lovable](https://lovable.dev/) | 15 | 13 | 15 | 8 | 10 | 6 | 8 | 9 | **84** |
| 11 | [Anthropic](https://www.anthropic.com/) | 13 | 15 | 8 | 9 | 7 | 10 | 8 | 12 | **82** |
| 11 | [Perplexity](https://www.perplexity.ai/) | 15 | 10 | 15 | 9 | 8 | 7 | 8 | 10 | **82** |
| 13 | [Scale AI](https://scale.com/) | 14 | 13 | 10 | 8 | 8 | 10 | 9 | 9 | **81** |
| 14 | [Midjourney](https://www.midjourney.com/home) | 11 | 15 | 11 | 7 | 9 | 7 | 10 | 8 | **78** |
| 15 | [Character.AI](https://character.ai/) | 13 | 13 | 9 | 6 | 10 | 4 | 8 | 8 | **71** |

> 分数不是流量、融资、模型能力或公司价值排名，只评价当前官网如何传达、证明并转化其产品与品牌。

### 2.2 维度冠军

- **视觉辨识度最强：** Mistral、Harvey、Midjourney、Vercel、Anthropic。
- **产品证明最强：** Cursor、OpenAI、ElevenLabs、Lovable、Runway、Perplexity。
- **企业信任最强：** Harvey、Cohere、Scale、Mistral、Anthropic。
- **搜索/问答首屏最值得借鉴：** OpenAI、Perplexity；对 Purify 更直接的竞品对照另见 Exa 与 Tavily专题。

---

### 2.3 逐站分析

#### 1. Cursor — 产品过程就是主视觉

来源：[官网](https://cursor.com/) · [安全](https://cursor.com/security)

- **定位与首屏：** “Cursor is your coding agent for building ambitious software.” 首屏没有先解释模型和技术，而是直接展示 Desktop、CLI、任务队列、计划、代码 diff、测试与预览。
- **视觉系统：** 暖中性色页面承接深色产品 UI；现代无衬线标题与代码等宽字体自然分工。品牌自身很克制，把注意力让给真实工作过程。
- **信息架构：** 顶部只保留 Sign in 与 Download；复杂能力在长页中逐步展开为 Agents、Teams、Enterprise、Code Review、CLI、Cloud Agents 等。登录态访问根域会直接进入产品，这体现“官网即应用”的策略。
- **动效与证明：** 动效模拟 Agent 读取文档、生成计划、改代码、运行测试和完成任务。每个运动都在解释状态，而不是装饰。
- **转化与信任：** Get started / Download 清楚；配合企业使用、行业人物证言、安全与 SOC 2 等证据。
- **风险：** 完整 IDE 演示资源重、信息密，移动端不能等比缩小。应改造成分步场景卡。登录态直接进应用，也会让用户不易返回品牌页。
- **可借鉴：** Purify 应展示 `started → navigated → cleaned → completed` 的真实 SSE 过程、原始页面与清洗结果差异、Token 变化和引擎选择，而不是只放一张静态结果截图。

#### 2. OpenAI — 输入框取代营销 Hero

来源：[官网](https://openai.com/)

- **定位与首屏：** 当前首页中心是“有什么可以帮忙的？”和可操作的 ChatGPT 输入框；ChatGPT Business、研究、API 等意图胶囊完成分流。
- **视觉系统：** 黑白高对比与大面积留白构成骨架，色彩来自产品、研究、人物与事件的编辑型封面，而非通用 AI 渐变。
- **信息架构：** Research / Products / Business / Developers / Company 五大域足以覆盖庞大生态；后续模块像高端媒体首页，依次承载发布、新闻、故事、研究和企业案例。
- **动效与证明：** Prompt 建议轮换，部分产品卡使用视频且提供暂停。首页、产品体验与内容门户被合并。
- **转化与信任：** 输入本身是最强 CTA；Try ChatGPT 是显式第二入口。安全、透明度、研究、客户故事和持续发布构成完整信任路径。
- **风险：** 公司使命和能力解释被推迟，首次访问者可能只把它理解为 ChatGPT 页面；长页也容易变成新闻流。
- **可借鉴：** Purify Hero 应允许用户输入 URL，下面只放 3–4 个示例网址或任务；先让用户看到输出，再解释架构。

#### 3. ElevenLabs — 抽象品牌图形必须可以被“听见”

来源：[官网](https://elevenlabs.io/) · [安全](https://elevenlabs.io/safety)

- **定位与首屏：** “Bringing technology to life”，紧接 ElevenCreative / ElevenAgents / ElevenAPI 三条路径。
- **视觉系统：** 白、浅灰、黑为骨架，光谱色球体象征声源、角色和对话；大标题与短说明采用干净的两栏结构。
- **产品证明：** 发光球体不是纯装饰，用户可以播放不同声音类别。编辑器、Agent 控制台和 API 能力随后继续展示。
- **信息架构：** Products / Solutions / Customers / Resources / Enterprise / Pricing 清楚；复杂产品线用“三个平台共享同一研究底座”统一。
- **转化与信任：** Sign up 与 Contact sales 并列；Disney、Twilio、Cisco、NVIDIA、Meta 等客户和 Safety、Moderation、Provenance 提前降低顾虑。
- **风险：** 音频、视频、轮播和长页若同时初始化，会迅速推高负载；声音不能成为唯一信息通道。
- **可借鉴：** Purify 的抽象“证据粒子”必须与真实状态或输出相连。例如每个粒子代表来源、清洗规则或引用；点击后应改变可见结果。

#### 4. Mistral — 一种几何单元撑起整个品牌系统

来源：[官网](https://mistral.ai/) · [品牌页](https://mistral.ai/brand/)

- **定位与首屏：** “Frontier AI. In your hands.” 左侧超大标题，右侧用一句话说明为组织构建定制 AI；橙红像素模块和 Featured News 紧随其后。
- **视觉系统：** 白、米、朱红、橙；像素 M、可见网格、矩形模块贯彻 logo、背景、卡片、图标和动效，形成最完整的识别系统之一。
- **信息架构：** Products / Solutions / Research / Developers / Blog / Customers / Company，配深层 mega menu；Studio、Forge、Vibe、Compute 分工清楚。
- **动效与证明：** 像素块组合、新闻滑块和案例切换都围绕“模块化系统”展开。
- **转化与信任：** Start building 与 Contact sales 分别服务开发者和企业；客户、自托管、EU hosting、行业与云伙伴证据充分。
- **风险：** 七项顶层导航和大型 mega menu 对首次访问者偏重；辅助树可能重复大量链接；移动端需要重构而非压缩。
- **可借鉴：** Purify 应选一个核心图形单元，例如“来源片段 / 干净文本块”，让它同时承担 Wordmark、加载、指标、引用、卡片和转场。

#### 5. Vercel AI — 用简单隐喻解释抽象基础设施

来源：[官网](https://vercel.com/ai) · [安全](https://vercel.com/security)

- **定位与首屏：** 黑色细网格上浮着对话气泡：“Infrastructure for AI.” 与“Ship intelligent, secure applications…”；气泡代表 AI，网格代表基础设施。
- **视觉系统：** 黑、白、灰、三角 logo、细网格和 Geist 风格字体高度系统化；技术命令与大标题并存。
- **产品证明：** 后续用模型厂商轨道、Gateway、Compute、Sandbox、终端与设备连接图解释复杂架构。
- **转化与信任：** Get a Demo、Sign up、Docs 和 `npm i ai` 形成企业与开发者路径；客户和安全证据充足。
- **动效与性能：** 气泡、网格和轨道图主要可由 CSS/SVG 实现，比全屏视频更容易控制负载。
- **风险：** 非开发者不一定理解 Gateway、Fluid Compute 与 Sandbox；活动条可能和 Hero CTA 争抢注意力。
- **可借鉴：** Purify 可以让“网页片段在清洗管道中流动”，用一个轻量网格或路径图解释 HTTP、Browser、Clean、Deliver，而不是再加一支抽象影片。

#### 6. Runway — 用作品质量证明模型质量

来源：[官网](https://runway.com/) · [研究](https://runway.com/research)

- **定位与首屏：** 从“AI 视频工具”升级为 Real-World Intelligence，首屏声明 “Building Real-World Intelligence”，覆盖 Creative、Dev、Robotics 三个平台。
- **视觉系统：** 黑白排版框架承接电影级全幅视频，色彩几乎全部来自生成画面。大号现代无衬线标题只占必要空间。
- **产品证明：** 高品质视频、真实世界模拟和创意成片直接展示模型能力；三平台共享同一底层智能的叙事避免产品列表碎片化。
- **转化：** Try Runway、Get API Key、Contact Sales 分别对应创作者、开发者、企业。
- **信任：** 60m+ creatives、行业合作、研究项目和模型发布共同证明。
- **风险：** 重视频是最大性能成本；宏大“现实世界智能”叙事也可能稀释用户原有的“最好用视频生成工具”认知。
- **可借鉴：** Purify 若有真实抓取前后对比，就应该让结果本身成为画面；愿景影片只能放在真实产品证据之后。

#### 7. Hugging Face — 社区数据本身就是社会证明

来源：[官网](https://huggingface.co/)

- **定位与首屏：** “The AI community building the future.” 未登录首页直接展示模型、Spaces、数据集与趋势；登录后则进入社区工作台。
- **视觉系统：** GitHub 式实用主义 UI，黄色 emoji logo、蓝色链接、高密度卡片和少量渐变；内容就是视觉。
- **信息架构：** 全局搜索 + Models / Datasets / Spaces / Buckets / Docs / Enterprise / Pricing，技术用户能够立即定位资源。
- **产品证明与信任：** 真实模型量、下载量、组织页、应用和社区活动比静态 logo wall 更强。
- **转化：** 未登录时引导 Explore、Browse、Sign up；登录后引导创建、关注和贡献。
- **风险：** 对非技术决策者门槛高；三栏高密度布局在移动端必须强折叠；登录态会让品牌叙事几乎消失。
- **可借鉴：** Purify 可公开展示真实 benchmark 数据集、常用抓取示例、规则库、版本与 GitHub 活动，让生态本身承担信任。

#### 8. Harvey — 进入行业自己的美学

来源：[官网](https://www.harvey.ai/) · [安全](https://www.harvey.ai/en-US/security)

- **定位与首屏：** “Practice Made Perfect”，以成熟专业人士、会议与文件的电影化场景表达法律与专业服务，而不是套用机器人、法槌或蓝紫科技模板。
- **视觉系统：** 近黑、深绿、暖棕、白；高对比 editorial serif 建立权威，sans 负责产品和导航，接近顶级律所与精品出版物。
- **信息架构：** Platform / Solutions / Customers / Security / Resources / Company；平台按 Agents、Vault、Knowledge、Shared Spaces、Contract Intelligence 等真实任务拆分。
- **转化与信任：** Request a Demo 为唯一主动作，适合高客单价采购；大型客户、证言、采用数据、SOC 2 II、ISO 27001、GDPR 等证据密度很高。
- **动效：** 电影、logo 流、证言轮播和客户案例维持高端气质。
- **风险：** “Perfect”承诺偏强；重视频与保守人物画面可能同时带来性能和包容性风险。
- **可借鉴：** 产品面向特定行业时，先采用该行业的文化语法，再加入 AI。若 Purify 服务研究、法律或金融，应使用真实研究现场、引用和证据，而不是泛化科技图。

#### 9. Cohere — 一句话完成企业差异化

来源：[官网](https://cohere.com/)

- **定位与首屏：** “Own your AI. Your data. Your infrastructure. Cohere keeps it that way.” 用极短文案把私有、安全、可部署和数据归属全部说清。
- **视觉系统：** 超大轻字重 sans、大量白色空间，小面积品牌色；产品 UI 与真实人物摄影不对称组合，平衡技术与业务现场。
- **信息架构：** Products / Solutions / Research / Resources / Company；正文顺序是客户、Security/Deployment/Customization、产品、行业、模型、开发者和案例。
- **转化：** Contact us 为主，Explore products 为次；企业导向明确，部分产品提供 instant demo。
- **信任：** 客户、VPC、on-prem、Model Vault、认证和行业说明都很早出现。
- **风险：** 对小团队而言 Contact us 门槛偏高；视觉成熟但比 Mistral/Harvey 更难形成独占记忆。
- **可借鉴：** Purify 可以把“Hosted or self-hosted”“No Redis or Postgres required”“Your URLs and content stay under your control”等所有权语言放在首屏附近。

#### 10. Lovable — 首页入口就是产品入口

来源：[官网](https://lovable.dev/) · [Trust Center](https://trust.lovable.dev/)

- **定位与首屏：** 高饱和渐变画布中央是 “Build something Lovable”，下方是真实可输入的构建框。Dogfooding 非常彻底。
- **视觉系统：** 蓝—紫—粉—红渐变营造年轻情绪，黑色 Prompt Box 成为唯一功能焦点，粗体 sans 保持直接。
- **产品证明：** Start with an idea → Watch it come to life → Refine and ship 三段静音演示与模板库解释完整路径。
- **转化：** 输入框和 Get started 双重驱动，摩擦很低。
- **信任：** 项目量、周创建量、访问量和 Trust Center 提供规模证明，但客户案例与具体安全证据不如企业站充足。
- **风险：** 渐变容易被复制；动态数字在 JS 未完成时可能显示为 0；视觉 Hero 与真正 H1 的语义次序需一致。
- **可借鉴：** Purify 可以直接把 URL 输入做 Hero，但背景色必须来自自己的 Evidence Realism，而不是照抄流行渐变。

#### 11. Anthropic — 把安全做成气质，而不是页脚声明

来源：[官网](https://www.anthropic.com/) · [Trust Center](https://trust.anthropic.com/)

- **定位与首屏：** “AI research and products that put safety at the frontier”；暖米色大画布、极大留白、左右非对称宣言和大型黑色内容块，像文化机构或学术出版物。
- **视觉系统：** 暖象牙白、黑、低饱和自然色；editorial serif 与极简 sans 组合，完全避开通用“未来科技感”。
- **信息架构：** Research / Policy / Commitments / Learn / News，Claude 产品通过独立入口分层。机构使命与产品营销被有意分开。
- **转化与信任：** Try Claude 明确但不抢占中心；公共利益公司、Responsible Scaling Policy、Claude’s Constitution、经济研究与透明度构成系统证据。
- **无障碍：** 同时提供 Skip to main content 与 Skip to footer，语义结构清楚。
- **风险：** 产品能力和即时价值证明偏弱，首次访问者的转化路径比 OpenAI 慢。
- **可借鉴：** Purify 现有米白、衬线和真实影像方向是有价值的；问题不在视觉，而在产品内容没有被放进这套气质中。

#### 12. Perplexity — 在输入框内部完成能力教育

来源：[官网](https://www.perplexity.ai/)

- **定位与首屏：** 首页就是应用；输入框内提供 Search、Computer、Model、语音等模式，下方用任务卡解释“搜索”和“完成工作”。
- **视觉系统：** 深炭黑、青绿色功能高亮、标准 UI sans，几乎不使用营销影像。
- **信息架构：** 左侧栏把 New、Computer、Artifacts、Projects、Sessions 等纳入持续工作台，而不是一次性答案页。
- **产品证明：** 模式开关、输入、来源、历史和任务产物都是真实产品组件。
- **转化：** Prompt 是主 CTA；登录与注册方式完整，但弹窗出现较早。
- **信任风险：** 首页缺少客户、方法、评估和安全证据；对新用户而言，“为什么比传统搜索好”仍需自己体会。
- **可借鉴：** Purify 可在 URL 输入框附近直接提供 `Scrape / Extract / Crawl / Map` 四个模式，替代八张解释卡。

#### 13. Scale AI — 用高后果场景表达可靠性

来源：[官网](https://scale.com/) · [品牌指南](https://brand.scale.com/)

- **定位与首屏：** “The world’s most important decisions need reliable AI systems.” 叠加飞行员等高后果场景，视觉像国防、航空与基础设施公司。
- **视觉系统：** 白色导航、黑色公告条、深蓝灰电影画面、大号白字；真实场景承担色彩与情绪。
- **信息架构：** Products / Solutions / Research / Resources，正文从 Applications、Data、行业、客户到 Labs。
- **信任：** 模型公司、Meta、Mayo Clinic、公共部门、Physical Intelligence、BP 等形成强力证据。
- **动效：** Hero 视频、行业词与场景切换、客户卡流动。
- **风险：** 单一国防画面可能过度定义品牌；重复 carousel DOM、弱 alt 与重视频会损害无障碍和性能。
- **可借鉴：** 如果 Purify 强调可靠，应展示真实失败升级、状态码、引用和重试证据，而不是用一个锁头图标代替可靠性。

#### 14. Midjourney — 单一世界观带来极强记忆

来源：[官网](https://www.midjourney.com/home)

- **定位与首屏：** 深蓝 CRT / 扫描线画面，以生成式 ASCII 字符构成品牌；Sign Up、Log In、Explore 以悬浮胶囊集中。
- **视觉系统：** 海军蓝、黑、电蓝、紫红，monospace、噪点、扫描线与器官像素图标构成神秘研究终端。
- **信息架构：** 极浅，只保留产品入口、文档、About、Projects、Careers、Contact。
- **动效：** 字符重组、扫描线、闪烁和浮动按钮本身就是体验核心。
- **转化：** Explore 允许未注册用户先看结果，Sign Up / Log In 直接。
- **风险：** 企业信任证据不足；字符画在移动端可读性弱；动画与内存成本高，需要 reduced-motion 降级。
- **可借鉴：** 不要复制 CRT，而要学习“生成规则一致”。Purify 的规则应是“噪音被逐步剥离，证据和引用保持可见”。

#### 15. Character.AI — 情绪承诺优先的消费站

来源：[官网](https://character.ai/) · [安全中心](https://character.ai/safety)

- **定位与首屏：** 深色背景、电影化动漫世界与注册卡，主张访问大量角色。首页首先卖“进入另一个世界”的情绪，而不是技术。
- **视觉系统：** 黑灰 UI 对比高情绪生成图，大数字承担说服。
- **信息架构与转化：** 顶部几乎只保留 Register / Login；首屏所有空间都服务注册。
- **信任：** 角色规模是主要证据，但安全、家长控制、隐私和内容质量没有进入首屏附近。
- **风险：** 安全与信任明显落后于转化；大型生成图加载和版权联想也需管理。
- **可借鉴：** 消费产品可表达使用后的情绪，但对 Purify 这种开发者基础设施而言，情绪必须在真实结果之后，而不能替代功能解释。

---

## 3. 直接竞品专题：Exa 与 Tavily

这两站不计入上面的 15 个“行业代表排名”，但对 Purify 的产品与首页决策最直接。

### 3.1 Exa — 品牌、产品 Demo 与基准三者结合最好

来源：[官网](https://exa.ai/)

- Hero 是 “Web search, built for AI agents”，一句副文案说明 search、crawling、research agents；主 CTA 是 Try API for free。
- 电脑实站核验显示：白底、超大黑色 serif、蓝绿水体式艺术图、蓝色网格小方块和黑色 CTA 构成强辨识度。
- Hero 直接嵌入真实 Search Agent：查询、effort、object/text 输出与结果表格都可理解；这比一张 Dashboard 截图更有效。
- 第二屏用艺术图 + “All the papers your research agent is looking for” + Cursor 客户说明完成场景化证明。
- 后续继续展示 token-efficient highlights、structured outputs、准确率/延迟 benchmark、客户、ZDR、SOC 2、研究规模和 Trust Center。
- **最值得借：** “艺术品牌层 + 可操作 Demo + 可验证工程证据”的三层结构。Purify 现有视觉可以承担第一层，但第二、第三层缺失。

### 3.2 Tavily — 商业信息架构更标准，工程证据更前置

来源：[官网](https://www.tavily.com/)

- Hero 是 “Connect your AI agents to the web”，副文案强调 one secure API，配 search / extract / crawl / research 模式输入框。
- 电脑实站核验显示：米白背景、黑色粗体 sans、薄荷绿高亮、柔和绿色地平线渐变和半透明输入面板，整体比 Exa 更传统、更企业。
- 客户与“2M+ developers”紧跟首屏；三条能力分别说明 fresh context、规模与 safeguards。
- Benchmark 明确展示 SimpleQA 等项目，并链接方法与 GitHub；同时展示月请求、SLA、p50 和页面规模。
- **问题：** Cookie Banner 占据首屏底部较大面积；视觉差异度低于 Exa；强调很多指标时需确保方法、日期和复现数据同步存在。
- **最值得借：** 在首屏直接切换 Scrape / Extract / Crawl / Map，以及把 benchmark 方法链接放在数字旁边。

### 3.3 Purify 与直接竞品的定位空位

| 维度 | Exa | Tavily | Purify 可占据的位置 |
|---|---|---|---|
| 核心类别 | Search / index / agent research | Secure web access / search | Clean web context / scraping / self-host |
| 主要证明 | Agent Demo、基准、客户、研究规模 | 多模式输入、基准、SLA、企业安全 | 抓取前后差异、Token 节省、多引擎升级、MCP、自托管 |
| 品牌语言 | 艺术、serif、蓝绿水体与网格 | 米白、薄荷绿、企业 SaaS | Evidence Realism：纸张、深海、矿物蓝、真实证据 |
| 关键 CTA | Try API for free | Try it for free | Try a URL / Copy curl / Self-host |
| 最大机会 | 不必复制独立搜索索引叙事 | 不必复制通用“web access layer” | “把任意网页变成可引用、低 Token、可自托管的 Agent 上下文” |

---

## 4. B 组：高评分 / 获奖 / 权威策展设计网站 15 例

### 4.1 为什么不叫“客观全球前 15”

设计奖项和策展平台的机制不同：

- **Awwwards：** SOTD 公开评分按 Design 40%、Usability 30%、Creativity 20%、Content 10% 加权；Developer Award 另看语义/SEO、动画、无障碍、WPO、响应式和 Markup。[2025 年度赢家](https://www.awwwards.com/annual-awards/winners)
- **CSS Design Awards：** WOTD 通常要求评委均分超过 8.0；Public Awards 另由公众票与评委分共同决定，分 UI、UX、Innovation。[官方机制](https://www.cssdesignawards.com/about)
- **FWA：** 依据专家 Live Judging 产生 FOTD，再进入月度、年度与 People's Choice；本报告仅用于趋势交叉验证，没有把无法稳定核验的项目硬塞进样本。
- **SiteInspire、Lapa Ninja、Recent/Godly：** 属编辑策展，不是统一评分榜。被收录只能证明编辑选择，不能写成“评委 9 分”。

以下 15 站按“证据强度、模式差异、行业代表性、可迁移价值”排列。分数沿用各平台原口径，不做跨平台换算。

### 4.2 样本总表

| # | 网站 | 类型 | 主要证据 | 最突出的设计模式 |
|---:|---|---|---|---|
| 1 | [Lando Norris](https://landonorris.com/) | 车手品牌 | Awwwards 2025 SOTY + Users’ Choice；SOTD 8.18 | 单一品牌色 + 中心人物 + 实时赛程 |
| 2 | [Igloo Inc](https://www.igloo.inc/) | 创意工作室 | Awwwards 2024 SOTY / DEV SOTY；Webby 2026 Winner | 品牌名变成可进入的 3D 世界 |
| 3 | [Yamê – The Molazone](https://mola-zone.com/) | 音乐体验 | Awwwards SOTD 7.64 | 专辑封面变成 3D 世界地图 |
| 4 | [Getty – Tracing Art](https://www.getty.edu/) | 文化 / 数据故事 | Awwwards SOTD 7.68 | 数据关系先变成视觉隐喻 |
| 5 | [The ADHD Experience](https://www.adhdexperience.com/) | 公益体验 | Awwwards SOTD 7.47 | 用界面行为模拟主题 |
| 6 | [Digital Design Days X](https://palermo.ddd.live/) | 活动 | Awwwards SOTD 7.34 | 激进 Shader + 传统活动 IA |
| 7 | [Son Daven](https://sondaven.com/en) | 高端地产 | CSSDA WOTD；Final Judge 8.50 | 文化神话 → 投资证据的长叙事 |
| 8 | Zorge 9 | 高端地产概念 | CSSDA WOTD；Final Judge 8.50 | 人物身份与建筑拼贴 |
| 9 | [Aardvark Book Club](https://www.aardvarkbookclub.com/) | 订阅电商 | CSSDA WOTD；Final Judge 8.04 | 实体书作为首屏演员 |
| 10 | [Motiondeep](https://www.motiondeep.com/) | 动效工作室 | CSSDA WOTD；Final Judge 8.25 | 官网本身就是可玩 Demo |
| 11 | Noho Redesign Concept | 非商业概念 | CSSDA WOTD；Final Judge 8.22 | 一句原则 + 使用姿态证明 |
| 12 | [Huts](https://huts.com/) | 建筑 / 服务 | SiteInspire 编辑收录 | 无图片 Hero 的排版权威感 |
| 13 | [Squarespace Foundations](https://brand.squarespace.com/) | 互动品牌手册 | SiteInspire 编辑收录 | 章节式品牌博物馆 |
| 14 | [Portal CG](https://www.portal.graphics/) | CG 工作室 | Lapa Ninja 编辑收录 | 作品气质优先，业务证据随后 |
| 15 | [Azione](https://azionepr.com/) | 创意公关 | Lapa Ninja 编辑收录 | 影像与客户证据带融合 |

> 另外，[Dropbox Brand Guidelines](https://www.dropboxbrand.com/) 是 CSSDA 2025 WOTY 9.03、Best UX，并获 2025 Webby Best UX 与移动响应式设计；它作为本报告的“获奖方法标杆”在后文单独拆解，而不占这 15 个差异化样本名额。

### 4.3 逐站分析

#### 16. Lando Norris — 一个色彩、一个人物、一个实时数据

证据：[Awwwards SOTD 8.18](https://www.awwwards.com/sites/lando-norris) · [年度赢家](https://www.awwwards.com/annual-awards/winners)

- **首屏：** 整屏荧光黄绿加载门后，进入暖白底、巨幅车手肖像和极淡赛道线稿；姓名字标、Store 胶囊和 Next Race 卡分布在边缘。
- **视觉系统：** `#D2FF00 + #111112` 的强约束配色把 F1 性能感与时尚杂志感结合；高反差衬线与硬朗 sans 混排。
- **内容与动效：** 赛程、On/Off Track、头盔档案、商店与社交；人物、头盔 3D、转场、视差和手势共同构成个人品牌世界。
- **转化：** Store 是唯一常驻高饱和 CTA；Next Race 提供持续回访理由。
- **风险：** 加载门延迟有效信息，人物与 WebGL 资源昂贵；隐藏菜单和复杂交互可能让键盘、低性能设备用户吃亏。
- **可借鉴：** Hero 不需要五个视觉中心。一个强色、一个中心证据物和一个动态事实，已经足够形成记忆。

#### 17. Igloo Inc — 品牌名直接变成物理世界

证据：[Awwwards SOTD 7.92](https://www.awwwards.com/sites/igloo-inc) · [Webby 2026](https://winners.webbyawards.com/2026/websites-and-mobile-sites/features-design/best-visual-design-aesthetic/361001/igloo-inc)

- **首屏：** 银灰冰原中央只有一个 3D 冰屋，四角是粗字标、极小 Manifesto、Scroll 与 Sound。
- **视觉系统：** `#B6BAC5 / #383E4E` 两色，材质、阴影和地形承担大部分视觉差异；粗短字标配极小等宽 HUD。
- **交互：** 3D 场景、无限滚动、场景转场与声音共同构成沉浸式探索，传统菜单被弱化。
- **风险：** Awwwards Developer 数据非常有启发：Animation 9.60，但 Semantics 6.60、Accessibility 6.60、Markup 6.40。获奖不等于产品质量全优。
- **可借鉴：** 可以把品牌隐喻做成世界，但必须提供语义化内容层、跳过入口、Reduced Motion 与低性能降级。

#### 18. Yamê – The Molazone — 专辑内容被拆成地点与玩法

证据：[Awwwards SOTD 7.64](https://www.awwwards.com/sites/yame-the-molazone)

- **首屏：** 卡通渲染的沙漠峡谷、歌手与摩托车构成中心；顶部是 Play、Tour、Shop、Album，右上为播放状态。
- **视觉系统：** 青绿天空、陶土岩石、沙色地面与墨绿车辆；漫画线描、Blender/Three.js 场景与音乐播放器融合。
- **信息架构：** 地点、角色、音轨、隐藏内容和小游戏构成世界地图式导航。
- **风险：** Creativity 8.02 高于 Usability 7.35，说明世界感强但学习成本也高；音频、3D 和小游戏对移动、键盘与 Reduced Motion 都是挑战。
- **可借鉴：** 当内容天然可被拆成章节、地点或角色时，网站可以成为可探索世界；工具站则不应为了沉浸感隐藏关键操作。

#### 19. Getty – Tracing Art — 先用隐喻解释数据关系

证据：[Awwwards SOTD 7.68](https://www.awwwards.com/sites/tracing-art)

- **首屏：** 模糊艺术品背景中央放置白色圆角展签；“Tracing / Art”之间由连续缩略图形成无穷环。
- **视觉系统：** 白与淡蓝灰让作品颜色成为变量；古典 serif 与极简 UI 同时传达博物馆权威和数字产品克制。
- **产品证明：** Getty Provenance Index 的复杂来源、路线、节点与时间，被转化为故事和可探索关系，而不是直接呈现数据库表格。
- **风险：** 小缩略图、Hover 细节与淡色层级对触屏和低视力用户不够友好。
- **可借鉴：** Purify 的来源、引用和清洗过程也需要一个视觉隐喻；最合理的是“噪音减少、证据链保持”，而非抽象宇宙。

#### 20. The ADHD Experience — 交互机制本身表达主题

证据：[Awwwards SOTD 7.47](https://www.awwwards.com/sites/the-adhd-experience)

- **首屏：** 暗色模糊背景、发光浏览器面板、被重叠和划线的巨大标题、网格、圆点与计时器主动制造注意力干扰。
- **视觉系统：** 黑白骨架搭配强蓝与橙红；字形漂移和破坏阅读顺序是内容的一部分。
- **结构：** Hyperactivity、Disorganization、Impulsivity 等模块把症状转成界面行为。
- **风险：** Usability 6.95、Accessibility 6.60 并不意外。模拟体验可能真实，却不能强迫所有用户承受。
- **可借鉴：** 让交互表达主题是高级做法，但必须同时提供平静模式、纯文本、跳过动画、闪烁警告与 Reduced Motion。

#### 21. Digital Design Days X — 视觉激进，操作仍然保守

证据：[Awwwards SOTD 7.34](https://www.awwwards.com/sites/digital-design-days-x)

- **首屏：** 白色画布中是一张粉紫蓝 WebGL 活动海报；巨大三行标题与 PALERMO 构成瑞士海报网格。
- **视觉系统：** `#F55AC2 / #201A39`、漂移 Shader 和硬朗排版形成强活动能量。
- **信息架构：** Program、Event Hub、Schedule、Partners 与 Get Tickets 都保持传统位置。
- **转化：** Get Tickets 是唯一高对比动作，视觉再激进也没有牺牲购票。
- **风险：** 白字叠渐变的局部对比、微型导航与 GPU Shader 需要真实设备测试。
- **可借鉴：** 越实验的画面越需要稳定的导航、CTA 和信息锚点。

#### 22. Son Daven — 情绪神话之后是完整投资证据

证据：[CSSDA WOTD / Final Judge 8.50](https://www.cssdesignawards.com/sites/son-daven/49788/)

- **首屏：** 黑褐山地、砂金点阵树、飞鸟、波纹山脉与超宽字标，像数字化古老织物或地形扫描。
- **叙事结构：** Prologue → 文化与概念 → 地点/地图 → 设施 → 四季 → 公寓 → 建设进度 → 财务指标 → 开发商 → FAQ / Contact。
- **转化：** Invest / Consultation 在不同叙事阶段反复出现，体验站没有失去商业目标。
- **风险：** 实站初次加载较重且页面极长；小字、复杂手势、户型和投资数据需要独立可访问结构。
- **可借鉴：** 高客单价产品可以先建立文化价值，但必须逐步交付地点、功能、数字、团队和风险等事实证据。

#### 23. Zorge 9 — 把“目标身份”与产品放在同一画面

证据：[CSSDA WOTD / Final Judge 8.50](https://www.cssdesignawards.com/sites/zorge-9/49749/)

- **证据限制：** CSSDA 没有公开稳定 live URL，本分析只基于官方获奖截图和标签，不冒充完整实站测试。
- **首屏：** 黑色大理石、黑裙模特、夕阳高层建筑与跨屏铜棕 `ZORGE N°9` 形成高级时装广告式拼贴。
- **视觉系统：** 黑、铜、肤色和晚霞；宽体优雅 sans、前后景遮挡、巨大字标压住图像。
- **风险：** 人物身份想象可能压过房产证据；暗色、微型文字和视差降低信息效率；无法核验具体 CTA。
- **可借鉴：** 目标用户形象可以与产品同屏，但要确保“身份”服务于产品，而不是替代产品。

#### 24. Aardvark Book Club — 实体商品成为首屏演员

证据：[CSSDA WOTD / Final Judge 8.04](https://www.cssdesignawards.com/sites/aardvark-book-club/49745/)

- **首屏：** 暖黄橙背景、浅黄波浪、粗黑定制字和多本从边缘切入的 3D 书；亮粉 CTA 与手写配送提示增加开箱感。
- **视觉系统：** BookTok 活泼语气、实体开箱和编辑策展结合；展示字、正文与手写批注形成三层声音。
- **导航与转化：** All Books、Gifting、FAQ、账户保持熟悉；加入书会、浏览本月书目和赠送订阅清楚。
- **风险：** 3D 书封、亮色与叠层易造成 CLS、阅读顺序和移动触控问题。
- **可借鉴：** 产品可在 Hero 中直接“表演”，但购买、账户与导航仍应遵循用户习惯。

#### 25. Motiondeep — 官网本身就是实时作品

证据：[CSSDA WOTD / Final Judge 8.25](https://www.cssdesignawards.com/sites/motiondeep/49758/)

- **首屏：** 暖灰白 3D 游戏场、小机器人与硬币，提示用户用 Click / WASD / Arrows 移动。
- **视觉系统：** 极小全大写几何字与游戏 HUD，把工作室的 Motion 能力变成一个实时 Demo。
- **导航：** About、What We Do、Reel、Projects、Awards、Contact 藏在探索层；另有 Play Game 和 Get in Touch。
- **风险：** 用户开始玩之前不知道业务价值；WASD、鼠标、倾斜传感器与 WebGL 对屏幕阅读器和移动设备都不友好。
- **可借鉴：** 作品集可以把专业能力变成 Demo，但要同步提供一句静态定位、普通导航和跳过入口。

#### 26. Noho Redesign Concept — 一句原则，一组使用姿态

证据：[CSSDA WOTD / Final Judge 8.22](https://www.cssdesignawards.com/sites/noho/49762/)

- **重要限制：** 这是非商业 redesign concept，不是 Noho 生产电商站；没有真实结账、CMS、SEO、客服和性能压力。
- **首屏：** 绿色草地背景中央浮奶油色界面卡，左侧一句巨大产品原则，右侧用紫、橙、绿小块拼出人物使用椅子的姿态。
- **视觉系统：** 环保自然背景 + 编辑式拼贴；粗黑 sans 与极简顶栏，左右 50/50 分屏。
- **风险：** 8.22 只能证明概念设计受到认可，不能写成生产电商转化验证。
- **可借鉴：** Purify 可以左侧写一句结果主张，右侧直接展示原始网页、干净 Markdown、引用和 Token 对比，以产品证据证明原则。

#### 27. Huts — 高客单价服务不一定需要图片 Hero

证据：[SiteInspire 编辑收录](https://www.siteinspire.com/website/13492-huts)

- **首屏：** 深橄榄绿整屏、象牙白高反差 serif 大标题、白色字标和浅黄绿 Get Started；没有房屋照片。
- **视觉系统：** 土地感、建筑事务所式可信度，排版代替豪宅大图。
- **内容结构：** 第二屏以后再展开住宅类型、过程、标准、作品、证言和数据，信息由情绪逐步转向证明。
- **转化：** Get Started 在顶栏和正文重复，稳定且单一。
- **风险：** 第二屏必须迅速出现实物证明；大 serif 在窄屏要严格控制换行；Cookie 面板不应侵占首屏。
- **可借鉴：** Purify 现有米白与 serif 可以保留，但真实 API 输出必须比现在更早出现。

#### 28. Squarespace Foundations — 品牌规范变成可游览的博物馆

证据：[SiteInspire 编辑收录](https://www.siteinspire.com/website/13465-squarespace-foundations)

- **首屏与结构：** 超大标志/字形动画后，页面按 Logo、Typography、Color、Photography、Campaign、Motion 六章组织；中间每次只显示一个主资产。
- **视觉系统：** 中性无衬线、极少层级、极大留白；背景随章节切换，让资产成为主角。
- **导航：** Index 提供全局目录，左右箭头和 Tap to Explore 提供线性路径。
- **风险：** 自动切换会让用户失去控制；媒体失败、键盘、Pause 与 Reduced Motion 都要有替代。
- **可借鉴：** Purify 的设计系统或技术文档可采用“章节 + 单一主资产 + 全局索引”，不要把所有规范堆成一页。

#### 29. Portal CG — 先展示“做出来的感觉”

证据：[Lapa Ninja 编辑收录](https://www.lapa.ninja/post/portal/)

- **首屏：** 3D Logo 部件、CG Showreel 和大体量字体构成工作室身份；导航只保留 About、Work、Jobs、Contact 与 Let’s Talk。
- **视觉系统：** 重字重 Space Grotesk、小型全大写导航、运动影像片头感。
- **结构与转化：** 先建立能力感，再给客户、专业范围、项目网格和团队；CTA 是 Let’s Talk 与 See All Projects。
- **上线风险：** 实测曾出现灰白空画布、Vimeo 不可用和占位字符串，说明策展收录不能替代生产 QA。
- **可借鉴：** 媒体与 Canvas 必须提供失败回退；创意质感和线上可靠性同样属于品牌。

#### 30. Azione — 客户 Logo 墙被升级为动态证据带

证据：[Lapa Ninja 编辑收录](https://www.lapa.ninja/post/azionepr/)

- **首屏：** 三列生活方式影像/视频拼贴，超大白色 AZIONE 压在画面上；中心是 agency 定位与 View Work，底部滚动 HOKA、Away、J.Crew、REI、Fender、Sweetgreen 等客户名。
- **视觉系统：** 超大现代 sans、编辑 serif 客户带和小号功能字，把公关公司的文化捕捉能力做成时尚杂志封面。
- **结构：** 作品、服务、Influencer、About、Instagram、Contact 清楚，影像之后再展开服务与社交内容。
- **风险：** 自动视频、遮挡字与跑马灯增加移动负担；重复客户名会在无障碍树中制造噪声。
- **可借鉴：** Purify 的信任区不要只放静态 Logo，可将真实站点、成功引擎、Token 节省或客户案例做成有上下文的证据带，但必须避免无意义重复。

### 4.4 获奖方法标杆：Dropbox Brand Guidelines

来源：[官网](https://www.dropboxbrand.com/) · [CSSDA WOTY 9.03](https://www.cssdesignawards.com/woty2025/sites/dropbox-brand/) · [Webby 2025](https://winners.webbyawards.com/2025/websites-and-mobile-sites/features-design/best-visual-design-function/333662/dropbox-brand-website)

电脑实站核验显示，Dropbox Brand 的优秀不在于“动效多”，而在于整个站点被一套简单规则统治：

- 白色底面由极细蓝色网格切分；
- 首屏只出现一段巨大 Dropbox Blue 文案和极大留白；
- 向下滚动时，橙、青、蓝色块依次进入网格，中心蓝卡承载核心文字；
- 同一网格可以容纳文字、图标、插画、摄影、色彩和 Motion，不需要每一章发明新布局；
- 交互鼓励“发现细节”，但内容依旧是可识别的品牌规范；
- CSSDA 把它评为 2025 WOTY 9.03 与 Best UX，Webby 还认可其 UX 与移动响应式设计。

对 Purify 的启发不是复制 Dropbox 蓝，而是：**先定义一个可扩展的空间规则。** Purify 可以用“来源网格 / 干净文本块 / 引用边界”组织所有模块，让视觉、Demo、Benchmark 和 Docs 看起来属于同一产品。

---

## 5. 三十个网站共同揭示了什么

### 5.1 一个成熟商业首页的真实叙事顺序

把 30 个网站放在一起看，最稳定的结构不是某种视觉风格，而是一条由“承诺”走向“证据”的路径：

```text
我是谁 / 为谁解决什么
        ↓
立即看见产品或结果
        ↓
确认有人真的在用
        ↓
理解它为什么有效
        ↓
找到与自己有关的场景
        ↓
核验性能、安全、部署与方法
        ↓
用最低成本开始
```

不同站点只是改变各环节的比重：

- OpenAI、Lovable、Perplexity 把“开始使用”直接提到第一步；
- Cursor、ElevenLabs、Runway 把“产品或结果”放到视觉中心；
- Harvey、Cohere、Scale 把“行业信任”提到前 1–1.5 屏；
- Anthropic 先建立机构气质，再用研究、安全与产品页面补足证据；
- Lando Norris、Igloo、Yamê 等体验站把情绪放得更大，但它们不承担 API 产品的解释和转化任务。

因此，对商业 AI 官网来说，**情绪是入口，证据才是主干，行动是出口。**

### 5.2 四条主要视觉路线

| 路线 | 代表网站 | 核心做法 | 最适合 | 主要风险 |
|---|---|---|---|---|
| 产品工具型 | OpenAI、Cursor、Perplexity、Lovable、Exa、Tavily | Hero 就是输入框、编辑器、音频或结果面板 | 可自助体验、价值能在 30 秒内显现的产品 | Demo 假、慢或失败会直接伤害品牌 |
| 编辑机构型 | Anthropic、Harvey、Cohere、Huts | 大排版、克制配色、人物/研究/行业影像 | 高信任、长决策周期、受监管行业 | 容易“有气质但不知道卖什么” |
| 计算系统型 | Mistral、Vercel、Midjourney、Motiondeep | 用像素、网格、字符、节点或 3D 规则表达系统 | 基础设施、开发者工具、创意技术 | 容易牺牲阅读、移动性能与无障碍 |
| 沉浸叙事型 | Runway、Lando Norris、Igloo、Yamê、Son Daven | 影片、空间、人物和滚动章节构成世界观 | 娱乐、文化、奢侈品、作品集 | 用户需要等待；商业路径容易被故事淹没 |

Purify 不应只选其中一条。更合适的组合是：**产品工具型为骨架、编辑机构型为气质、计算系统型只用于解释抓取状态。** 沉浸叙事只保留在一个短 Vision 区，不再主导整页。

### 5.3 十四条高频设计规律

#### 规律 1：一句话必须同时回答对象、动作与结果

“AI-powered”“next-generation”“intelligence meets the world”都可以制造氛围，却无法单独完成定位。更有效的句式通常包含：

> 为谁 + 做什么 + 得到什么结果

例如 Exa 的 “Web search, built for AI agents” 直接给出类别与对象；Cohere 强调企业安全部署；Cursor 指向使用者正在完成的工作。Purify 的一句话必须出现 **AI Agent、网页、干净上下文**，而不是只谈 Search 或 Evidence。

#### 规律 2：Hero 只需要一个主对象

高水平首屏常由一个视觉主角统治：一个输入框、一段真实 UI、一个人物、一张影片画面、一个几何系统或一句巨大标题。最常见的失败是同时放渐变球、代码卡、Logo 墙、三条数字、两组 CTA 和漂浮图标，导致任何元素都不再重要。

#### 规律 3：网站正在从“产品介绍”变成“产品前厅”

用户不愿为了理解一个 API 连续滚动。能安全开放体验的产品，首屏正在承担轻量 Playground 的职责：给一个默认示例、允许修改一个输入、显示有限而真实的输出，再把复杂配置交给完整控制台。

#### 规律 4：真实结果是最强的主视觉素材

Cursor 的代码 diff、ElevenLabs 的声音、Runway 的生成影像、Exa 的结果表格、Tavily 的多模式输入都表明：**产品输出越有辨识度，越不需要额外编造 AI 图形。**

#### 规律 5：品牌需要一条简单且可重复的生成规则

Mistral 不是“用了像素风”，而是把同一模块应用到图标、插图、动效、卡片和背景；Dropbox 不是“用了网格”，而是让所有内容遵守同一网格。真正可扩展的设计系统必须回答：“下一张图、下一种状态、下一页文档如何由同一规则生成？”

#### 规律 6：字体承担品牌角色，UI 字体承担任务角色

Anthropic、Exa、Harvey、Huts 用 serif 建立知识、编辑或专业感，但按钮、标签、代码、数据仍回到清晰 sans / mono。把展示字体用到表格、输入框和长段落会迅速损害效率。

#### 规律 7：颜色更像角色分配，而不是装饰

优秀站点通常有明确分工：背景负责气质，主色负责身份，强调色只标记行动或状态。例如 Tavily 的薄荷绿指向连接和激活；Dropbox Blue 负责核心品牌信息，橙/青负责章节变化。Purify 的矿物蓝应该表示“已净化/已验证”，而不是每个模块都涂蓝。

#### 规律 8：B2B 的信任必须在用户产生兴趣时出现

安全、部署方式、客户、使用规模、SLA、开源协议与基准若全部埋在页脚，用户在抵达之前就可能离开。最好的做法是首屏后给一个轻量 Proof Strip，在后半页再给完整方法与政策。

#### 规律 9：动效应该让状态变化可见

有意义的动效回答“系统刚才做了什么”：请求开始、页面导航、浏览器升级、内容清洗、引用生成、完成或失败。没有意义的动效只回答“设计师会做什么”。

#### 规律 10：顶层导航越复杂，首屏越需要极简

大型公司可以有 Mega Menu，但默认只暴露 4–6 个用户心智域。高水平站不会把每一项功能都提升为一级导航。产品、Use Cases、Developers、Pricing、Resources 足够承载大部分 API SaaS。

#### 规律 11：每一屏只完成一个说服任务

Runway 的首屏只证明视觉能力，Harvey 的行业卡只解释场景，Mistral 的系统段只解释能力组合。把功能、客户证言、定价和团队故事挤进同一屏，会让页面节奏失去主次。

#### 规律 12：桌面体验不能等比缩成手机

复杂 IDE、横向对照、3D 场景和大字号，在移动端必须重新编排为分步卡、上下对比、静态 poster 或单列摘要。缩小不是响应式设计，重新定义信息顺序才是。

#### 规律 13：失败回退也是视觉设计的一部分

本次核验中，部分策展站出现空画布、视频源不可用或占位字符串。高分截图不能保证线上体验。Canvas、视频、第三方播放器、动态数字和实时 Demo 都需要 Loading、Error、Retry 与静态替代。

#### 规律 14：奖项分数与商业效果没有直接等号

奖项能证明视觉、创意或技术执行受到认可，却不能自动证明激活率、留存、SEO、销售线索质量或无障碍表现。商业站只能借获奖站的“设计原则”，不能照搬它的等待成本和探索门槛。

### 5.4 可以复制、需要改造、应当避免

| 决策 | 模式 | Purify 的处理方式 |
|---|---|---|
| 直接采用 | Hero 可运行输入、单一主 CTA、真实输出、方法链接、失败回退 | 直接进入 V1 |
| 结合品牌改造 | 大 serif、编辑摄影、矿物蓝抽象图、章节式叙事 | 保留，但只服务定位、解释和 Vision |
| 谨慎使用 | 自动视频、Canvas、跑马灯、横向滚动、滚动触发状态机 | 每屏最多一个；有暂停、Reduced Motion 和静态替代 |
| 不采用 | 强制 Loading、滚动劫持、假终端、无方法的大数字、没有权限的 Logo 墙 | 明确列为设计验收禁项 |

---

## 6. 高分网站也常犯的错误

### 6.1 用“神秘感”替代定位

一句诗意标语可以提高记忆，却不能独立承担产品解释。修正方法不是删除品牌语言，而是给它一条明确的功能副标题。品牌层说“为什么存在”，产品层说“今天能做什么”。

### 6.2 把虚构界面当成产品证明

如果 Demo 中的查询、延迟、输出、进度和结果都与后端无关，用户会把它识别为动画。可以用预录结果，但必须明确写“示例输出”；更理想的是提供受限、可缓存、可失败的真实请求。

### 6.3 所有内容都获得同等视觉重量

当每一段都有大标题、彩色背景、滚动动画和悬浮卡片时，页面没有高潮。建议使用“强—静—强—静”的节奏，并确保一屏只有一个动作焦点。

### 6.4 Logo 墙没有上下文

未经授权的客户 Logo 有法律和信任风险；即使获得授权，仅显示 Logo 也无法解释价值。更有效的形式是：客户名 + 使用场景 + 一条可核验结果 + 案例链接。

### 6.5 用无限跑马灯制造“很忙”的感觉

跑马灯常导致文字重复、键盘焦点混乱、屏幕阅读器噪声和移动性能问题。如果信息重要，应可静止阅读；如果不重要，就不应占据首屏。

### 6.6 强制用户看完整开场

Loading 百分比、不可跳过片头、滚动吸附和自定义光标适合作品体验，不适合开发者 API 的主要购买路径。用户必须随时能看文档、复制 curl 或开始试用。

### 6.7 只有成功状态，没有真实边界

网页抓取天然存在 robots、登录墙、超时、限流、反爬、JS 渲染和部分内容等边界。只展示完美成功，会降低技术用户的信任。显示“何时失败、如何升级、返回什么错误”反而更专业。

### 6.8 追求动效而忽略内容可索引性

标题、功能、FAQ、代码和价格不应依赖 Canvas 或动画后才进入 DOM。快速滚动到底、禁用 JS、启用 Reduced Motion 后，核心内容仍应完整。

---

## 7. Purify 当前站点审计

### 7.1 产品真相与创意叙事之间的落差

仓库中的真实产品定义很清楚：[README](</Users/eason/Documents/purify-search/README.md:5>) 将 Purify 描述为 “Web Scraping API for AI Agents”，卖点是单二进制、零外部依赖、内置 MCP、REST 和自托管。当前路由实际提供 `/scrape`、`/extract`、`/batch/scrape`、`/crawl`、`/map`，可在 [API Router](</Users/eason/Documents/purify-search/api/router.go:43>) 核验；没有 `/search`。

创意首页则在 [Hero](</Users/eason/Documents/purify-search/creative/site-v1/prototype/index.html:61>) 使用 “Purify Search” 和 “Search is how intelligence meets the world”，并在 [Living Evidence](</Users/eason/Documents/purify-search/creative/site-v1/prototype/index.html:193>) 声称 Assimilation 会长期保存来源变化与冲突、Self-healing 会测试修复候选。当前仓库没有可对应的持久化证据图谱、跨时间冲突管理或候选验证系统。

这会造成三个具体问题：

1. 用户以为 Purify 是 Exa / Tavily 式搜索索引，随后却找不到 Search API；
2. 首页最抢眼的功能不可立即购买或验证，真实的 Scrape / Extract / MCP 反而被隐藏；
3. “Self-healing” 属高承诺用语，一旦没有方法、边界与失败记录，会损伤工程可信度。

### 7.2 当前设计资产的评估

| 项目 | 现状评分 | 判断 |
|---|---:|---|
| 产品真相一致性 | 4/10 | 品牌叙事超前于当前 API，Search 类别容易误导 |
| 首屏理解速度 | 4/10 | 使命清楚但产品、对象、输出和部署方式不清楚 |
| 视觉辨识度 | 9/10 | 纸白、深海、矿物蓝、serif/sans/mono 与纪实影像已经形成独特系统 |
| 产品证明 | 3/10 | 主要是抽象状态和示意记录，没有真实 URL 输入与响应 |
| 信息架构 | 5/10 | 四章叙事完整，但缺 Product、Use Cases、Developers、Pricing 等购买路径 |
| CTA 与转化 | 2/10 | “Explore the technology” 不是业务动作；无法就地试用或复制快速开始 |
| 信任证据 | 3/10 | 开源、协议、5 个 MCP 工具、部署方式、基准、错误边界未成为页面内容 |
| 动效概念 | 8/10 | 状态机、Reduced Motion 和性能边界考虑充分；当前只是与未来叙事绑定过深 |
| 无障碍基础 | 7/10 | 已有 Skip Link、语义、ARIA、alt 和 Reduced Motion 计划；仍需真实键盘与媒体 QA |
| 生产准备度 | 3/10 | 视频仍是 placeholder，Demo 与商业入口未连接，部署和数据采集闭环未定义 |

这里最不应该推倒重来的是视觉基础。[现有设计变量](</Users/eason/Documents/purify-search/creative/site-v1/prototype/styles.css:1>) 中的 paper、deep-ocean、mineral-blue 与字体分工都很合适；[Motion 文档](</Users/eason/Documents/purify-search/creative/site-v1/MOTION-RHYTHM-V1.md:214>) 对 Reduced Motion、可见性与性能也有成熟思考。问题是这些资产需要为真实产品服务。

### 7.3 应保留、重写和延期的内容

| 处理 | 内容 | 原因 |
|---|---|---|
| 保留 | 纸张感底色、深海色、矿物蓝、serif 展示字、mono 数据字、研究者/自然证据摄影 | 已形成区别于通用 AI 渐变的 Evidence Realism |
| 保留并改义 | 矿物碎片动画 | 从“证据自愈”改成“原始 DOM → 主体内容 → Markdown / citations” |
| 重写 | Hero、导航、Technology 三阶段、Meta Description、CTA | 与真实产品能力和搜索意图对齐 |
| 前置 | URL Demo、curl、Token 指标、引擎、MCP、单二进制、自托管、Apache 2.0 | 这些才是当前可验证的差异化 |
| 延期 | Assimilation、跨来源冲突、长期证据演进、Self-healing | 只有产品、数据模型、验证方法与可见失败分支上线后才能升级为主张 |

---

## 8. 推荐总方向：Evidence Utility

### 8.1 战略定义

推荐把现有 Evidence Realism 从“品牌影片主题”升级为 **Evidence Utility（证据实用主义）**：页面既有研究机构般的审慎气质，又像开发者工具一样可以立刻操作、检查和复现。

内容比重建议：

- **60% 产品证明：** URL Demo、输出、代码、状态、性能、引擎与部署；
- **25% 信任：** 开源、方法、错误边界、客户案例、安全与数据政策；
- **15% 愿景：** 开放网页、上下文质量、人类判断和未来路线。

### 8.2 类别、对象与差异化

| 层级 | 推荐答案 |
|---|---|
| 类别 | Open-source web context API / 开源网页上下文 API |
| 核心对象 | 构建 Agent、RAG、研究助手、知识库和自动化流程的开发者与团队 |
| 核心任务 | 抓取网页、清理噪声、转换格式、生成引用、提取结构化数据、批量/递归处理 |
| 差异化 | Go 单服务、自托管简单、REST + MCP、HTTP / 浏览器引擎、Token 指标、Apache 2.0 |
| 不抢的心智 | 独立网页索引、答案引擎、长期证据图谱、自治式事实自愈——除非产品已经实现 |

### 8.3 推荐 Hero 文案

**中文主版本**

> 面向 AI Agent 的开源网页上下文 API<br />
> **把网页，变成 AI Agent 可直接使用的干净上下文。**<br />
> Purify 自动选择 HTTP 或浏览器引擎，清理导航、广告和样式，返回 Markdown、引用、元数据与 Token 指标。通过 REST、MCP 或单二进制自托管使用。

主 CTA：**免费试一个网址**<br />
次 CTA：**查看 60 秒快速开始**<br />
文字链接：**GitHub ↗**

**英文对应版本**

> Open-source web context API for AI agents<br />
> **Turn webpages into clean context your agents can use.**<br />
> Purify chooses HTTP or browser rendering, removes navigation, ads and styling, and returns Markdown, citations, metadata and token metrics through REST, MCP or one self-hosted binary.

不建议继续把 “Search is how intelligence meets the world” 放在 H1；它可以作为后半页 Vision 区的大句子保留。

---

## 9. 推荐首页信息架构

### 9.1 完整页面顺序

| 顺序 | 模块 | 要回答的问题 | 关键内容 | 主动作 |
|---:|---|---|---|---|
| 1 | Outcome Hero | 这是什么，我现在能做什么？ | 定位、URL 输入、模式、示例输出 | Try a URL |
| 2 | Proof Strip | 为什么值得继续看？ | Apache 2.0、5 MCP tools、REST、自托管、单服务 | View GitHub |
| 3 | Live Product Proof | 实际发生了什么？ | 原始页面/干净 Markdown、SSE 状态、Token、耗时、引擎 | Open result / Copy response |
| 4 | Fetch → Clean → Deliver | 它如何工作？ | 取回、渲染升级、清洗、格式与交付 | Read architecture |
| 5 | Reliability | 复杂网页怎么办？ | HTTP / browser、动作、超时、重试、部分结果、错误边界 | View error model |
| 6 | Use Cases | 与我的工作有关吗？ | Agent、RAG、Research、Knowledge Base、Monitoring | View recipe |
| 7 | Benchmarks | 比替代方案好在哪里？ | Token、延迟、部署复杂度、质量；完整方法 | See methodology |
| 8 | Quickstart | 多快可以开始？ | Hosted curl、Docker、MCP 三个 Tab | Copy command |
| 9 | Hosted vs Self-host | 应该选哪种部署？ | 控制、运维、数据、价格、限制 | Start free / Self-host |
| 10 | Trust | 可以安全投入生产吗？ | 协议、版本、状态、数据政策、安全、真实案例 | Trust / Status |
| 11 | Vision Band | 品牌为何存在？ | “Search is how intelligence meets the world” 与真实观察影像 | Read the vision |
| 12 | Final CTA + Footer | 下一步是什么？ | URL 输入或 API Key；完整站点地图 | Start building |

### 9.2 顶部导航

推荐桌面导航：

```text
Purify   Product   Use Cases   Developers   Pricing   Resources      EN / 中文   GitHub   Try free
```

- **Product：** Scrape、Extract、Crawl、Map、Batch；
- **Use Cases：** Agents、RAG、Research、Knowledge Bases；
- **Developers：** Docs、API Reference、MCP、SDK/Examples、Changelog、Status；
- **Resources：** Benchmarks、Blog、Security、Roadmap；
- GitHub 是可见文字动作，不藏在 Resources 内；
- 移动端保留 Try free 固定动作，其余进入菜单；菜单打开后要锁滚动、焦点循环、Esc 关闭并回到触发按钮。

### 9.3 为什么这个顺序更有效

它把用户的认知风险逐层降低：

1. 先看到结果，而不是先相信愿景；
2. 看到开源与部署方式，确认不是封闭黑盒；
3. 看工作原理和失败边界，确认工程可控；
4. 看基准方法和真实案例，确认不是营销数字；
5. 最后用 curl、MCP 或 Docker 在自己环境开始。

---

## 10. Hero 与 Live Demo 的详细设计

### 10.1 桌面构图

首屏不建议传统的“左文案 + 右漂浮 Dashboard”。更适合 Purify 的是上下两层：

```text
┌────────────────────────────────────────────────────────────────┐
│ Logo  Product  Use Cases  Developers  Pricing        GitHub CTA│
├────────────────────────────────────────────────────────────────┤
│ Eyebrow                                                       │
│ 把网页，变成 AI Agent 可直接使用的干净上下文。                  │
│ 一段精确副文案                                                  │
│                                                                │
│ [ https://example.com/article                         ] [Run]  │
│ [Scrape] [Extract] [Crawl] [Map]     示例：Docs / News / Blog   │
├───────────────────────┬────────────────────────────────────────┤
│ 原始网页 / DOM 摘要     │ Clean Markdown                         │
│ 导航·广告·样式·脚本     │ 标题、正文、引用、元数据                │
│                       │ 11,708 → 5,572 tokens · 400ms · HTTP   │
└───────────────────────┴────────────────────────────────────────┘
```

Hero 的视觉主角不是输入框本身，而是 **“混乱 → 干净”这一组可检查变化**。自然影像和矿物蓝抽象图不在首屏与 Demo 抢主角，可放到后续 Vision。

### 10.2 输入区规则

- URL 输入使用真实 `<label>`，placeholder 只给格式示例；
- 默认填入稳定、允许抓取、输出清楚的示例页面；
- Run 按钮在输入合法前禁用，并说明原因；
- Scrape 为默认模式；Extract 在选择后出现 schema / prompt 的渐进式输入；
- Crawl / Map 因成本更高，可在首页使用固定示例或进入 Playground；
- 输入历史只保存在本地，除非用户明确同意账户同步；
- 明确告诉用户不要提交敏感 URL、登录凭据或无权访问的页面。

### 10.3 输出区需要展示的真实字段

| 类别 | 建议显示 | 设计表现 |
|---|---|---|
| 内容 | Markdown / Text / Citations | 可切换 Tab，代码与正文分别排版 |
| 来源 | source URL、final URL、title | 顶部来源条，可新开原网页 |
| 过程 | started、navigated、completed / error | 左侧时间线或紧凑状态条 |
| 引擎 | HTTP、browser / stealth（以真实返回为准） | 小型 mono badge，不用夸张动画 |
| 质量 | 原始 Token、清洗 Token、节省比例 | 数字 + 迷你条形图，同时给绝对值 |
| 性能 | total ms、缓存命中 | 与测试条件一起出现 |
| 资产 | links、images、metadata | 默认折叠，避免结果面板过载 |
| 操作 | Copy Markdown、Copy JSON、Open Playground | 操作后给可见且可朗读的成功反馈 |

### 10.4 Live Demo 的成本与安全边界

首页 Demo 不应成为开放代理：

- 服务端进行 URL 规范化、DNS/IP 校验和 SSRF 防护；
- 禁止 localhost、私网、云元数据与非 HTTP(S) 协议；
- 限制页面体积、重定向次数、执行时间、浏览器动作和输出长度；
- 公共体验使用速率限制与匿名额度，昂贵模式要求登录；
- 遵守 robots、站点条款与授权边界，清楚说明“不绕过登录与付费墙”；
- 对常用示例预热缓存，但在 UI 中标记 cache hit；
- 不把用户提交的 URL、响应内容或凭据用于模型训练，除非政策真实支持该承诺；
- Demo 出错时显示错误类别与下一步，而不是吞掉错误后播放成功动画。

---

## 11. 视觉系统：保留什么，如何让它更产品化

### 11.1 设计概念

视觉母题建议定义为：

> **The visible boundary between raw web and usable context.**<br />
> 让原始网页与可用上下文之间的边界变得可见。

这条规则可以生成全站资产：

- 来源页使用带噪声、稀疏、偏暖的纸张层；
- 清洗结果使用稳定网格、深墨文字与矿物蓝状态；
- 引用以细边界和编号把结果重新连接到来源；
- Browser 升级表现为边界被扩展，而不是魔法闪光；
- 错误与不确定状态保留空缺、断线或琥珀色说明，不伪装成成功。

### 11.2 色彩角色

| Token | 当前方向 | 推荐角色 | 使用限制 |
|---|---|---|---|
| Paper / White | `#f7f8f4` / `#fcfcf9` | 主页面、原始证据、长文本背景 | 保留，防止全白 SaaS 同质化 |
| Deep Ocean | `#061820` | 代码、Footer、技术深层区 | 连续深色区不超过 1–2 屏 |
| Mineral Blue | `#4c96b3` | 已完成、干净结果、主行动 | 不同时承担 warning / decorative |
| Ink | `#0a1519` | 正文与主标题 | 长文本优先，不使用低透明度灰 |
| Memory Indigo | `#6474d2` | 未来 / Vision 的辅助色 | 不作为产品成功色 |
| 新增 Amber | 建议另定义 | partial、warning、browser upgrade | 必须满足文字与背景对比 |
| 新增 Red | 建议另定义 | error、blocked、invalid | 不只靠颜色表达，始终配图标与文本 |

### 11.3 字体分工

- **Serif Display：** Hero H1、Vision 大句、研究引用；负责“证据、人文、时间感”；
- **Sans UI：** 导航、正文、按钮、表格和说明；负责效率与清晰；
- **Mono Data：** URL、HTTP 状态、事件、耗时、Token、curl；负责可检查性；
- 中文 serif 需要逐尺寸核验宋体字面、标点悬挂和中英文混排，不能只依赖系统 fallback；
- 正文不小于 16px，代码不以 11–12px 模拟“专业感”；
- 大标题最大行长控制在 9–14 个中文字符或约 12–16 个英文单词，避免窄屏孤字。

### 11.4 网格与空间

- 桌面沿用 12 栏、最大 1344px；Demo 建议 5/7 或 6/6 分栏；
- 组件内部使用 4px 基础步进，页面空间以 8px 系列扩展；
- 每个章节只允许一个“突破网格”的对象；
- 细 hairline 用于来源、引用和状态边界，不能在每张卡片外都画边框；
- 卡片圆角保持小到中等，不复制通用 24–32px 玻璃 SaaS 卡；
- 阴影只表达层级或浮层，不作为默认装饰。

### 11.5 影像与抽象图

继续使用真实自然、人类观察、纸张、地图、标本和材料影像，因为它们建立了独特的“证据来自世界”气质。但需要三条纪律：

1. 每张影像旁边必须说明它与产品命题的关系；
2. 产品段不用品牌影像代替真实 UI；
3. 所有品牌影片都提供 poster、字幕/文字摘要、暂停和 Reduced Motion 版本。

应继续避免的素材包括：通用紫蓝渐变、漂浮玻璃球、发光大脑、机器人头像、代码雨、没有意义的 HUD、无法解释的粒子宇宙。

---

## 12. 组件与状态设计

### 12.1 URL Demo 的完整状态表

| 状态 | 用户应看到什么 | 可执行动作 | 无障碍要求 |
|---|---|---|---|
| Empty | 一条明确示例与用途说明 | 输入、选择示例 | label 与 hint 关联 |
| Invalid | URL 哪一部分无效 | 修改输入 | 错误文字与 `aria-describedby` |
| Ready | 模式、预计成本/限制 | Run | 键盘 Enter 可执行 |
| Queued | 请求已接受、是否缓存 | Cancel | `aria-live=polite`，不连续播报计时 |
| Started | 请求 ID 与起始时间 | Cancel | 状态文字，不只用 spinner |
| Navigated | final URL、引擎、HTTP 状态 | 查看详情 | 时间线焦点顺序合理 |
| Browser upgrade | 为什么从 HTTP 升级 | 继续等待 / 取消 | 不用动画强迫用户等待 |
| Cleaning | 正在提取主体和引用 | 取消 | Reduced Motion 下静态进度 |
| Completed | 输出、Token、耗时、引擎 | Copy / Download / Open | 完成消息可被朗读一次 |
| Partial | 哪些内容成功、哪些缺失 | 使用部分结果 / Retry | 琥珀提示且文字说明 |
| Timeout | 阶段、时限、可能原因 | Retry / Docs | 保留用户输入 |
| Rate limited | 恢复时间与额度 | 登录 / 稍后再试 | 不只给 429 代码 |
| Blocked | robots、auth、paywall 或 policy | 查看规则 | 不承诺绕过 |
| Server error | request ID 与状态页 | Retry / Status | 避免“Something went wrong”空话 |
| Copy success | 已复制什么 | 继续操作 | 可见反馈 + 短 `aria-live` |

### 12.2 卡片体系

全站只需要四类卡：

1. **Proof Card：** 一个可核验结论 + 方法链接；
2. **Workflow Card：** 输入、处理、输出三个区域；
3. **Use-case Card：** 用户任务 + recipe / code；
4. **Trust Card：** 协议、安全、部署、状态或案例。

不同模块用同一骨架，通过内容与背景层级变化，不为每一屏发明新卡片。

### 12.3 按钮层级

- Primary：Try a URL / Get API key；每屏最多一个；
- Secondary：View quickstart / Open Playground；
- Text Link：Docs、Methodology、GitHub、Learn more；
- Danger：只在取消、清除或不可逆动作出现，官网通常不需要；
- 所有按钮必须有 loading、disabled、focus-visible、success / error 反馈；
- “Explore”“Discover”“Learn more”不能在没有上下文时充当主要转化文案。

---

## 13. 动效系统：从品牌影片改为产品状态语言

### 13.1 动效层级

| 层级 | 时长建议 | 用途 | 示例 |
|---|---:|---|---|
| Micro | 120–180ms | Hover、Pressed、Focus、Copy | 按钮、Tab、复制状态 |
| State | 220–320ms | 面板、状态和结果切换 | Scrape → Extract、结果 Tab |
| Reveal | 400–650ms | 媒体与章节进入 | before/after crossfade |
| Narrative | 700–900ms | 一次性品牌叙事 | Vision 文案与影像 |
| Continuous | 默认禁用 | 只允许真实进行中的过程 | 请求中简洁 activity，不使用无限装饰循环 |

### 13.2 三条硬规则

1. **每个视口最多一个主要运动源。** 视频播放时 Canvas 静止；Demo 状态变化时背景不再运动。
2. **状态来自真实事件。** 已有 SSE 可以映射 `scrape.started → scrape.navigated → scrape.completed / scrape.error`；不要播放固定时序后假装请求成功。
3. **用户永远能接管。** 自动轮播超过 5 秒必须暂停；Hover 不是唯一控制方式；Tab、播放、声音、关闭都能用键盘。

### 13.3 现有动效资产的重映射

| 现有概念 | 推荐新含义 |
|---|---|
| Hero 方块矩阵 | DOM 节点被识别、去噪并稳定为内容块 |
| Living Evidence 三阶段 | Fetch → Clean → Deliver |
| Signal / Candidate | HTTP 结果 → browser upgrade / retry，不再叫 Self-healing |
| Human Measure | 移到 Vision，表达“系统应保留来源与不确定性” |
| 四幕品牌影片 | 缩短为 10–15 秒可跳过 Vision 资产，不阻塞 Hero |

### 13.4 Reduced Motion 与低性能模式

- `prefers-reduced-motion: reduce` 时直接显示最终状态；
- 禁用平滑滚动、视差、滚动 scrub、自动轮播和 autoplay；
- 视频显示 poster 与显式播放；
- 状态变化保留必要的 opacity，不依赖位移；
- `saveData`、低内存、低核心数或小视口时不初始化非必要 Canvas；
- 页面隐藏或模块低于可见阈值时停止计时器、视频和绘制；
- 低性能回退不能删除文字、代码或 CTA。

---

## 14. 信任、基准与内容证据

### 14.1 首屏后的 Proof Strip

只放真实、长期成立且能点开验证的 4–5 项：

```text
Apache 2.0  ·  Built-in MCP (5 tools)  ·  REST API  ·  Single-service self-host  ·  Citation output
```

GitHub Stars、请求数、客户数和延迟不应写死在 HTML 中；如果展示，应有更新时间、数据源和异常回退。没有足够规模时，开源协议和产品能力比一个很小的 vanity number 更有说服力。

### 14.2 Benchmark 的最低方法标准

任何“52–99% Token 节省”“低于 X ms”“优于某竞品”的主张，旁边都要能打开 Methodology。至少公开：

- 测试日期、Purify 版本或 commit；
- 测试机器、区域、网络和冷/热缓存条件；
- URL 样本集类别、数量和选择规则；
- 每个样本运行次数、median、p75 / p95，而不是只报最好值；
- Tokenizer 名称与版本；
- 内容质量评分规则，防止“删得最多”被误当成“最好”；
- 失败率、超时、被阻止和部分成功；
- 对比产品的版本、配置、价格日期与官方来源；
- 原始 JSON / CSV 与可复现脚本；
- 明确区分实验室基准、生产遥测与单个案例。

推荐展示四组图，而不是一张“总分”：

1. 内容保真度；
2. Token 降低；
3. 延迟分布；
4. 部署复杂度 / 资源占用。

### 14.3 Trust 页面必须回答的问题

- 传入 URL、Headers、页面内容、提取 schema 和输出会保留多久？
- 是否记录日志、用于训练或交给第三方模型？
- Hosted 与 Self-host 的数据路径有何不同？
- API Key 如何存储、轮换与撤销？
- 支持哪些加密、区域、子处理方与安全认证？
- 浏览器引擎怎样隔离，如何防 SSRF 和恶意页面？
- 如何处理 robots、版权、登录墙与滥用？
- 漏洞如何报告，状态页和事故历史在哪里？
- 开源仓库、发布包与 Hosted 服务是否同版本？

没有认证时不要用模糊盾牌图标暗示“企业级安全”。可以诚实写当前已实现的控制、限制和路线图。

### 14.4 客户与案例

优先顺序应是：

1. 有授权的客户案例；
2. 可公开的开源用户 / 集成；
3. 匿名但有方法的使用场景；
4. 产品自身的可重复 Demo。

每个案例用相同模板：背景 → 原问题 → Purify 配置 → 可核验结果 → 限制 → 代码 / 架构。一个完整案例比二十个无上下文 Logo 更有效。

---

## 15. 响应式、无障碍和性能验收

### 15.1 移动端不是桌面缩小版

移动首屏顺序建议：定位 → URL 输入 → Run → 状态摘要 → Clean Markdown → 原始网页折叠。Before/After 从左右对照改成 Tab 或上下段，避免 320px 宽屏强塞双栏。

- 顶栏只保留 Logo、Try 和 Menu；
- 模式选择横向可滚动但需有可见截断提示，或改成 Select；
- 代码块默认显示关键 6–10 行，用户再展开；
- 表格转成 definition list / card，不让用户横向找列名；
- Sticky CTA 不能遮住结果、Cookie 控件或系统安全区；
- 长 URL 允许中间断行，但 Copy 保持原值；
- 输入、按钮和 Tab 触控目标至少 44×44 CSS px。

### 15.2 WCAG 2.2 AA 核对清单

- 一个页面一个清楚 H1，标题层级不跳；
- 有 Skip Link、Landmark 与描述性页面标题；
- 所有交互使用原生 button / a / input，不用可点击 div；
- 完整键盘路径、清楚 `:focus-visible`，菜单与对话框管理焦点；
- 前景/背景达到对比要求；大面积低对比灰不能承载正文；
- 颜色、位置、形状都不是唯一状态线索；
- 输入有可见 Label、帮助、错误定位与恢复；
- 动态结果使用节制的 live region，不把每个百分比变化都朗读；
- 视频有字幕或等价文本，音频默认静音且可暂停；
- 图片根据作用写 alt；纯装饰使用空 alt，不重复旁边文案；
- 动画、自动更新、跑马灯可暂停；Reduced Motion 信息完整；
- 200% 缩放和 320 CSS px reflow 不丢内容、不出现双向滚动；
- 中英文切换同步更新 `lang`、标题、Meta 与辅助文本。

### 15.3 性能目标

外部发布底线采用 Core Web Vitals 的 p75：LCP ≤ 2.5s、INP ≤ 200ms、CLS ≤ 0.1。项目内部建议采用更严格的工程预算：

| 指标 | 内部目标 | 实现建议 |
|---|---:|---|
| LCP | ≤ 1.8s | Hero 不依赖视频；关键 CSS 与字体策略明确 |
| INP | ≤ 100ms | Demo 更新分片；不在主线程做大段清洗 |
| CLS | ≤ 0.05 | 图片/视频/结果容器预留尺寸；数字更新不改变布局 |
| 首屏传输 | ≤ 500KB（压缩后，第三方另审） | 品牌影片和后半页图延迟加载 |
| 初始 JS | ≤ 80KB gzip（目标） | 原生交互优先；不要为简单 reveal 引入大型运行时 |
| 长任务 | 单次 < 50ms | Canvas 分帧；结果渲染分页或虚拟化 |
| 字体 | 首屏最多 2 个必要切片 | 自托管 WOFF2、subset、合理 fallback |

还需要：

- Hero LCP 使用静态文本或优化后的图，不让 autoplay video 成为 LCP；
- 图片使用 AVIF/WebP、明确宽高、响应式 `srcset`；
- 第三方分析、客服、Cookie 与视频播放器按同意和可见性加载；
- Demo API 与官网静态资源分域或隔离故障，接口失败不能让整站不可用；
- 静态文案、Docs、Pricing 和代码在 Demo 服务离线时仍可访问；
- 上线后用真实用户监测按设备、国家和网络拆分 p75，而不是只看开发机 Lighthouse。

### 15.4 SEO 与可发现性

- 首页 Title 直接包含 “Web Scraping API for AI Agents” 或 “Web Context API”；
- Meta Description 写能力、对象和部署方式，不写未来功能；
- Product、Docs、Pricing、Benchmark、MCP、Self-host 有可索引独立 URL；
- 使用 Product / SoftwareApplication、Organization、Breadcrumb、FAQ 等结构化数据时，只标记页面真实可见内容；
- Canonical、Open Graph、Twitter Card、sitemap、robots 与多语言 hreflang 完整；
- JavaScript 关闭后仍能获得主要文案、链接与代码；
- API 文档版本化，旧链接有稳定重定向；
- 不用隐藏文字或自动生成大量薄内容页面追逐关键词。

---

## 16. 转化与实验设计

### 16.1 核心漏斗

```text
Landing view
  → URL entered
  → Demo run
  → Useful result viewed/copied
  → API key or self-host quickstart
  → First successful API request
  → Second-day / seventh-day retained use
```

首页优化不能只看 CTA 点击率。更重要的是“第一次有用结果”和“第一次成功 API 请求”。如果 Hero 点击多而成功请求少，说明定位或体验在制造误导。

### 16.2 推荐事件

- `hero_url_example_selected`
- `demo_mode_selected`
- `demo_run_started`
- `demo_run_completed` / `demo_run_failed`，含错误类别但不含敏感 URL；
- `result_markdown_copied`
- `quickstart_tab_selected`
- `quickstart_command_copied`
- `api_key_started`
- `github_opened`
- `methodology_opened`
- `self_host_opened`

分析数据应避免保存完整用户 URL、Headers、内容和 schema；域名级数据也要有明确政策与脱敏策略。

### 16.3 首轮 A/B 测试

| 实验 | A | B | 主要指标 | 护栏指标 |
|---|---|---|---|---|
| 类别文案 | Web scraping API | Web context API | Demo 启动、首次成功请求 | 跳出、错误理解访谈 |
| Hero 结构 | 先输入 | 先 before/after 示例 | Useful result viewed | LCP、失败率 |
| 主 CTA | Try a URL | Get API key | 首次成功请求 | 低质量注册 |
| 信任顺序 | 开源/自托管前置 | 客户/规模前置 | Quickstart 进入率 | 企业线索质量 |
| 快速开始 | curl 默认 | MCP 默认 | 命令复制后成功率 | 不同用户类型偏差 |

每次只测试一个主要认知变量；不要同时换文案、颜色、布局和 CTA 后把结果归因于某一个因素。样本不足时用可用性访谈和会话回放发现问题，不制造“统计显著”。

---

## 17. 分阶段实施路线图

### Phase 0：产品真相对齐（1–2 天）

- 正式决定对外品牌是 Purify，还是 Purify Search 仅代表未来产品线；
- 以当前 API 清单建立 claim matrix：已上线 / Beta / 路线图 / 不宣传；
- 删除或降级 Assimilation、Self-healing、跨时间冲突等未实现主张；
- 确定唯一主转化：免费 API Key、匿名 URL Demo 或 Self-host，三者不能同权；
- 为所有数字建立来源、更新时间和负责人。

**验收：** 用户在 5 秒内能说出“这是给 AI Agent 用的网页清洗/上下文 API”，并且找得到对应路由。

### Phase 1：转化首页 MVP（3–5 天）

- 重写 Hero、Title、Meta、导航和 CTA；
- 使用固定真实样本完成 before/after，不依赖 Live Demo 也能证明价值；
- 前置 Apache 2.0、MCP、REST、单服务自托管；
- 增加三 Tab Quickstart：Hosted curl / Docker / MCP；
- 将原 Living Evidence 改为 Fetch → Clean → Deliver；
- 补 FAQ、错误边界、Pricing / Hosted vs Self-host 基础信息。

**验收：** 禁用动画或接口后，用户仍能理解产品并复制一个可执行命令。

### Phase 2：真实 Demo 与工程证据（1–2 周）

- 接入受限 URL Demo 与真实 SSE 状态；
- 完成所有成功、失败、超时、限流、部分结果和 browser-upgrade 状态；
- 构建可复现 benchmark 页面与原始数据下载；
- 增加 Trust、Status、Changelog 与至少一个完整案例；
- 上线必要的安全防护、速率限制、缓存与成本监控。

**验收：** Demo 失败不会破坏页面；每一个重要数字都能打开方法；错误状态比成功状态同样完整。

### Phase 3：生产质量（约 1 周，随后持续）

- 双语路由、SEO、分享卡、sitemap 与结构化数据；
- 跨浏览器、键盘、屏幕阅读器、200% Zoom、Reduced Motion、低性能设备 QA；
- Core Web Vitals 预算与真实用户监测；
- 视频、图片、Canvas 的 poster / fallback / lazy-load；
- 埋点隐私审查、转化漏斗与 Demo 成本告警；
- 明确静态站部署、CDN、缓存、回滚和 API 故障隔离。

**验收：** 移动/桌面 p75 达到质量底线；任一媒体或 Demo 服务失败时，核心购买路径仍然成立。

### Phase 4：品牌深度与未来产品（持续）

- 将现有自然、人类观察与 Evidence 影片用于发布、About、Vision 和品牌内容；
- 只有在真实数据模型与验证逻辑上线后，再发布 Assimilation / Self-healing；
- 新能力先通过 Docs、Demo、方法和失败边界被证明，再升级为 Hero 语言；
- 建立真实案例、模板、集成和开发者教育内容，而不是持续增加装饰模块。

---

## 18. P0 / P1 / P2 决策清单

### P0：上线前必须完成

- [ ] “Purify Search” 与当前非 Search API 的命名冲突得到解决；
- [ ] 所有主页主张对应当前可运行能力；
- [ ] Hero 改成结果主张，并出现真实产品输出；
- [ ] 主 CTA 可以完成，不再是页面内“Explore”；
- [ ] URL Demo 有 SSRF、额度、超时与错误回退；若来不及则使用明确标记的固定示例；
- [ ] Hosted、Self-host、MCP、License 与价格信息一致；
- [ ] 基准数字提供方法与日期；
- [ ] 快速开始命令实际跑通；
- [ ] 视频/Canvas 失败不产生空白首屏；
- [ ] 键盘、Reduced Motion、移动端和 Core Web Vitals 通过验收。

### P1：首个正式版本应完成

- [ ] 完整 Use Cases 与 recipe；
- [ ] Trust、Status、Changelog、数据保留说明；
- [ ] 一个可公开、可量化、有代码的案例；
- [ ] 可下载 benchmark 数据与复现脚本；
- [ ] 双语独立 URL 与完整 SEO；
- [ ] 真实用户性能监测和从 Demo 到首次 API 成功的漏斗。

### P2：有数据后再做

- [ ] 个性化示例、复杂滚动叙事、更多 3D / Canvas；
- [ ] 自动切换行业场景；
- [ ] 大规模动态 Logo / 数据带；
- [ ] Assimilation、长期证据图谱与 Self-healing 品牌叙事；
- [ ] 为奖项提交而增加的实验性交互。

---

## 19. 最终建议

如果只能做一轮改版，优先完成这五件事：

1. **把类别说真。** 从模糊的 Purify Search 改为面向 AI Agent 的开源网页上下文 / 抓取清洗 API。
2. **把产品放进 Hero。** 用户输入 URL，立刻看见原始网页与干净 Markdown、Token、耗时、引擎和引用。
3. **把差异化前置。** 单服务、自托管、REST + MCP、Apache 2.0、HTTP / browser 与可复现方法进入前两屏。
4. **保留现有品牌气质。** 纸白、深海、矿物蓝、serif 与真实观察影像是资产；让它们支撑产品，不再代替产品。
5. **把未来愿景放到它应在的位置。** “Search is how intelligence meets the world” 可以成为令人记住的结尾；“今天可以运行什么”必须成为开头。

这会让 Purify 不必在 Exa 的搜索索引、Tavily 的通用 Web Access 或 Firecrawl 的功能广度上正面复制，而是占据一个更清晰的位置：

> **一个可检查、低 Token、可引用、容易自托管的 AI Agent 网页上下文层。**

---

## 20. 主要来源与复核入口

### 排名与评审机制

- [CSS Design Awards 2025 Website of the Year Winners](https://www.cssdesignawards.com/blog/2025-website-of-the-year-winners/430/)
- [CSS Design Awards — About / Judging](https://www.cssdesignawards.com/about)
- [Awwwards Annual Awards Winners](https://www.awwwards.com/annual-awards/winners)
- [Dropbox Brand — CSSDA WOTY 2025](https://www.cssdesignawards.com/woty2025/sites/dropbox-brand/)
- [Dropbox Brand — Webby 2025](https://winners.webbyawards.com/2025/websites-and-mobile-sites/features-design/best-visual-design-function/333662/dropbox-brand-website)

### 质量标准

- [W3C Web Content Accessibility Guidelines 2.2](https://www.w3.org/TR/WCAG22/)
- [web.dev — Web Vitals](https://web.dev/articles/vitals)

### Purify 本地事实来源

- [产品定义与快速开始](</Users/eason/Documents/purify-search/README.md:5>)
- [当前 API Router](</Users/eason/Documents/purify-search/api/router.go:31>)
- [当前创意首页 Hero](</Users/eason/Documents/purify-search/creative/site-v1/prototype/index.html:61>)
- [当前视觉 Tokens](</Users/eason/Documents/purify-search/creative/site-v1/prototype/styles.css:1>)
- [现有 Motion / Reduced Motion 规范](</Users/eason/Documents/purify-search/creative/site-v1/MOTION-RHYTHM-V1.md:214>)

> 本报告是 2026-08-09 的设计研究快照。网站会持续改版；AI 组分数是本次研究的统一编辑评分，奖项组只保留主办方或策展平台的原始口径。视觉观察与定性性能风险不等同于统一实验环境的 Lighthouse 或真实用户数据，做最终采购和上线判断时仍应复测。
