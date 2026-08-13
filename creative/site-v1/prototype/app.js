const VISUAL_LOCK_STATIC = true;
const HERO_MATRIX_LIVE = true;
const PAGE_PARAMS = new URLSearchParams(window.location.search);
const HERO_VARIANT = PAGE_PARAMS.get("hero") === "a" ? "a" : "b";
const CAPTURE_MODE = PAGE_PARAMS.has("capture");
const THEME_STORAGE_KEY = "purify-theme";
const SYSTEM_THEME_MEDIA = window.matchMedia("(prefers-color-scheme: dark)");
const THEME_COLORS = { light: "#fcfcf9", dark: "#0a111b" };

if (CAPTURE_MODE) document.documentElement.classList.add("capture-mode");
document.documentElement.dataset.heroVariant = HERO_VARIANT;

const copy = {
  en: {
    "nav.product": "Product",
    "nav.developers": "Developers",
    "nav.vision": "Vision",
    "nav.journal": "Journal",
    "nav.docs": "Docs",
    "nav.signIn": "Sign in",
    "nav.trial": "Try Purify",
    "nav.menu": "Menu",
    "theme.dark": "Switch to dark mode",
    "theme.light": "Switch to light mode",
    "hero.eyebrow": "Purify Search",
    "hero.title": "Search is how intelligence<br />meets the world.",
    "hero.lead":
      "Purify Search is building a path from the open, changing web to context AI systems can inspect, update, and use.",
    "hero.primary": "Try Purify",
    "hero.secondary": "Read our vision",
    "pillars.eyebrow": "Purify Search API",
    "pillars.title": "Search that proves every answer.",
    "pillars.sub":
      "Purify fetches the live page, finds it in an index we own, and verifies the claim before it reaches you.",
    "pillars.c1Title": "Fetch",
    "pillars.c1Body":
      "Reach any public page with a real Chrome fingerprint — TLS, HTTP/2, full rendering, and an archive fallback.",
    "pillars.c2Title": "Find",
    "pillars.c2Body": "A bilingual index we build and grow ourselves — no resold rankings, no upstream terms.",
    "pillars.c3Title": "Verify",
    "pillars.c3Body":
      "Independent sources cross-checked, conflicts kept visible, and every receipt signed with Ed25519.",
    "dev.eyebrow": "For developers",
    "dev.title": "The receipt is in the response.",
    "dev.sub":
      "GET /search returns results the way your agent needs them — the snapshot ID, the observed time, and a signed receipt, inline.",
    "dev.b1": "snapshot_id pins the exact bytes an answer was read from",
    "dev.b2": "observed_at timestamps every read",
    "dev.b3": "The receipt replays through /verify",
    "dev.link": "Read the API docs",
    "rec.eyebrow": "Built-in verification",
    "rec.title": "Disagreement stays on the record.",
    "rec.sub":
      "When sources conflict, Purify does not guess. Each claim stays attached to its source; nothing is promoted until verification settles it.",
    "rec.b1": "Conflicts retained, never hidden",
    "rec.b2": "Every source carries its observed time",
    "rec.b3": "One receipt covers the whole path",
    "rec.link": "See the trust stack on GitHub",
    "specimen.kicker": "SEARCH RECEIPT / ILLUSTRATIVE RECORD",
    "specimen.title": "Every answer ships with a receipt you can audit.",
    "specimen.status": "Needs review",
    "specimen.queryLabel": "QUESTION",
    "specimen.query": "When was the security patch released?",
    "specimen.summaryLabel": "PURIFY NOTE",
    "specimen.summary":
      "Two sources disagree on the release date. Each claim stays attached to its source and snapshot; neither is promoted until verification settles it.",
    "specimen.source": "Source",
    "specimen.observed": "Observed",
    "specimen.state": "State",
    "specimen.current": "Current",
    "specimen.conflict": "Conflict retained",
    "specimen.changed": "Changed",
    "stats.title": "Built to be checked.",
    "stats.l1": "FETCH ENGINES",
    "stats.l2": "LANGUAGES INDEXED",
    "stats.l3": "ANSWERS SNAPSHOTTED",
    "stats.l4": "RECEIPT PER ANSWER",
    "vision2.eyebrow": "The road ahead",
    "vision2.title": "From answers to a living fact layer.",
    "vision2.sub":
      "Receipts accumulate into memory. Memory learns to watch for change, heal what breaks, and keep every repair inspectable. Search is only the first layer.",
    "cta.title": "Start building on verified answers.",
    "cta.sub": "One call to /search. Evidence in every response.",
    "cta.primary": "Try Purify",
    "cta.secondary": "Read the docs",
    "footer.note": "Search that can show its work.",
    "footer.product": "PRODUCT",
    "footer.perspective": "PERSPECTIVE",
    "footer.company": "COMPANY",
    "footer.contact": "Contact",
    "footer.privacy": "Privacy",
    "footer.legal": "Legal",
    "footer.made": "Built for an open, changing world."
  },
  zh: {
    "nav.product": "产品",
    "nav.developers": "开发者",
    "nav.vision": "愿景",
    "nav.journal": "手记",
    "nav.docs": "文档",
    "nav.signIn": "登录",
    "nav.trial": "试用 Purify",
    "nav.menu": "菜单",
    "theme.dark": "切换到暗黑模式",
    "theme.light": "切换到明亮模式",
    "hero.eyebrow": "Purify Search",
    "hero.title": "搜索，是智能<br />理解世界的方式。",
    "hero.lead":
      "Purify Search 正在建立一条通往开放且持续变化的网络之路，把其中的信息转化为 AI 系统能够检查、更新并使用的上下文。",
    "hero.primary": "试用 Purify",
    "hero.secondary": "阅读我们的愿景",
    "pillars.eyebrow": "PURIFY 搜索 API",
    "pillars.title": "让每一条答案，都能自证。",
    "pillars.sub": "Purify 抓取实时页面，在自建索引中检索，并在结果抵达你之前完成核验。",
    "pillars.c1Title": "抓取",
    "pillars.c1Body": "以真实 Chrome 指纹抵达任何公开页面——TLS、HTTP/2、完整渲染，外加历史存档回退。",
    "pillars.c2Title": "检索",
    "pillars.c2Body": "自建自养的中英双语索引——不转售排序，不受上游条款约束。",
    "pillars.c3Title": "核验",
    "pillars.c3Body": "交叉核对独立来源，冲突保持可见，每张回执都以 Ed25519 签名。",
    "dev.eyebrow": "面向开发者",
    "dev.title": "回执，就在响应里。",
    "dev.sub": "GET /search 以智能体需要的方式返回结果——快照 ID、观察时间与签名回执，全部内联。",
    "dev.b1": "snapshot_id 锁定答案所依据的原始字节",
    "dev.b2": "observed_at 记录每一次读取的时间",
    "dev.b3": "回执可经 /verify 重放核验",
    "dev.link": "查看 API 文档",
    "rec.eyebrow": "内建核验",
    "rec.title": "分歧，留在记录上。",
    "rec.sub": "来源冲突时，Purify 不做猜测。每种陈述都与各自来源保持关联；核验完成之前，谁也不会生效。",
    "rec.b1": "冲突保留，绝不隐藏",
    "rec.b2": "每个来源都带观察时间",
    "rec.b3": "一张回执覆盖全程",
    "rec.link": "在 GitHub 查看信任栈",
    "specimen.kicker": "搜索回执 / 示意记录",
    "specimen.title": "每一条答案，都附带一张可以复核的回执。",
    "specimen.status": "等待复核",
    "specimen.queryLabel": "问题",
    "specimen.query": "这个安全补丁是什么时候发布的？",
    "specimen.summaryLabel": "PURIFY 注记",
    "specimen.summary": "两个来源对发布日期说法不一。每种陈述都与其来源和快照保持关联；在核验完成之前，谁也不会被自动当作结论。",
    "specimen.source": "来源",
    "specimen.observed": "观察时间",
    "specimen.state": "状态",
    "specimen.current": "当前",
    "specimen.conflict": "保留冲突",
    "specimen.changed": "已变化",
    "stats.title": "为被检验而生。",
    "stats.l1": "抓取引擎",
    "stats.l2": "索引语言",
    "stats.l3": "答案留有快照",
    "stats.l4": "每条答案一张回执",
    "vision2.eyebrow": "路线图",
    "vision2.title": "从答案，到活的事实层。",
    "vision2.sub": "回执沉淀为记忆；记忆学会察觉变化、修复断裂，并让每一次修复都可被检查。搜索，只是第一层。",
    "cta.title": "开始在可核验的答案上构建。",
    "cta.sub": "一次调用 /search，每个响应都带证据。",
    "cta.primary": "试用 Purify",
    "cta.secondary": "阅读文档",
    "footer.note": "让每一次搜索，都能出示自己的证据。",
    "footer.product": "产品",
    "footer.perspective": "观点",
    "footer.company": "公司",
    "footer.contact": "联系",
    "footer.privacy": "隐私",
    "footer.legal": "法律",
    "footer.made": "为一个开放且持续变化的世界而构建。"
  }
};


const clamp = (value, min = 0, max = 1) => Math.min(max, Math.max(min, value));
const lerp = (from, to, amount) => from + (to - from) * amount;
const smoothstep = (from, to, value) => {
  const progress = clamp((value - from) / (to - from));
  return progress * progress * (3 - 2 * progress);
};
const easeOut = (value) => 1 - Math.pow(1 - clamp(value), 3);
const cssColor = (token, fallback) =>
  getComputedStyle(document.documentElement).getPropertyValue(token).trim() || fallback;

let locale = "en";
let activeTheme = document.documentElement.dataset.theme === "dark" ? "dark" : "light";
let hasManualTheme = false;

function seededRandom(seed) {
  let state = seed >>> 0;
  return function random() {
    state = (state * 1664525 + 1013904223) >>> 0;
    return state / 4294967296;
  };
}

function getMotionPreferences({ ignoreVisualLock = false } = {}) {
  const connection = navigator.connection || navigator.mozConnection || navigator.webkitConnection;
  const reduced =
    CAPTURE_MODE ||
    (!ignoreVisualLock && VISUAL_LOCK_STATIC) ||
    window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  const constrained = Boolean(
    connection?.saveData ||
      (navigator.deviceMemory && navigator.deviceMemory <= 4) ||
      (navigator.hardwareConcurrency && navigator.hardwareConcurrency <= 4) ||
      window.innerWidth < 768
  );
  return { reduced, constrained };
}

class HeroMatrix {
  constructor(stage, hero, preferences) {
    this.stage = stage;
    this.hero = hero;
    this.preferences = preferences;
    this.canvas = document.createElement("canvas");
    this.canvas.className = "motion-canvas hero-canvas";
    this.canvas.setAttribute("aria-hidden", "true");
    this.context = this.canvas.getContext("2d", { alpha: true });
    this.blocks = [];
    this.width = 0;
    this.height = 0;
    this.elapsed = preferences.reduced ? 9720 : 0;
    this.lastFrame = 0;
    this.frameRequest = 0;
    this.visible = true;
    this.pageVisible = !document.hidden;
    this.scrollProgress = 0;

    this.stage.replaceChildren(this.canvas);
    this.resizeObserver = new ResizeObserver(() => this.resize());
    this.resizeObserver.observe(this.stage);
    this.visibilityObserver = new IntersectionObserver(
      ([entry]) => {
        this.visible = entry.intersectionRatio >= 0.15;
        this.syncPlayback();
      },
      { threshold: [0, 0.15, 0.6] }
    );
    this.visibilityObserver.observe(this.hero);

    this.onScroll = this.onScroll.bind(this);
    window.addEventListener("scroll", this.onScroll, { passive: true });
    this.resize();
    this.onScroll();
    this.syncPlayback();
  }

  setPreferences(preferences) {
    this.preferences = preferences;
    if (preferences.reduced) this.elapsed = 9720;
    this.buildBlocks();
    this.draw();
    this.syncPlayback();
  }

  refreshTheme() {
    this.buildBlocks();
    this.draw();
  }

  setPageVisible(visible) {
    this.pageVisible = visible;
    this.syncPlayback();
  }

  resize() {
    const rect = this.stage.getBoundingClientRect();
    const nextWidth = Math.max(1, Math.round(rect.width));
    const nextHeight = Math.max(1, Math.round(rect.height));
    if (nextWidth === this.width && nextHeight === this.height) return;

    this.width = nextWidth;
    this.height = nextHeight;
    const dprLimit = this.width < 768 ? 1.25 : 1.75;
    this.dpr = Math.min(window.devicePixelRatio || 1, dprLimit);
    this.canvas.width = Math.round(this.width * this.dpr);
    this.canvas.height = Math.round(this.height * this.dpr);
    this.canvas.style.width = `${this.width}px`;
    this.canvas.style.height = `${this.height}px`;
    this.buildBlocks();
    this.draw();
  }

  buildBlocks() {
    const random = seededRandom(130853);
    const mobile = this.width < 768;
    const blueStage = HERO_VARIANT === "b";
    const columns = mobile
      ? 52
      : blueStage
        ? this.preferences.constrained
          ? 112
          : Math.min(172, Math.max(136, Math.floor(this.width / 9)))
        : this.preferences.constrained
          ? 82
          : Math.min(118, Math.max(96, Math.floor(this.width / 13.5)));
    const rows = mobile
      ? 26
      : blueStage
        ? this.preferences.constrained
          ? 46
          : Math.min(64, Math.max(50, Math.floor(this.height / 12)))
        : this.preferences.constrained
          ? 30
          : 34;
    const tones = blueStage
      ? [
          cssColor("--color-data-highlight", "#eaf7f9"),
          cssColor("--color-data-quiet", "#d4eff4"),
          cssColor("--color-data-muted", "#a9dce7"),
          cssColor("--color-data-default", "#79c2d3")
        ]
      : ["#a8d2dc", "#78b6c8", "#4c96b3", "#287995", "#105975", "#063f59"];
    this.blocks = [];

    for (let row = 0; row < rows; row += 1) {
      for (let column = 0; column < columns; column += 1) {
        const wave = (Math.sin(column * 0.51 + row * 0.69) + 1) * 0.5;
        const rowProgress = row / Math.max(1, rows - 1);
        const density = blueStage
          ? clamp(0.94 - rowProgress * 0.08 + wave * 0.05, 0.8, 0.97)
          : clamp(0.92 - rowProgress * 0.24 + wave * 0.07, 0.58, 0.96);
        if (random() > density) continue;

        const anchor = random() > (blueStage ? 0.97 : 0.93);
        const baseSize = mobile ? 3 : blueStage ? 2.1 : 3.4;
        const size = anchor
          ? baseSize + (blueStage ? 3 : 5) + random() * (blueStage ? 4 : 5.4)
          : baseSize + random() * (blueStage ? 2.4 : 3.8);
        const x = (column / Math.max(1, columns - 1)) * this.width + (random() - 0.5) * 8;
        const y = blueStage
          ? rowProgress * this.height * 1.025 - this.height * 0.006 + (random() - 0.5) * 8
          : rowProgress * this.height * 0.96 + (random() - 0.5) * 8;
        const verticalFade = 1 - Math.pow(rowProgress, 1.28);
        const alignmentEligible = random() > 0.82 && y < this.height * 0.84;
        const alignmentY = this.height * (0.34 + Math.sin((x / this.width) * Math.PI * 2.2) * 0.045) + (random() - 0.5) * 64;
        const toneRoll = random();
        const toneIndex = blueStage
          ? toneRoll < 0.12
            ? 0
            : toneRoll < 0.38
              ? 1
              : toneRoll < 0.74
                ? 2
                : 3
          : Math.min(tones.length - 1, Math.floor(Math.pow(toneRoll, 0.78) * tones.length));
        const baseOpacity = blueStage
          ? (0.22 + random() * 0.56) * (0.7 + verticalFade * 0.3)
          : (0.34 + random() * 0.62) * (0.46 + verticalFade * 0.54);

        this.blocks.push({
          x,
          y,
          size,
          anchor,
          color: tones[toneIndex],
          opacity: Math.min(1, baseOpacity * (anchor ? 1.08 : 1)),
          rotation: (random() - 0.5) * 0.052,
          phase: random() * Math.PI * 2,
          spark: random(),
          layer: Math.floor(random() * 3),
          band: Math.min(11, Math.floor((column / columns) * 8 + (row / rows) * 4)),
          drift: random() > 0.54,
          orbitX: 0.9 + random() * 4.8,
          orbitY: 0.55 + random() * 3.35,
          motionSpeedX: 0.34 + random() * 1.31,
          motionSpeedY: 0.28 + random() * 1.17,
          detailSpeed: 1.15 + random() * 2.7,
          wander: 0.35 + random() * 1.9,
          flashSpeed: 0.24 + random() * 0.5,
          flashPhase: random() * Math.PI * 2,
          starSeed: random(),
          starEligible: false,
          rowProgress,
          alignmentEligible,
          alignmentY
        });
      }
    }

    if (blueStage) {
      // Exact caps keep the star language rare at every breakpoint. Candidates
      // are sampled from the full field; only edge clipping is prevented.
      const starLimit = mobile ? 4 : this.preferences.constrained ? 6 : 9;
      const starCycle = mobile ? 17 : 20.5;
      const starCandidates = this.blocks
        .filter((block) => {
          const safelyInsideStage =
            block.x > this.width * 0.04 &&
            block.x < this.width * 0.96 &&
            block.y > this.height * 0.06 &&
            block.y < this.height * 0.92;
          return !block.anchor && safelyInsideStage;
        })
        .sort((a, b) => b.starSeed - a.starSeed)
        .slice(0, starLimit);

      starCandidates.forEach((block, index) => {
        block.starEligible = true;
        block.starPeriod = starCycle;
        block.starDuration = 1.7 + (index % 3) * 0.08;
        block.starOffset =
          (index * (starCycle / starLimit) + (block.starSeed - 0.5) * 0.24 + starCycle) % starCycle;
      });
    }
  }

  getOpticalStrength(x, y) {
    if (HERO_VARIANT !== "b") return 0;
    const mobile = this.width < 768;
    const centerX = this.width * 0.5;
    const centerY = this.height * 1.035;
    const radiusX = this.width * (mobile ? 0.27 : 0.22);
    const radiusY = this.height * (mobile ? 0.22 : 0.24);
    const dx = (x - centerX) / radiusX;
    const dy = (y - centerY) / radiusY;
    return 1 - smoothstep(0.08, 1, Math.hypot(dx, dy));
  }

  drawEllipticalGlow(context, {
    centerX,
    centerY,
    radiusX,
    radiusY,
    stops
  }) {
    context.save();
    context.translate(centerX, centerY);
    context.scale(1, radiusY / radiusX);
    const gradient = context.createRadialGradient(0, 0, 0, 0, 0, radiusX);
    stops.forEach(([offset, color]) => gradient.addColorStop(offset, color));
    context.fillStyle = gradient;
    context.fillRect(-radiusX, -radiusX, radiusX * 2, radiusX * 2);
    context.restore();
  }

  drawOpticalBloom(context, motionSeconds, exitFade) {
    if (HERO_VARIANT !== "b" || exitFade <= 0.002) return;
    const mobile = this.width < 768;
    const breath = this.preferences.reduced ? 1 : 0.97 + Math.sin(motionSeconds * 0.42) * 0.03;

    context.save();
    context.globalCompositeOperation = "screen";
    context.globalAlpha = exitFade;

    // A stage-sized fluorescent field: the surface itself carries light,
    // rather than making every photon flash at once.
    this.drawEllipticalGlow(context, {
      centerX: this.width * 0.5,
      centerY: this.height * 0.56,
      radiusX: this.width * (mobile ? 0.72 : 0.68),
      radiusY: this.height * (mobile ? 0.9 : 0.86),
      stops: [
        [0, `rgba(122, 170, 255, ${0.2 * breath})`],
        [0.46, `rgba(67, 129, 255, ${0.14 * breath})`],
        [0.76, `rgba(23, 101, 255, ${0.06 * breath})`],
        [1, "rgba(23, 101, 255, 0)"]
      ]
    });

    this.drawEllipticalGlow(context, {
      centerX: this.width * 0.5,
      centerY: this.height * 1.07,
      radiusX: this.width * (mobile ? 0.45 : 0.4),
      radiusY: this.height * (mobile ? 0.34 : 0.37),
      stops: [
        [0, `rgba(184, 220, 255, ${0.46 * breath})`],
        [0.34, `rgba(122, 170, 255, ${0.32 * breath})`],
        [0.68, `rgba(67, 129, 255, ${0.14 * breath})`],
        [1, "rgba(23, 101, 255, 0)"]
      ]
    });

    this.drawEllipticalGlow(context, {
      centerX: this.width * 0.5,
      centerY: this.height * 1.035,
      radiusX: this.width * (mobile ? 0.28 : 0.235),
      radiusY: this.height * (mobile ? 0.22 : 0.24),
      stops: [
        [0, `rgba(252, 254, 255, ${0.99 * breath})`],
        [0.14, `rgba(232, 246, 255, ${0.9 * breath})`],
        [0.34, `rgba(174, 215, 255, ${0.68 * breath})`],
        [0.64, `rgba(82, 157, 255, ${0.3 * breath})`],
        [1, "rgba(23, 101, 255, 0)"]
      ]
    });

    context.restore();
  }

  getStarPulse(block, motionSeconds) {
    if (!block.starEligible || this.preferences.reduced) return 0;
    const localTime = (motionSeconds + block.starOffset) % block.starPeriod;
    if (localTime >= block.starDuration) return 0;
    const progress = localTime / block.starDuration;
    if (progress < 0.28) return smoothstep(0, 0.28, progress);
    if (progress < 0.68) return 1;
    return 1 - smoothstep(0.68, 1, progress);
  }

  drawFourPointStar(context, radius) {
    const inner = radius * 0.16;
    context.beginPath();
    context.moveTo(0, -radius);
    context.lineTo(inner, -inner);
    context.lineTo(radius, 0);
    context.lineTo(inner, inner);
    context.lineTo(0, radius);
    context.lineTo(-inner, inner);
    context.lineTo(-radius, 0);
    context.lineTo(-inner, -inner);
    context.closePath();
    context.fill();
  }

  onScroll() {
    const rect = this.hero.getBoundingClientRect();
    const distance = Math.max(window.innerHeight * 0.72, this.hero.offsetHeight * 0.78);
    this.scrollProgress = VISUAL_LOCK_STATIC ? 0 : clamp(-rect.top / distance);
    const copyProgress = smoothstep(0.45, 0.72, this.scrollProgress);
    this.hero.style.setProperty("--hero-copy-y", `${(-18 * copyProgress).toFixed(2)}px`);
    this.hero.style.setProperty("--hero-copy-opacity", `${(1 - 0.28 * copyProgress).toFixed(3)}`);
    if (this.preferences.reduced && this.visible) this.draw();
  }

  syncPlayback() {
    const shouldRun = this.visible && this.pageVisible && !this.preferences.reduced;
    if (shouldRun && !this.frameRequest) {
      this.lastFrame = performance.now();
      this.frameRequest = requestAnimationFrame((time) => this.frame(time));
    } else if (!shouldRun && this.frameRequest) {
      cancelAnimationFrame(this.frameRequest);
      this.frameRequest = 0;
    }
    if (!shouldRun) this.draw();
  }

  frame(time) {
    this.frameRequest = 0;
    const frameCap = this.preferences.constrained ? 1000 / 30 : 0;
    const delta = Math.min(50, time - this.lastFrame);
    if (!frameCap || delta >= frameCap - 1) {
      this.elapsed += delta;
      this.lastFrame = time;
      this.draw();
    }
    if (this.visible && this.pageVisible && !this.preferences.reduced) {
      this.frameRequest = requestAnimationFrame((nextTime) => this.frame(nextTime));
    }
  }

  draw() {
    if (!this.context || !this.width || !this.height) return;
    const context = this.context;
    const blueStage = HERO_VARIANT === "b";
    context.setTransform(1, 0, 0, 1, 0, 0);
    context.clearRect(0, 0, this.canvas.width, this.canvas.height);
    context.scale(this.dpr, this.dpr);

    const loopDuration = 14000;
    const loopTime = this.preferences.reduced ? 7400 : Math.max(0, this.elapsed - 920) % loopDuration;
    const loopSeconds = loopTime / 1000;
    const motionSeconds = this.preferences.reduced ? 7.4 : Math.max(0, this.elapsed - 920) / 1000;
    const alignment =
      loopSeconds < 4.8
        ? 0
        : loopSeconds < 6.8
          ? smoothstep(4.8, 6.8, loopSeconds)
          : loopSeconds < 7.8
            ? 1
            : loopSeconds < 11.2
              ? 1 - smoothstep(7.8, 11.2, loopSeconds)
              : 0;
    const signalRise = smoothstep(1.4, 4.8, loopSeconds) * (1 - smoothstep(7.8, 11.6, loopSeconds));
    const seamEnvelope = Math.pow(Math.sin((loopTime / loopDuration) * Math.PI), 2);
    const loopWeight = this.preferences.reduced ? 0 : 1 - smoothstep(0.05, 0.22, this.scrollProgress);
    const alignmentMotionWeight = this.preferences.reduced ? 1 : loopWeight;
    const exitWeight = smoothstep(0.55, 0.88, this.scrollProgress);
    const exitFade = 1 - smoothstep(0.62, 1, this.scrollProgress);

    // The optical core lives inside the matrix canvas. Nearby photons inherit
    // its brightness below, so the light reads as part of the data field.
    this.drawOpticalBloom(context, motionSeconds, exitFade);

    const activeStars = [];
    this.blocks.forEach((block) => {
      const entryStart = 80 + block.band * 22;
      const entry = this.preferences.reduced ? 1 : easeOut((this.elapsed - entryStart) / 500);
      if (entry <= 0) return;

      let x;
      let y;
      let rotation;
      let shimmer = 1;
      let localScale = 1;
      let starPulse = 0;

      if (blueStage) {
        // Photons wander on independent, non-looping paths. Only a tiny subset
        // emits a narrow flash; the field never travels as a synchronized group.
        const motionWeight = this.preferences.reduced ? 0 : loopWeight;
        const horizontalTremor =
          Math.sin(motionSeconds * block.motionSpeedX + block.phase) * block.orbitX +
          Math.sin(motionSeconds * block.detailSpeed + block.phase * 2.17) * block.orbitX * 0.28 +
          Math.sin(motionSeconds * 0.17 + block.phase * 0.43) * block.wander;
        const verticalTremor =
          Math.cos(motionSeconds * block.motionSpeedY + block.phase * 1.31) * block.orbitY +
          Math.sin(motionSeconds * (block.detailSpeed * 0.81) + block.phase * 0.71) * block.orbitY * 0.34 +
          Math.cos(motionSeconds * 0.13 + block.phase * 0.57) * block.wander * 0.72;
        const shimmerWave = (Math.sin(motionSeconds * (0.35 + block.motionSpeedY * 0.31) + block.phase * 0.81) + 1) * 0.5;
        const flashWave = block.spark > 0.9985
          ? Math.pow(Math.max(0, Math.sin(motionSeconds * block.flashSpeed + block.flashPhase)), 22)
          : 0;
        starPulse = this.getStarPulse(block, motionSeconds) * motionWeight;
        x = block.x + horizontalTremor * motionWeight;
        y = block.y + verticalTremor * motionWeight;
        rotation = block.rotation + Math.sin(motionSeconds * (0.42 + block.motionSpeedX * 0.19) + block.phase) * 0.022 * motionWeight;
        shimmer = 1 - motionWeight * 0.06 + shimmerWave * 0.1 * motionWeight + flashWave * 0.9 * motionWeight;
        localScale = 1 + flashWave * (block.anchor ? 0.58 : 0.42) * motionWeight;
      } else {
        const driftAmplitude = (block.drift ? 2.6 + signalRise * 7.2 : 0.7 + signalRise * 1.6) * seamEnvelope * loopWeight;
        const flow = Math.sin(loopSeconds * 0.82 + block.rowProgress * Math.PI * 3.2 + block.phase * 0.22);
        x = block.x + Math.cos(loopSeconds * 0.82 + block.phase) * driftAmplitude + flow * signalRise * 1.8;
        y = block.y + Math.sin(loopSeconds * 0.66 + block.phase * 1.17) * driftAmplitude * 0.76;
        rotation = block.rotation + Math.sin(loopSeconds * 0.52 + block.phase) * 0.027 * signalRise * loopWeight;

        if (block.alignmentEligible) {
          const alignmentWeight = alignment * 0.78 * alignmentMotionWeight;
          y = lerp(y, block.alignmentY, alignmentWeight);
          rotation = lerp(rotation, 0, alignmentWeight);
        }
      }

      y -= [8, 14, 22][block.layer] * exitWeight;
      const opticalStrength = blueStage ? this.getOpticalStrength(x, y) : 0;
      const opticalPulse = this.preferences.reduced
        ? 1
        : 0.94 + Math.sin(motionSeconds * 0.67 + block.phase * 0.46) * 0.06;
      localScale *= 1 + opticalStrength * (block.anchor ? 0.42 : 0.26) * opticalPulse;
      const opacity = Math.min(
        1,
        block.opacity * entry * exitFade * shimmer + opticalStrength * 0.34 * entry * exitFade
      );
      if (opacity <= 0.002) return;

      context.save();
      context.globalAlpha = opacity;
      context.translate(x + block.size / 2, y + block.size / 2);
      context.rotate(rotation);
      const opticalSpark = blueStage && opticalStrength > 0.38 && (block.anchor || block.spark > 0.9985);
      if ((block.anchor || opticalSpark) && !this.preferences.constrained) {
        context.shadowColor = opticalSpark
          ? `rgba(218, 240, 255, ${0.52 + opticalStrength * 0.34})`
          : blueStage
            ? "rgba(190, 225, 255, 0.46)"
            : "rgba(35, 104, 129, 0.18)";
        context.shadowBlur = opticalSpark ? 10 + opticalStrength * 9 : blueStage ? 9 : 6;
        context.shadowOffsetY = 2;
      }
      const renderedSize = block.size * localScale;
      const squareOpacity = opacity * (1 - starPulse);
      context.globalAlpha = squareOpacity;
      context.fillStyle = block.color;
      context.fillRect(-renderedSize / 2, -renderedSize / 2, renderedSize, renderedSize);
      if (opticalStrength > 0.025) {
        context.shadowColor = "transparent";
        context.globalCompositeOperation = "screen";
        context.globalAlpha = Math.min(1, squareOpacity * opticalStrength * (0.72 + opticalPulse * 0.18));
        context.fillStyle = opticalStrength > 0.58 ? "#f7fcff" : "#c4e4ff";
        context.fillRect(-renderedSize / 2, -renderedSize / 2, renderedSize, renderedSize);
        context.globalCompositeOperation = "source-over";
        context.globalAlpha = squareOpacity;
      }
      if (renderedSize > 7) {
        context.shadowColor = "transparent";
        context.fillStyle = "rgba(255,255,255,0.34)";
        context.fillRect(-renderedSize / 2 + 1, -renderedSize / 2 + 1, Math.max(1, renderedSize - 2), 1);
      }
      if (starPulse > 0.002) {
        const starRadius =
          Math.max(this.width < 768 ? 8.5 : 11, renderedSize * 1.7) * (0.78 + starPulse * 0.22);
        const starOpacity = Math.min(
          1,
          (0.84 + block.opacity * 0.16) * entry * exitFade * starPulse
        );
        activeStars.push({
          x: x + block.size / 2,
          y: y + block.size / 2,
          rotation,
          radius: starRadius,
          opacity: starOpacity,
          pulse: starPulse
        });
      }
      context.restore();
    });

    // Stars are composited after the whole photon field so later squares can
    // never cover them. The scheduler guarantees this list has at most one item.
    activeStars.forEach((star) => {
      context.save();
      context.translate(star.x, star.y);
      context.rotate(star.rotation);
      context.globalCompositeOperation = "source-over";
      context.globalAlpha = star.opacity;
      context.shadowColor = `rgba(255, 255, 255, ${0.6 + star.pulse * 0.34})`;
      context.shadowBlur = 14 + star.pulse * 16;
      context.shadowOffsetY = 0;
      context.fillStyle = "#ffffff";
      this.drawFourPointStar(context, star.radius);

      context.shadowColor = "transparent";
      context.globalAlpha = star.opacity * 0.46;
      context.strokeStyle = "rgba(255, 255, 255, 0.96)";
      context.lineWidth = 0.9;
      context.beginPath();
      context.moveTo(0, -star.radius * 1.42);
      context.lineTo(0, star.radius * 1.42);
      context.moveTo(-star.radius * 1.42, 0);
      context.lineTo(star.radius * 1.42, 0);
      context.stroke();
      context.restore();
    });
  }
}

class MemoryField {
  constructor(stage, field, preferences) {
    this.stage = stage;
    this.field = field;
    this.preferences = preferences;
    this.canvas = document.createElement("canvas");
    this.canvas.className = "motion-canvas memory-canvas";
    this.canvas.setAttribute("aria-hidden", "true");
    this.context = this.canvas.getContext("2d", { alpha: true });
    this.points = [];
    this.width = 0;
    this.height = 0;
    this.elapsed = 0;
    this.lastFrame = 0;
    this.frameRequest = 0;
    this.started = false;
    this.settled = false;
    this.visible = false;
    this.pageVisible = !document.hidden;
    this.state = "idle";

    this.stage.replaceChildren(this.canvas);
    this.resizeObserver = new ResizeObserver(() => this.resize());
    this.resizeObserver.observe(this.stage);
    this.visibilityObserver = new IntersectionObserver(
      ([entry]) => {
        this.visible = entry.intersectionRatio >= 0.15;
        const trigger = window.innerWidth < 768 ? 0.2 : 0.35;
        if (!this.started && entry.intersectionRatio >= trigger) this.start();
        this.syncPlayback();
      },
      { threshold: [0, 0.15, 0.2, 0.35, 0.6] }
    );
    this.visibilityObserver.observe(this.field);
    this.resize();
  }

  setPreferences(preferences) {
    this.preferences = preferences;
    if (preferences.reduced) {
      this.started = true;
      this.settled = true;
      this.setState("settled");
    }
    this.buildPoints();
    this.draw();
    this.syncPlayback();
  }

  setPageVisible(visible) {
    this.pageVisible = visible;
    this.syncPlayback();
  }

  resize() {
    const rect = this.stage.getBoundingClientRect();
    const nextWidth = Math.max(1, Math.round(rect.width));
    const nextHeight = Math.max(1, Math.round(rect.height));
    if (nextWidth === this.width && nextHeight === this.height) return;
    this.width = nextWidth;
    this.height = nextHeight;
    const dprLimit = this.width < 768 ? 1.25 : 1.75;
    this.dpr = Math.min(window.devicePixelRatio || 1, dprLimit);
    this.canvas.width = Math.round(this.width * this.dpr);
    this.canvas.height = Math.round(this.height * this.dpr);
    this.canvas.style.width = `${this.width}px`;
    this.canvas.style.height = `${this.height}px`;
    this.buildPoints();
    this.draw();
  }

  buildPoints() {
    const random = seededRandom(812721);
    const sourcePoints = Array.isArray(window.PURIFY_MEMORY_POINTS) ? window.PURIFY_MEMORY_POINTS : [];
    if (!sourcePoints.length) {
      this.points = [];
      return;
    }

    const stride = this.width < 520 ? 4 : this.preferences.constrained ? 2 : 1;
    const sourceAspect = 4 / 5;
    let displayWidth;
    let displayHeight;
    let offsetX;
    let offsetY;
    if (this.width / this.height > sourceAspect) {
      displayHeight = this.height * 0.96;
      displayWidth = displayHeight * sourceAspect;
      offsetX = (this.width - displayWidth) / 2;
      offsetY = this.height * 0.02;
    } else {
      displayWidth = this.width * 0.96;
      displayHeight = displayWidth / sourceAspect;
      offsetX = this.width * 0.02;
      offsetY = (this.height - displayHeight) / 2;
    }
    const pointScale = clamp(displayWidth / 360, 0.72, 1.45);

    this.points = sourcePoints.filter((_, index) => index % stride === 0).map((sourcePoint, index) => {
      const [normalizedX, normalizedY, sourceSize, sourceOpacity, edge] = sourcePoint;
      return {
        index,
        scatterX: (0.08 + random() * 0.86) * this.width,
        scatterY: (0.08 + random() * 0.84) * this.height,
        shapeX: offsetX + normalizedX * displayWidth,
        shapeY: offsetY + normalizedY * displayHeight,
        size: sourceSize * pointScale,
        opacity: sourceOpacity,
        phase: random() * Math.PI * 2,
        kind: edge ? "feature" : "body"
      };
    });
  }

  start() {
    this.started = true;
    if (this.preferences.reduced) {
      this.settled = true;
      this.setState("settled");
      this.draw();
      return;
    }
    this.syncPlayback();
  }

  setState(nextState) {
    if (nextState === this.state) return;
    this.state = nextState;
    this.field.dataset.memoryState = nextState;
  }

  syncPlayback() {
    const shouldRun = this.started && !this.settled && this.visible && this.pageVisible && !this.preferences.reduced;
    if (shouldRun && !this.frameRequest) {
      this.lastFrame = performance.now();
      this.frameRequest = requestAnimationFrame((time) => this.frame(time));
    } else if (!shouldRun && this.frameRequest) {
      cancelAnimationFrame(this.frameRequest);
      this.frameRequest = 0;
    }
    if (!shouldRun) this.draw();
  }

  frame(time) {
    this.frameRequest = 0;
    const frameCap = this.preferences.constrained ? 1000 / 30 : 0;
    const delta = Math.min(50, time - this.lastFrame);
    if (!frameCap || delta >= frameCap - 1) {
      this.elapsed += delta;
      this.lastFrame = time;
      this.draw();
    }
    if (this.elapsed >= 5100) {
      this.settled = true;
      this.setState("settled");
      this.draw();
      return;
    }
    if (this.visible && this.pageVisible) {
      this.frameRequest = requestAnimationFrame((nextTime) => this.frame(nextTime));
    }
  }

  draw() {
    if (!this.context || !this.width || !this.height) return;
    const context = this.context;
    context.setTransform(1, 0, 0, 1, 0, 0);
    context.clearRect(0, 0, this.canvas.width, this.canvas.height);
    if (this.preferences.reduced) return;
    context.scale(this.dpr, this.dpr);

    const elapsed = this.elapsed;
    let state = "idle";
    if (this.settled) state = "settled";
    else if (elapsed >= 4200) state = "human";
    else if (elapsed >= 2700) state = "resolving";
    else if (elapsed >= 1800) state = "dense";
    else if (elapsed >= 300) state = "forming";
    this.setState(state);

    this.points.forEach((point) => {
      let x = point.scatterX;
      let y = point.scatterY;
      let opacity = point.opacity;

      if (state === "idle") {
        if (point.index % 9 !== 0) return;
        opacity *= 0.42;
      } else if (state === "forming") {
        const progress = easeOut((elapsed - 300) / 1500);
        const stagger = clamp((progress * 1.18 - (point.index % 17) / 92));
        x = lerp(point.scatterX, point.shapeX, stagger);
        y = lerp(point.scatterY, point.shapeY, stagger);
        opacity *= 0.32 + stagger * 0.68;
      } else if (state === "dense") {
        const pulse = Math.sin((elapsed - 1800) / 180 + point.phase) * 1.5;
        x = point.shapeX + Math.cos(point.phase) * pulse;
        y = point.shapeY + Math.sin(point.phase) * pulse;
        opacity *= 0.94 + Math.sin(point.phase + elapsed / 240) * 0.06;
      } else if (state === "resolving") {
        const progress = smoothstep(2700, 4200, elapsed);
        const residualMotion = (1 - progress) * 4;
        x = point.shapeX + Math.cos(point.phase * 1.7) * residualMotion;
        y = point.shapeY + Math.sin(point.phase * 1.3) * residualMotion;
        opacity *= 0.94 + progress * 0.06;
      } else {
        x = point.shapeX;
        y = point.shapeY;
        opacity *= point.kind === "feature" ? 1 : 0.96;
      }

      if (opacity <= 0.005) return;
      context.beginPath();
      context.globalAlpha = opacity;
      context.fillStyle = point.kind === "feature" ? "#2447c6" : point.kind === "outline" ? "#3159d9" : "#5f78e7";
      context.arc(x, y, point.size / 2, 0, Math.PI * 2);
      context.fill();
    });
    context.globalAlpha = 1;
  }
}

function readStoredTheme() {
  try {
    const stored = window.localStorage.getItem(THEME_STORAGE_KEY);
    return stored === "dark" || stored === "light" ? stored : null;
  } catch (_) {
    return null;
  }
}

function syncThemeControl() {
  const toggle = document.querySelector("[data-theme-toggle]");
  const label = activeTheme === "dark" ? copy[locale]["theme.light"] : copy[locale]["theme.dark"];
  if (!toggle) return;
  toggle.setAttribute("aria-label", label);
  toggle.setAttribute("aria-pressed", String(activeTheme === "dark"));
  const hiddenLabel = toggle.querySelector("[data-theme-label]");
  if (hiddenLabel) hiddenLabel.textContent = label;
}

function applyTheme(nextTheme, { persist = false } = {}) {
  activeTheme = nextTheme === "dark" ? "dark" : "light";
  document.documentElement.dataset.theme = activeTheme;
  document.documentElement.style.colorScheme = activeTheme;
  const themeColor = document.querySelector('meta[name="theme-color"]');
  if (themeColor) themeColor.content = THEME_COLORS[activeTheme];

  if (persist) {
    hasManualTheme = true;
    try {
      window.localStorage.setItem(THEME_STORAGE_KEY, activeTheme);
    } catch (_) {
      // File previews and privacy modes may make storage unavailable.
    }
  }

  syncThemeControl();
  document.dispatchEvent(new CustomEvent("purify:theme-change", { detail: { theme: activeTheme } }));
}

function setupTheme() {
  const storedTheme = readStoredTheme();
  hasManualTheme = Boolean(storedTheme);
  applyTheme(storedTheme || document.documentElement.dataset.theme || (SYSTEM_THEME_MEDIA.matches ? "dark" : "light"));

  document.querySelector("[data-theme-toggle]")?.addEventListener("click", () => {
    applyTheme(activeTheme === "dark" ? "light" : "dark", { persist: true });
  });

  SYSTEM_THEME_MEDIA.addEventListener("change", (event) => {
    if (!hasManualTheme) applyTheme(event.matches ? "dark" : "light");
  });
}

function applyLocale(nextLocale) {
  locale = nextLocale;
  const lang = nextLocale === "zh" ? "zh-CN" : "en";
  document.documentElement.lang = lang;

  document.querySelectorAll("[data-copy]").forEach((element) => {
    const value = copy[nextLocale][element.dataset.copy];
    if (value) element.innerHTML = value;
  });

  document.querySelectorAll("[data-alt-en]").forEach((image) => {
    image.alt = nextLocale === "zh" ? image.dataset.altZh : image.dataset.altEn;
  });

  document.querySelectorAll("[data-auth-link]").forEach((link) => {
    const params = new URLSearchParams();
    if (link.dataset.authLink === "signup") params.set("mode", "signup");
    if (nextLocale === "zh") params.set("lang", "zh");
    link.href = `./login.html${params.size ? `?${params}` : ""}`;
  });

  const toggle = document.querySelector("[data-language-toggle]");
  if (toggle) {
    toggle.querySelector(".language-current").textContent = nextLocale === "zh" ? "中文" : "EN";
    toggle.querySelector(".language-next").textContent = nextLocale === "zh" ? "EN" : "中文";
    toggle.setAttribute("aria-label", nextLocale === "zh" ? "Switch to English" : "切换到中文");
  }

  document.title =
    nextLocale === "zh"
      ? "Purify Search — 搜索，是智能理解世界的方式。"
      : "Purify Search — Search is how intelligence meets the world.";
  document.querySelector('meta[name="description"]').content =
    nextLocale === "zh"
      ? "Purify Search 是一个会抓、会找、会核的搜索 API——每一条答案都附带快照、来源与签名回执。"
      : "Purify Search is a search API that fetches, finds, and verifies the open web — every answer ships with a snapshot, sources, and a signed receipt.";

  syncThemeControl();
}

function setupMenu() {
  const toggle = document.querySelector("[data-menu-toggle]");
  const menu = document.querySelector("[data-mobile-menu]");
  if (!toggle || !menu) return;

  function closeMenu({ restoreFocus = false } = {}) {
    const wasOpen = toggle.getAttribute("aria-expanded") === "true";
    toggle.setAttribute("aria-expanded", "false");
    menu.hidden = true;
    document.body.classList.remove("menu-open");
    document.dispatchEvent(new CustomEvent("purify:menu-state", { detail: { open: false } }));
    if (wasOpen && restoreFocus && toggle.offsetParent !== null) toggle.focus();
  }

  toggle.addEventListener("click", () => {
    const open = toggle.getAttribute("aria-expanded") === "true";
    if (open) {
      closeMenu({ restoreFocus: true });
      return;
    }
    toggle.setAttribute("aria-expanded", String(!open));
    menu.hidden = open;
    document.body.classList.toggle("menu-open", !open);
    document.dispatchEvent(new CustomEvent("purify:menu-state", { detail: { open: !open } }));
    requestAnimationFrame(() => menu.querySelector("a")?.focus());
  });

  menu.querySelectorAll("a").forEach((link) => link.addEventListener("click", closeMenu));
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape" && toggle.getAttribute("aria-expanded") === "true") {
      closeMenu({ restoreFocus: true });
    }
  });
  menu.addEventListener("keydown", (event) => {
    if (event.key !== "Tab") return;
    const links = [...menu.querySelectorAll("a")];
    if (!links.length) return;
    const first = links[0];
    const last = links[links.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  });
  const desktopMedia = window.matchMedia("(min-width: 900px)");
  desktopMedia.addEventListener("change", (event) => {
    if (event.matches) closeMenu();
  });
}

function setupSectionReveals(preferences) {
  const revealTargets = [...document.querySelectorAll("[data-reveal]")];
  if (preferences.reduced) {
    revealTargets.forEach((target) => target.classList.add("is-visible"));
    return;
  }

  const pendingTargets = new Set(revealTargets);

  const observer = new IntersectionObserver(
    (entries) => {
      entries.forEach((entry) => {
        if (!entry.isIntersecting) return;
        const target = entry.target;
        target.classList.add("is-visible");
        pendingTargets.delete(target);
        observer.unobserve(target);
      });
    },
    { rootMargin: "0px 0px -10%", threshold: 0.14 }
  );

  revealTargets.forEach((target) => observer.observe(target));

  let scanRequest = 0;
  const revealPassedContent = () => {
    scanRequest = 0;
    pendingTargets.forEach((target) => {
      if (target.getBoundingClientRect().bottom >= 0) return;
      target.classList.add("is-visible");
      pendingTargets.delete(target);
      observer.unobserve(target);
    });
  };
  window.addEventListener(
    "scroll",
    () => {
      if (!scanRequest) scanRequest = requestAnimationFrame(revealPassedContent);
    },
    { passive: true }
  );
  requestAnimationFrame(revealPassedContent);
}

document.addEventListener("DOMContentLoaded", () => {
  let preferences = getMotionPreferences();
  let heroPreferences = getMotionPreferences({ ignoreVisualLock: HERO_MATRIX_LIVE });
  document.documentElement.classList.add("motion-enabled");
  document.documentElement.classList.toggle("motion-reduced", preferences.reduced);
  document.documentElement.classList.toggle("motion-constrained", preferences.constrained);

  setupTheme();
  setupMenu();
  setupSectionReveals(preferences);

  const heroMotion = new HeroMatrix(
    document.querySelector("[data-hero-matrix]"),
    document.querySelector("[data-hero]"),
    heroPreferences
  );
  document.addEventListener("purify:theme-change", () => {
    heroMotion.refreshTheme();
  });

  document.querySelector("[data-language-toggle]")?.addEventListener("click", () => {
    applyLocale(locale === "en" ? "zh" : "en");
  });

  document.addEventListener("visibilitychange", () => {
    heroMotion.setPageVisible(!document.hidden);
  });

  document.addEventListener("purify:menu-state", (event) => {
    heroMotion.setPageVisible(!document.hidden && !event.detail.open);
  });

  const applyPreferences = () => {
    const nextPreferences = getMotionPreferences();
    const nextHeroPreferences = getMotionPreferences({ ignoreVisualLock: HERO_MATRIX_LIVE });
    const changed =
      nextPreferences.reduced !== preferences.reduced || nextPreferences.constrained !== preferences.constrained;
    const heroChanged =
      nextHeroPreferences.reduced !== heroPreferences.reduced ||
      nextHeroPreferences.constrained !== heroPreferences.constrained;
    if (!changed && !heroChanged) return;
    preferences = nextPreferences;
    heroPreferences = nextHeroPreferences;
    document.documentElement.classList.toggle("motion-reduced", preferences.reduced);
    document.documentElement.classList.toggle("motion-constrained", preferences.constrained);
    heroMotion.setPreferences(heroPreferences);
  };

  const reducedMedia = window.matchMedia("(prefers-reduced-motion: reduce)");
  reducedMedia.addEventListener("change", applyPreferences);

  let preferenceResizeTimer;
  window.addEventListener("resize", () => {
    clearTimeout(preferenceResizeTimer);
    preferenceResizeTimer = window.setTimeout(applyPreferences, 160);
  });
});
