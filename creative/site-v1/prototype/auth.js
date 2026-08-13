const AUTH_THEME_STORAGE_KEY = "purify-theme";
const authThemeMedia = window.matchMedia("(prefers-color-scheme: dark)");
const authParams = new URLSearchParams(window.location.search);
let authMode = authParams.get("mode") === "signup" ? "signup" : "signin";
let authLocale = authParams.get("lang") === "zh" ? "zh" : "en";
let authTheme = document.documentElement.dataset.theme || (authThemeMedia.matches ? "dark" : "light");

const authCopy = {
  en: {
    "story.kicker": "Living evidence",
    "story.title": "Return to what the web<br />actually said.",
    "story.body": "Search, compare, and keep every update connected to its source.",
    "story.note": "Evidence stays inspectable as the world changes.",
    "action.back": "Back to site",
    "action.google": "Continue with Google",
    "action.github": "Continue with GitHub",
    "action.show": "Show",
    "action.hide": "Hide",
    "action.forgot": "Forgot password?",
    "form.kicker": "Your workspace",
    "form.divider": "or continue with email",
    "form.terms": "By creating an account, you agree to the Terms and Privacy Policy.",
    "field.email": "Email address",
    "field.password": "Password",
    "field.remember": "Keep me signed in",
    "footer.note": "Evidence-first search infrastructure.",
    signinTitle: "Welcome back.",
    signinSubtitle: "Sign in to continue to Purify Search.",
    signinSubmit: "Sign in",
    signinSwitch: "New to Purify?",
    signinLink: "Start free",
    signupTitle: "Start building.",
    signupSubtitle: "Create your Purify workspace and begin with inspectable evidence.",
    signupSubmit: "Create account",
    signupSwitch: "Already have an account?",
    signupLink: "Sign in",
    invalid: "Enter a valid email and a password with at least 8 characters.",
    prototype: "The page is ready. Authentication still needs to be connected to the product backend.",
    themeDark: "Switch to dark mode",
    themeLight: "Switch to light mode"
  },
  zh: {
    "story.kicker": "持续演进的证据",
    "story.title": "回到网页<br />真正说过的话。",
    "story.body": "搜索、比较，并让每一次更新都与它的来源保持连接。",
    "story.note": "世界不断变化，证据仍然可以被检查。",
    "action.back": "返回网站",
    "action.google": "使用 Google 继续",
    "action.github": "使用 GitHub 继续",
    "action.show": "显示",
    "action.hide": "隐藏",
    "action.forgot": "忘记密码？",
    "form.kicker": "你的工作区",
    "form.divider": "或使用邮箱继续",
    "form.terms": "创建账户即表示你同意服务条款与隐私政策。",
    "field.email": "邮箱地址",
    "field.password": "密码",
    "field.remember": "保持登录",
    "footer.note": "以证据为先的搜索基础设施。",
    signinTitle: "欢迎回来。",
    signinSubtitle: "登录以继续使用 Purify Search。",
    signinSubmit: "登录",
    signinSwitch: "第一次使用 Purify？",
    signinLink: "开始试用",
    signupTitle: "开始构建。",
    signupSubtitle: "创建 Purify 工作区，从可以检查的证据开始。",
    signupSubmit: "创建账户",
    signupSwitch: "已经有账户？",
    signupLink: "登录",
    invalid: "请输入有效邮箱，密码至少需要 8 个字符。",
    prototype: "页面已经准备好，认证功能仍需接入产品后端。",
    themeDark: "切换到暗黑模式",
    themeLight: "切换到明亮模式"
  }
};

function setAuthTheme(nextTheme, persist = false) {
  authTheme = nextTheme === "dark" ? "dark" : "light";
  document.documentElement.dataset.theme = authTheme;
  document.documentElement.style.colorScheme = authTheme;
  document.querySelector('meta[name="theme-color"]').content = authTheme === "dark" ? "#0a111b" : "#fcfcf9";
  const toggle = document.querySelector("[data-theme-toggle]");
  if (toggle) {
    toggle.setAttribute("aria-label", authTheme === "dark" ? authCopy[authLocale].themeLight : authCopy[authLocale].themeDark);
    toggle.querySelector("[data-theme-glyph]").textContent = authTheme === "dark" ? "☼" : "◐";
  }
  if (persist) {
    try {
      window.localStorage.setItem(AUTH_THEME_STORAGE_KEY, authTheme);
    } catch (_) {
      // Private browsing and local file previews may block storage.
    }
  }
}

function setAuthMode(nextMode, updateUrl = false) {
  authMode = nextMode === "signup" ? "signup" : "signin";
  const strings = authCopy[authLocale];
  const signup = authMode === "signup";
  document.querySelector("[data-auth-title]").textContent = signup ? strings.signupTitle : strings.signinTitle;
  document.querySelector("[data-auth-subtitle]").textContent = signup ? strings.signupSubtitle : strings.signinSubtitle;
  document.querySelector("[data-auth-submit]").textContent = signup ? strings.signupSubmit : strings.signinSubmit;
  document.querySelector("[data-auth-switch-copy]").textContent = signup ? strings.signupSwitch : strings.signinSwitch;
  const modeLink = document.querySelector("[data-mode-link]");
  modeLink.textContent = signup ? strings.signupLink : strings.signinLink;
  modeLink.href = signup ? `./login.html?lang=${authLocale}` : `./login.html?mode=signup&lang=${authLocale}`;
  document.querySelector("[data-signin-only]").hidden = signup;
  document.querySelector("[data-signup-only]").hidden = !signup;
  const password = document.querySelector('input[name="password"]');
  password.autocomplete = signup ? "new-password" : "current-password";
  document.title = `${signup ? strings.signupSubmit : strings.signinSubmit} — Purify Search`;
  document.querySelector("[data-auth-status]").textContent = "";
  if (updateUrl) {
    const params = new URLSearchParams();
    if (signup) params.set("mode", "signup");
    if (authLocale === "zh") params.set("lang", "zh");
    window.history.replaceState({}, "", `${window.location.pathname}${params.size ? `?${params}` : ""}`);
  }
}

function setAuthLocale(nextLocale, updateUrl = false) {
  authLocale = nextLocale === "zh" ? "zh" : "en";
  document.documentElement.lang = authLocale === "zh" ? "zh-CN" : "en";
  document.querySelectorAll("[data-auth-copy]").forEach((element) => {
    element.innerHTML = authCopy[authLocale][element.dataset.authCopy];
  });
  const languageToggle = document.querySelector("[data-language-toggle]");
  languageToggle.textContent = authLocale === "zh" ? "中文" : "EN";
  languageToggle.setAttribute("aria-label", authLocale === "zh" ? "Switch to English" : "切换到中文");
  setAuthMode(authMode, updateUrl);
  setAuthTheme(authTheme);
}

function reportAuthStatus(message, isError = false) {
  const status = document.querySelector("[data-auth-status]");
  status.textContent = message;
  status.classList.toggle("is-error", isError);
}

document.addEventListener("DOMContentLoaded", () => {
  setAuthLocale(authLocale);
  setAuthTheme(authTheme);

  document.querySelector("[data-theme-toggle]").addEventListener("click", () => {
    setAuthTheme(authTheme === "dark" ? "light" : "dark", true);
  });

  document.querySelector("[data-language-toggle]").addEventListener("click", () => {
    setAuthLocale(authLocale === "en" ? "zh" : "en", true);
  });

  document.querySelector("[data-mode-link]").addEventListener("click", (event) => {
    event.preventDefault();
    setAuthMode(authMode === "signin" ? "signup" : "signin", true);
  });

  document.querySelector("[data-password-toggle]").addEventListener("click", (event) => {
    const password = document.querySelector('input[name="password"]');
    const reveal = password.type === "password";
    password.type = reveal ? "text" : "password";
    event.currentTarget.textContent = authCopy[authLocale][reveal ? "action.hide" : "action.show"];
  });

  document.querySelectorAll("[data-auth-provider], [data-auth-unavailable]").forEach((button) => {
    button.addEventListener("click", () => reportAuthStatus(authCopy[authLocale].prototype));
  });

  document.querySelector("[data-auth-form]").addEventListener("submit", (event) => {
    event.preventDefault();
    const email = event.currentTarget.elements.email;
    const password = event.currentTarget.elements.password;
    if (!email.validity.valid || password.value.length < 8) {
      reportAuthStatus(authCopy[authLocale].invalid, true);
      (email.validity.valid ? password : email).focus();
      return;
    }
    reportAuthStatus(authCopy[authLocale].prototype);
    password.value = "";
  });
});
