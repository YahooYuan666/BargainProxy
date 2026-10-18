#!/usr/bin/env node
/**
 * BargainProxy → ZCode 供应商同步（独立插件，零依赖，纯 HTTP）
 *
 * 做什么：从本机运行的 BargainProxy 网关读取两批模型，在 ZCode 的个人
 * Provider 配置里维护 **9 个供应商**（3 个模型族 × 3 条协议线）：
 *
 *   BargainProxy OpenCode  Chat/Response/Anthropic  ← /panel/api/opencodezen 的可用免费模型
 *   BargainProxy 国模      Chat/Response/Anthropic  ← /v1/models 中非 antigravity 的模型
 *   BargainProxy Gemini    Chat/Response/Anthropic  ← /v1/models 中 antigravity 的模型（gemini/claude）
 *
 * baseUrl 的拼接口径是实测钉死的（与 ZCode 内置 SDK 的行为一致）：
 *   chat/responses 线的 baseUrl 必须带 /v1，ZCode 会追加 /chat/completions、/responses；
 *   anthropic 线的 baseUrl 不带 /v1，ZCode 会追加 /v1/messages。
 * api.type 是 ZCode 的严格枚举，只有上面三个合法值。
 *
 * 用户意图保留（本版修正：之前只按"族"判断，隐藏单条协议线上的模型会被
 * 另外两条线的并集掩盖，同步时又塞回去——本次改成逐 provider 判定）：
 *   - provider 级：沿用原规则的 enabled / visibility（你在 ZCode 里关掉或隐藏的
 *     供应商不会被重新打开）；整组被删掉的 provider 也不再自动重建，
 *     需要恢复时显式跑 `--readd`；
 *   - 模型级：某条协议线的 personalModelIds 里消失的模型 = 你在这条线上隐藏了它，
 *     后续同步不会再塞回来；重新加回即自动解除。逐 provider 独立记录，
 *     不再用三条线并集判断；
 *   - 排序：你调整过的 modelOrder 保留，新模型追加到末尾；
 *   - 未知字段：原 provider / model 规则里的其它键原样保留。
 *
 * 安全轨（对着两次真实事故的根因逐条设防）：
 *   1. 只碰自己那 9 个 providerId，其余供应商、模型声明、排序原样保留（写后逐条深比对）；
 *   2. 所有数组永远写成 []，绝不出现 null（事故一：Go 的 nil 切片序列化成 null）；
 *   3. api.type 只用固定枚举常量（事故三：写了非法的 "anthropic"）；
 *   4. 写前带时间戳备份，临时文件 + rename 原子替换；
 *   5. 写完立即回读严格校验，任何一项不过就从备份自动回滚；
 *   6. 清单为空 / 网关不可达 / 配置读不动 —— 一律原地不动（宁可不写，不写坏）；
 *   7. 内容没变就完全不写（连备份都不产生）。
 *
 * 用法（钩子无参数运行时=同步；人工排查用下列开关）：
 *   node scripts/session-start.mjs                 # 同步（ZCode SessionStart 钩子默认调用）
 *   node scripts/session-start.mjs --status        # 看当前安装状态（不改动）
 *   node scripts/session-start.mjs --dry-run       # 打印将要发生的改动（不落盘）
 *   node scripts/session-start.mjs --remove        # 从 ZCode 里摘掉这 9 个供应商
 *   node scripts/session-start.mjs --readd         # 把用户整组删掉的供应商重新加回
 *   node scripts/session-start.mjs --config <path> --gateway <url> --api-key <key> --verbose
 */

import fs from "node:fs";
import path from "node:path";
import os from "node:os";
import { fileURLToPath } from "node:url";

// ── 固定身份（3 个模型族 × 3 条协议线）─────────────────────────────────────
const FAMILIES = [
  {
    key: "opencode",
    label: "OpenCode 免费",
    providers: [
      { id: "bargainproxy-opencode-chat", name: "BargainProxy OpenCode Chat", apiType: "openai-chat-completions", v1: true },
      { id: "bargainproxy-opencode-responses", name: "BargainProxy OpenCode Response", apiType: "openai-responses", v1: true },
      { id: "bargainproxy-opencode-anthropic", name: "BargainProxy OpenCode Anthropic", apiType: "anthropic-messages", v1: false },
    ],
  },
  {
    key: "cn",
    label: "国模",
    providers: [
      { id: "bargainproxy-cn-chat", name: "BargainProxy 国模 Chat", apiType: "openai-chat-completions", v1: true },
      { id: "bargainproxy-cn-responses", name: "BargainProxy 国模 Response", apiType: "openai-responses", v1: true },
      { id: "bargainproxy-cn-anthropic", name: "BargainProxy 国模 Anthropic", apiType: "anthropic-messages", v1: false },
    ],
  },
  {
    key: "gemini",
    label: "Gemini/Claude",
    providers: [
      { id: "bargainproxy-gemini-chat", name: "BargainProxy Gemini Chat", apiType: "openai-chat-completions", v1: true },
      { id: "bargainproxy-gemini-responses", name: "BargainProxy Gemini Response", apiType: "openai-responses", v1: true },
      { id: "bargainproxy-gemini-anthropic", name: "BargainProxy Gemini Anthropic", apiType: "anthropic-messages", v1: false },
    ],
  },
];
const PROVIDERS = FAMILIES.flatMap((f) => f.providers);
const OUR_IDS = PROVIDERS.map((p) => p.id);

const DEFAULT_GATEWAY = "http://127.0.0.1:13140";
const DEFAULT_API_KEY = "bp-public"; // 网关开放模式时任意值；设了网关 api_key 时用 --api-key 覆盖
const STATE_PATH = "/panel/api/opencodezen";
const MODELS_DEV_URL = "https://models.dev/api.json";

// 元数据不可得时的中性默认（不编造确定值）
const DEFAULT_CONTEXT_WINDOW = 262144;
const DEFAULT_MAX_OUTPUT = 32768;
const ACCEPTED_EFFORTS = ["minimal", "low", "medium", "high", "xhigh", "max"];
const DEFAULT_EFFORTS = ["minimal", "low", "medium", "high"];

const HTTP_TIMEOUT_MS = 5000;
const TAG = "bargainproxy-opencode";

// ── CLI ────────────────────────────────────────────────────────────────────
function parseArgs(argv) {
  const out = { mode: "sync", config: "", gateway: "", apiKey: "", verbose: false, readd: false };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    const next = () => argv[++i] ?? "";
    if (a === "--status") out.mode = "status";
    else if (a === "--dry-run") out.mode = "dry-run";
    else if (a === "--remove") out.mode = "remove";
    else if (a === "--config") out.config = next();
    else if (a === "--gateway") out.gateway = next();
    else if (a === "--api-key") out.apiKey = next();
    else if (a === "--readd") out.readd = true;
    else if (a === "--verbose" || a === "-v") out.verbose = true;
    else if (a === "--help" || a === "-h") out.help = true;
  }
  return out;
}

function note(msg, verbose) {
  if (verbose) console.log(`${TAG}: ${msg}`);
}
function fail(msg, code, { quiet } = {}) {
  console.error(`${TAG}: ${msg}`);
  if (!quiet && code !== 0) process.exitCode = code;
}

// ── 配置路径解析 ────────────────────────────────────────────────────────────
/**
 * 显式 --config 是权威的：给了就必须用它，路径不可用时**硬失败**，绝不回退到别处。
 * （教训：一次彩排里 --config 指到未创建的文件，脚本静默回退到了环境变量解析出的
 *   真实配置并写了它——显式参数绝不能有"猜一个别的"这种兜底。）
 */
function resolveConfigPath(explicit) {
  if (explicit) return fs.existsSync(explicit) ? explicit : "";
  const candidates = [];
  if (process.env.ZCODE_PROVIDER_CONFIG) candidates.push(process.env.ZCODE_PROVIDER_CONFIG);
  const base = process.env.ZCODE_DATA_BASE_DIR;
  if (base) candidates.push(path.join(base, ".zcode", "v2", "provider_config.json"));
  if (process.env.USERPROFILE) candidates.push(path.join(process.env.USERPROFILE, ".zcode", "v2", "provider_config.json"));
  if (process.env.HOME) candidates.push(path.join(process.env.HOME, ".zcode", "v2", "provider_config.json"));
  for (const c of candidates) {
    try {
      if (fs.statSync(c).isFile()) return c;
    } catch {
      /* keep looking */
    }
  }
  return "";
}

/** 状态文件与 ZCode 配置同目录，专门记录"上次写过的清单"用于识别用户隐藏。*/
function statePathFor(configFile) {
  return path.join(path.dirname(configFile), "bargainproxy-opencode.state.json");
}

function readState(configFile) {
  try {
    const raw = fs.readFileSync(statePathFor(configFile), "utf8");
    const doc = JSON.parse(raw);
    return doc && typeof doc === "object" && doc.families && typeof doc.families === "object" ? doc : { families: {} };
  } catch {
    return { families: {} };
  }
}

/**
 * 把 buildDoc 记录的逐 provider 状态收拢成 state.families。
 * 来源不可达（本次没碰）的族沿用旧状态，否则它们的隐藏记录会被抹掉。
 */
function stateFromPlan(plan, prevState) {
  const families = { ...(prevState?.families ?? {}) };
  for (const f of plan) {
    if (!f?.state) continue;
    families[f.key] = f.state;
  }
  return families;
}

function writeState(configFile, families) {
  const doc = { version: 2, updatedAt: new Date().toISOString(), families };
  const file = statePathFor(configFile);
  const tmp = `${file}.${process.pid}.tmp`;
  fs.writeFileSync(tmp, JSON.stringify(doc, null, 2) + "\n");
  fs.renameSync(tmp, file);
}

// ── HTTP ────────────────────────────────────────────────────────────────────
async function getJson(url, timeoutMs = HTTP_TIMEOUT_MS, apiKey = "") {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), timeoutMs);
  try {
    const headers = { accept: "application/json" };
    if (apiKey) headers.authorization = `Bearer ${apiKey}`;
    const res = await fetch(url, { signal: ctl.signal, headers });
    if (!res.ok) return null;
    return await res.json();
  } catch {
    return null;
  } finally {
    clearTimeout(timer);
  }
}

// ── 来源 1：OpenCode 免费模型（面板同口径：候选里没被判定失败的；全失败给全候选）
// probe_status：ok 可用 / limited 临时限流（429、超时，路由保留）/ fail 确定性失败。
// 限流不是"模型坏了"，所以和 ok 一起写进 ZCode；只有 fail 才排除。
function usableModels(state) {
  const models = state?.snapshot?.models;
  if (!Array.isArray(models)) return null;
  const usable = [];
  const all = [];
  for (const m of models) {
    if (!m || m.candidate !== true || typeof m.id !== "string" || !m.id) continue;
    all.push(m.id);
    if (m.probe_status !== "fail") usable.push(m.id);
  }
  if (usable.length > 0) return usable;
  return all;
}

// ── 来源 2：网关 /v1/models → 国模 / Gemini 两类 ─────────────────────────────
async function gatewayFamilies(gateway, apiKey) {
  const doc = await getJson(`${gateway}/v1/models`, HTTP_TIMEOUT_MS, apiKey);
  if (!doc || !Array.isArray(doc.data)) return null;
  const cn = [];
  const gemini = [];
  for (const m of doc.data) {
    const id = typeof m?.id === "string" ? m.id : "";
    if (!id) continue;
    const owner = String(m?.owned_by ?? "");
    if (owner === "opencodezen") continue; // 由 OpenCode 族单独维护
    if (owner === "antigravity") gemini.push(id);
    else cn.push(id);
  }
  const uniq = (arr) => [...new Set(arr)].sort();
  return { cn: uniq(cn), gemini: uniq(gemini) };
}

// ── models.dev 元数据（尽力而为；拿不到就中性默认，绝不编造）────────────────
const PREFERRED_PROVIDERS = [
  "zai", "zhipuai", "alibaba-cn", "alibaba", "deepseek", "moonshotai-cn", "moonshotai",
  "minimax-cn", "minimax", "google", "anthropic",
];

function providersOf(data) {
  if (!data || typeof data !== "object") return {};
  return data.providers && typeof data.providers === "object" ? data.providers : data;
}

/** 在 models.dev 全量 provider 里按优先级找同一个模型 id 的官方条目。*/
function pickModelMetaRaw(id, data) {
  const providers = providersOf(data);
  if (!providers || typeof providers !== "object") return null;
  const order = [...PREFERRED_PROVIDERS, ...Object.keys(providers).filter((k) => !PREFERRED_PROVIDERS.includes(k)).sort()];
  for (const key of order) {
    const models = providers[key]?.models;
    if (!models || typeof models !== "object") continue;
    if (models[id] && typeof models[id] === "object") return models[id];
  }
  return null;
}

function metadataFor(id, modelsDev) {
  const raw = modelsDev ? pickModelMetaRaw(id, modelsDev) : null;
  const limit = raw?.limit ?? {};
  const num = (v) => (typeof v === "number" && Number.isFinite(v) ? v : null);
  const ctx = num(limit.context) ?? DEFAULT_CONTEXT_WINDOW;
  const maxOut = num(limit.output) ?? DEFAULT_MAX_OUTPUT;
  const inputs = Array.isArray(raw?.modalities?.input) ? raw.modalities.input.filter((s) => typeof s === "string") : [];
  const supportsImage = inputs.includes("image");
  return { contextWindow: ctx, maxOutputTokens: maxOut, supportsImage, efforts: effortsFor(raw) };
}

/** reasoning_options → 思考档位阶梯（去掉上游不收的 off/none；与参考实现同口径）*/
function effortsFor(raw) {
  if (raw?.reasoning !== true) return [];
  const declared = [];
  const options = Array.isArray(raw?.reasoning_options) ? raw.reasoning_options : [];
  for (const option of options) {
    if (!option || typeof option !== "object" || option.type !== "effort") continue;
    if (!Array.isArray(option.values)) continue;
    for (const v of option.values) {
      const level = v === "none" ? "off" : v;
      if (typeof level === "string" && ACCEPTED_EFFORTS.includes(level) && !declared.includes(level)) declared.push(level);
    }
  }
  if (declared.length === 0) return [...DEFAULT_EFFORTS];
  declared.sort((a, b) => ACCEPTED_EFFORTS.indexOf(a) - ACCEPTED_EFFORTS.indexOf(b));
  return declared;
}

// ── 组装 ────────────────────────────────────────────────────────────────────
const ours = (rule) => rule && typeof rule === "object" && OUR_IDS.includes(rule.providerId);
const asArray = (v) => (Array.isArray(v) ? v : []);
const stable = (v) => {
  if (Array.isArray(v)) return v.map(stable);
  if (v && typeof v === "object") {
    const out = {};
    for (const k of Object.keys(v).sort()) out[k] = stable(v[k]);
    return out;
  }
  return v;
};
const sameJson = (a, b) => JSON.stringify(stable(a)) === JSON.stringify(stable(b));

/**
 * Provider 级历史状态。v1 的 state 只有族级 writtenIds/hiddenIds（三条协议线
 * 共用一个清单），v2 起按 provider 分开，因为 ZCode 的"隐藏某个模型"是**逐
 * provider** 从 personalModelIds 里删掉该模型——族级并集会把它当成仍可见，
 * 于是下一次同步又把它塞回去（这就是"改了但没改干净"的根因）。
 */
function providerState(familyState, providerId) {
  const map = familyState?.providers;
  if (map && typeof map === "object") {
    const p = map[providerId];
    if (p && typeof p === "object") {
      return {
        wrote: p.wrote === true,
        writtenIds: asArray(p.writtenIds).filter((x) => typeof x === "string"),
        hiddenIds: asArray(p.hiddenIds).filter((x) => typeof x === "string"),
      };
    }
    return { wrote: false, writtenIds: [], hiddenIds: [] };
  }
  // v1 迁移：族级清单视为每条协议线都写过。
  const writtenIds = asArray(familyState?.writtenIds).filter((x) => typeof x === "string");
  return {
    wrote: writtenIds.length > 0,
    writtenIds,
    hiddenIds: asArray(familyState?.hiddenIds).filter((x) => typeof x === "string"),
  };
}

/** 保留用户已有的模型排序，新模型追加在末尾。*/
function mergeModelOrder(prevOrder, effective) {
  const keep = asArray(prevOrder).filter((id) => effective.includes(id));
  const seen = new Set(keep);
  return [...keep, ...effective.filter((id) => !seen.has(id))];
}

/**
 * 计算某条协议线本次应写入的模型清单，并识别"用户隐藏"（逐 provider）：
 *   - 上次写过、但这条线的 personalModelIds 里没有 = 用户在这里隐藏了它；
 *   - 用户重新加回（出现在本条 personalModelIds）= 自动解除隐藏；
 *   - 隐藏集合持久化在 state.providers[providerId].hiddenIds。
 * 只在该 provider 规则仍然存在时才做"新隐藏"推断：规则整体消失是删号/
 * 换配置，不能据此把模型全判成隐藏。
 */
function computeEffective(existing, family, state, providerId) {
  const familyState = state?.families?.[family.key] || {};
  const st = providerState(familyState, providerId);
  const prevRule = asArray(existing?.config?.providerConfigRules?.providerRules)
    .find((r) => r?.providerId === providerId);
  const source = Array.isArray(family.ids) ? family.ids : [];
  const visible = new Set(
    prevRule
      ? asArray(prevRule?.config?.personalModelIds).filter((x) => typeof x === "string")
      : st.writtenIds,
  );
  const newlyHidden = prevRule ? st.writtenIds.filter((id) => !visible.has(id)) : [];
  const hidden = new Set([...st.hiddenIds, ...newlyHidden].filter((id) => !visible.has(id)));
  const effective = source.filter((id) => !hidden.has(id));
  return {
    effective,
    order: mergeModelOrder(prevRule?.config?.modelOrder, effective),
    hidden: [...hidden],
    source,
    prevRule,
    st,
  };
}

/**
 * 组装目标文档：
 * - 只重建"有最新清单"的族；来源不可达的族原样保留；
 * - provider 规则沿用原规则的 enabled / visibility / 未知字段；
 * - 模型规则对已存在的模型整条复用（保留用户可能写入的隐藏字段）；
 * - 模型隐藏逐 provider 判定（见 computeEffective），不再用族级并集；
 * - 用户整组删掉/隐藏的 provider 不再自动重建（--readd 可强制加回）；
 * - 模型全被隐藏的 provider 原样保留，不写空清单（写空会让 ZCode 丢掉该组
 *   连同里面的 enabled / visibility 用户设置）；
 * - 用户调整过的 modelOrder 保留原顺序，新模型追加到末尾。
 */
function buildDoc(existing, base, apiKey, plan, metaFor, prevState, opts = {}) {
  const doc = JSON.parse(JSON.stringify(existing));
  const cfg = doc.config;
  const providerRules = asArray(cfg?.providerConfigRules?.providerRules);
  const modelRules = cfg?.modelConfigRules ?? (cfg.modelConfigRules = {});
  const existingModelRules = asArray(modelRules.providerModelRules);
  const existingByKey = new Map();
  for (const r of existingModelRules) {
    if (r && typeof r.providerId === "string" && typeof r.modelId === "string") {
      existingByKey.set(`${r.providerId}\u0000${r.modelId}`, r);
    }
  }

  const active = plan.filter((f) => f.ids !== null && f.ids.length > 0);

  const ourProviderRules = [];
  const ourModelRules = [];
  const writtenIds = new Set(); // providers we rewrite this round
  for (const family of active) {
    family.written = [];
    family.skipped = [];
    family.state = { sourceIds: [...family.ids], providers: {} };
    for (const p of family.providers) {
      const prev = providerRules.find((r) => r?.providerId === p.id);
      const st = providerState(prevState?.families?.[family.key], p.id);
      // The user removed/hid this whole provider group in ZCode: it is gone
      // from the rules. Re-creating it every sync undoes that choice, so only
      // --readd brings it back.
      if (!prev && st.wrote && !opts.readd) {
        family.skipped.push(p.id);
        family.state.providers[p.id] = {
          wrote: true,
          writtenIds: st.writtenIds,
          hiddenIds: st.hiddenIds,
        };
        continue;
      }
      const { effective, order, hidden } = computeEffective(existing, family, prevState, p.id);
      const prevCfg = prev?.config && typeof prev.config === "object" ? prev.config : {};
      if (effective.length === 0 && prev) {
        // Everything on this line is hidden by the user: keep the rule byte
        // for byte so its enabled/visibility survive.
        family.written.push({
          id: p.id,
          effective: asArray(prevCfg.personalModelIds),
          order: asArray(prevCfg.modelOrder),
          untouched: true,
        });
        family.state.providers[p.id] = { wrote: true, writtenIds: st.writtenIds, hiddenIds: hidden };
        continue;
      }
      const rule = {
        ...(prev ?? {}),
        providerId: p.id,
        providerName: p.name,
        enabled: typeof prev?.enabled === "boolean" ? prev.enabled : true,
        config: {
          ...prevCfg,
          group: typeof prevCfg.group === "string" ? prevCfg.group : "standard-personal",
          visibility: typeof prevCfg.visibility === "string" ? prevCfg.visibility : "visible",
          access: { type: "api-key", apiKey },
          api: { type: p.apiType, baseUrl: p.v1 ? `${base}/v1` : base },
          personalModelIds: [...effective],
          modelOrder: order,
        },
      };
      ourProviderRules.push(rule);
      writtenIds.add(p.id);
      family.written.push({ id: p.id, effective, order, untouched: false });
      family.state.providers[p.id] = { wrote: true, writtenIds: effective, hiddenIds: hidden };
      for (const id of effective) {
        const reused = existingByKey.get(`${p.id}\u0000${id}`);
        if (reused) {
          ourModelRules.push({ ...reused, providerId: p.id, modelId: id });
        } else {
          ourModelRules.push(buildModelRule(p.id, id, metaFor(id)));
        }
      }
    }
  }

  // Rules we did not rewrite (other vendors + untouched/removed providers) are
  // carried over verbatim.
  const keptProviderRules = providerRules.filter((r) => !writtenIds.has(r?.providerId));
  const keptModelRules = existingModelRules.filter((r) => !writtenIds.has(r?.providerId));
  const keptManualRules = asArray(modelRules.manualProviderModelRules).filter((r) => !writtenIds.has(r?.providerId));
  const keptOrder = asArray(cfg.providerOrder).filter((v) => !(typeof v === "string" && writtenIds.has(v)));

  doc.schemaVersion = 1;
  cfg.providerOrder = [...keptOrder, ...ourProviderRules.map((r) => r.providerId)];
  cfg.providerConfigRules = { ...(cfg.providerConfigRules ?? {}), providerRules: [...keptProviderRules, ...ourProviderRules] };
  modelRules.providerModelRules = [...keptModelRules, ...ourModelRules];
  modelRules.manualProviderModelRules = [...keptManualRules];
  return doc;
}

function buildModelRule(providerId, id, meta) {
  const properties = {
    contextWindow: meta.contextWindow,
    inputFormat: { supportsImage: meta.supportsImage },
  };
  const optionSpecs = {};
  if (meta.efforts.length > 0) optionSpecs.reasoningLevel = { values: [...meta.efforts] };
  if (Number.isFinite(meta.maxOutputTokens)) optionSpecs.maxOutputTokens = { max: meta.maxOutputTokens };
  const config = { properties };
  if (Object.keys(optionSpecs).length > 0) config.optionSpecs = optionSpecs;
  return { providerId, modelId: id, config };
}

/** 摘除：同样在原文档深拷贝上打补丁，只去掉自己的 9 个 id，其余原样。*/
function removeDoc(existing) {
  const doc = JSON.parse(JSON.stringify(existing));
  const cfg = doc.config;
  const providerRules = asArray(cfg?.providerConfigRules?.providerRules);
  const modelRules = cfg?.modelConfigRules ?? (cfg.modelConfigRules = {});
  const keptProviderRules = providerRules.filter((r) => !ours(r));
  const keptModelRules = asArray(modelRules.providerModelRules).filter((r) => !ours(r));
  const keptManualRules = asArray(modelRules.manualProviderModelRules).filter((r) => !ours(r));
  const keptOrder = asArray(cfg.providerOrder).filter((v) => !(typeof v === "string" && OUR_IDS.includes(v)));

  doc.schemaVersion = 1;
  cfg.providerOrder = keptOrder;
  cfg.providerConfigRules = { ...(cfg.providerConfigRules ?? {}), providerRules: keptProviderRules };
  modelRules.providerModelRules = keptModelRules;
  modelRules.manualProviderModelRules = keptManualRules;
  return doc;
}

// ── 校验（写后回读的严格闸门；任一不过即回滚）──────────────────────────────
function findNulls(v, where = "$", out = []) {
  if (v === null) out.push(where);
  else if (Array.isArray(v)) v.forEach((x, i) => findNulls(x, `${where}[${i}]`, out));
  else if (v && typeof v === "object") for (const [k, x] of Object.entries(v)) findNulls(x, `${where}.${k}`, out);
  return out;
}

/** 我方 provider 规则里，用户/未知字段必须原样保留（api/access/清单/顺序归插件管）。*/
function preservedOurs(prev, next) {
  const problems = [];
  if (!prev) return problems;
  for (const [k, v] of Object.entries(prev)) {
    if (["providerId", "providerName", "enabled", "config"].includes(k)) continue;
    if (!sameJson(v, next?.[k])) problems.push(`未知字段 ${k} 被改动`);
  }
  if (typeof prev.enabled === "boolean" && prev.enabled !== next.enabled) problems.push("enabled 被改动");
  const owned = new Set(["group", "visibility", "access", "api", "personalModelIds", "modelOrder"]);
  for (const [k, v] of Object.entries(prev.config ?? {})) {
    if (owned.has(k)) continue;
    if (!sameJson(v, next?.config?.[k])) problems.push(`config.${k} 被改动`);
  }
  if (typeof prev.config?.visibility === "string" && prev.config.visibility !== next?.config?.visibility) {
    problems.push("visibility 被改动");
  }
  return problems;
}

function validateDoc(doc, { base, plan, previous, expectOurs = true }) {
  const problems = [];
  if (doc?.schemaVersion !== 1) problems.push("schemaVersion 必须为 1");
  const cfg = doc?.config;
  const providerRules = cfg?.providerConfigRules?.providerRules;
  const modelRules = cfg?.modelConfigRules;
  if (!Array.isArray(providerRules)) problems.push("providerConfigRules.providerRules 必须是数组（不能是 null）");
  if (!Array.isArray(modelRules?.providerModelRules)) problems.push("providerModelRules 必须是数组");
  if (!Array.isArray(modelRules?.manualProviderModelRules)) problems.push("manualProviderModelRules 必须是数组");
  if (!Array.isArray(cfg?.providerOrder)) problems.push("providerOrder 必须是数组");

  const ourRules = asArray(providerRules).filter(ours);
  if (!expectOurs && ourRules.length !== 0) problems.push(`应已摘除自己的供应商，实际还剩 ${ourRules.length} 条`);
  if (expectOurs) {
    const active = plan.filter((f) => f.ids !== null && f.ids.length > 0);
    for (const family of active) {
      // buildDoc records exactly which lines it wrote and with what; verify
      // against that instead of recomputing (recomputing would re-derive the
      // old family-wide union and mask a wrong write).
      const written = asArray(family.written);
      for (const w of written) {
        const rule = ourRules.find((r) => r.providerId === w.id);
        if (!rule) {
          problems.push(`缺少供应商 ${w.id}`);
          continue;
        }
        const p = family.providers.find((x) => x.id === w.id);
        if (!p) continue;
        if (rule.config?.api?.type !== p.apiType) problems.push(`${p.id}: api.type=${rule.config?.api?.type} 非法`);
        const want = p.v1 ? `${base}/v1` : base;
        if (rule.config?.api?.baseUrl !== want) problems.push(`${p.id}: baseUrl=${rule.config?.api?.baseUrl}，应为 ${want}`);
        if (rule.config?.access?.type !== "api-key" || !rule.config?.access?.apiKey) problems.push(`${p.id}: access 形状不对`);
        if (!w.untouched) {
          if (!sameJson(rule.config?.personalModelIds, w.effective)) problems.push(`${p.id}: personalModelIds 与可用清单不一致`);
          if (!sameJson(rule.config?.modelOrder, w.order)) problems.push(`${p.id}: modelOrder 与预期顺序不一致`);
        }
        const prev = asArray(previous?.config?.providerConfigRules?.providerRules).find((r) => r?.providerId === p.id);
        for (const msg of preservedOurs(prev, rule)) problems.push(`${p.id}: ${msg}`);
      }
      // A provider the user removed must not be re-created behind their back.
      for (const id of asArray(family.skipped)) {
        if (ourRules.some((r) => r.providerId === id)) problems.push(`用户已删除的供应商被重建：${id}`);
      }
      const ruleKeys = new Set(asArray(modelRules?.providerModelRules).map((r) => `${r?.providerId}\u0000${r?.modelId}`));
      for (const w of written) {
        if (w.untouched) continue;
        for (const id of w.effective) {
          if (!ruleKeys.has(`${w.id}\u0000${id}`)) problems.push(`${w.id}: 缺少模型声明 ${id}`);
        }
      }
    }
    const nulls = findNulls(ourRules).concat(findNulls(asArray(modelRules?.providerModelRules).filter(ours)));
    if (nulls.length > 0) problems.push(`自己的条目里出现 null：${nulls.slice(0, 3).join(", ")}`);
  }

  // 别人的条目必须逐条原样保留（深比对，防"顺手改坏"）
  if (previous) {
    const prevRules = asArray(previous?.config?.providerConfigRules?.providerRules).filter((r) => !ours(r));
    const preserved = new Set(asArray(providerRules).filter((r) => !ours(r)).map((r) => JSON.stringify(stable(r))));
    for (const r of prevRules) {
      if (!preserved.has(JSON.stringify(stable(r)))) problems.push(`原有供应商被改动：${r?.providerId ?? "?"}`);
    }
    const prevModels = asArray(previous?.config?.modelConfigRules?.providerModelRules).filter((r) => !ours(r));
    const preservedModels = new Set(asArray(modelRules?.providerModelRules).filter((r) => !ours(r)).map((r) => JSON.stringify(stable(r))));
    for (const r of prevModels) {
      if (!preservedModels.has(JSON.stringify(stable(r)))) problems.push(`原有模型声明被改动：${r?.providerId ?? "?"}/${r?.modelId ?? "?"}`);
    }
  }
  return problems;
}

// ── 落盘（备份 + 原子替换 + 回读校验 + 失败回滚）────────────────────────────
function writeAtomic(file, doc) {
  const text = JSON.stringify(doc, null, 2) + "\n";
  const dir = path.dirname(file);
  const stamp = Date.now();
  const backup = `${file}.bak-${stamp}`;
  fs.copyFileSync(file, backup);
  const tmp = path.join(dir, `${path.basename(file)}.${process.pid}.tmp`);
  fs.writeFileSync(tmp, text);
  try {
    fs.renameSync(tmp, file); // 同目录 rename：Windows 上也是替换语义
  } catch (err) {
    try { fs.unlinkSync(tmp); } catch {}
    throw err;
  }
  return backup;
}

function restore(file, backup) {
  try {
    fs.copyFileSync(backup, file);
    return true;
  } catch {
    return false;
  }
}

// ── 主流程 ──────────────────────────────────────────────────────────────────
async function main(argv) {
  const args = parseArgs(argv);
  if (args.help) {
    console.log(fs.readFileSync(fileURLToPath(import.meta.url), "utf8").split("*/")[0].replace(/^#!.*\n/, ""));
    return 0;
  }
  // 钩子路径（无参数调用）绝不用非零退出码打断会话；显式带 --config 的调用视为人工操作，给真实退出码。
  const quiet = args.mode === "sync" && !args.config;
  const gatewayArg = args.gateway || process.env.BARGAINPROXY_GATEWAY || DEFAULT_GATEWAY;
  const apiKey = args.apiKey || process.env.BARGAINPROXY_API_KEY || DEFAULT_API_KEY;

  const file = resolveConfigPath(args.config);
  if (!file) {
    fail(
      args.config
        ? `--config 指定的文件不存在，拒绝回退到其他路径：${args.config}`
        : "找不到 ZCode 配置（可用 --config 指定）",
      1,
      { quiet },
    );
    return quiet ? 0 : 1;
  }
  if (path.basename(file) !== "provider_config.json") {
    fail(`拒绝操作非 provider_config.json 的文件：${file}`, 1, { quiet });
    return quiet ? 0 : 1;
  }

  let existingText;
  let existing;
  try {
    existingText = fs.readFileSync(file, "utf8");
    existing = JSON.parse(existingText);
  } catch (err) {
    fail(`ZCode 配置读不动/不是合法 JSON，保持原样：${err.message}`, 1, { quiet });
    return quiet ? 0 : 1;
  }
  if (existing?.schemaVersion !== 1 || typeof existing.config !== "object") {
    fail("ZCode 配置形状不认识（schemaVersion != 1），保持原样", 1, { quiet });
    return quiet ? 0 : 1;
  }

  const installed = asArray(existing?.config?.providerConfigRules?.providerRules).filter(ours);
  const prevState = readState(file);

  // 两个来源
  const [zenState, gw] = await Promise.all([
    getJson(`${gatewayArg}${STATE_PATH}`, HTTP_TIMEOUT_MS),
    gatewayFamilies(gatewayArg, apiKey),
  ]);
  const zenIds = zenState ? usableModels(zenState) : null;
  const plan = FAMILIES.map((f) => {
    let ids = null;
    if (f.key === "opencode") ids = zenIds;
    else if (gw) ids = gw[f.key] ?? null;
    return { ...f, ids };
  });

  // ── --status ──
  if (args.mode === "status") {
    console.log(`配置文件: ${file}`);
    console.log(`网关: ${gatewayArg} ${zenState || gw ? "可达" : "不可达"}`);
    for (const fam of plan) {
      console.log(`  ${fam.label}: ${fam.ids === null ? "未知（来源不可达）" : fam.ids.length + " 个" + (fam.ids.length ? "：" + fam.ids.join(", ") : "")}`);
    }
    if (installed.length === 0) console.log("ZCode 内: 未安装");
    else {
      for (const p of PROVIDERS) {
        const r = installed.find((x) => x.providerId === p.id);
        console.log(`ZCode 内: ${p.id} → ${r ? (r.config?.personalModelIds?.length ?? 0) + " 个模型 · enabled=" + r.enabled + " · visibility=" + (r.config?.visibility ?? "-") : "缺失"}`);
      }
    }
    return 0;
  }

  // ── --remove ──
  if (args.mode === "remove") {
    if (installed.length === 0) {
      console.log("未安装，无需摘除。");
      return 0;
    }
    const doc = removeDoc(existing);
    const problems = validateDoc(doc, { base: gatewayArg, plan: [], previous: existing, expectOurs: false });
    if (problems.length > 0) {
      fail(`摘除结果自检不过：${problems[0]}`, 1, { quiet: false });
      return 1;
    }
    const backup = writeAtomic(file, doc);
    const reread = JSON.parse(fs.readFileSync(file, "utf8"));
    const post = validateDoc(reread, { base: gatewayArg, plan: [], previous: existing, expectOurs: false });
    if (post.length > 0) {
      restore(file, backup);
      fail(`写后回读不过（已从备份回滚）：${post[0]}`, 1, { quiet: false });
      return 1;
    }
    console.log(`已摘除 ${OUR_IDS.length} 个供应商（备份：${path.basename(backup)}）。`);
    return 0;
  }

  // ── 同步（默认；钩子调用）──
  if (plan.every((f) => f.ids === null || f.ids.length === 0)) {
    note("没有可同步的模型（两个来源都不可达/为空），未改动", true);
    return 0;
  }
  const base = (typeof zenState?.gateway_base === "string" && zenState.gateway_base) || gatewayArg;

  // 元数据：已存在的模型声明整条复用；只有新模型才查 models.dev
  const existingKeys = new Set(
    asArray(existing?.config?.modelConfigRules?.providerModelRules)
      .filter((r) => r && typeof r.providerId === "string" && typeof r.modelId === "string")
      .map((r) => `${r.providerId}\u0000${r.modelId}`),
  );
  const newIds = [];
  for (const fam of plan) {
    if (fam.ids === null) continue;
    for (const id of fam.ids) {
      if (!PROVIDERS.some((p) => existingKeys.has(`${p.id}\u0000${id}`))) newIds.push(id);
    }
  }
  let modelsDev = null;
  if (newIds.length > 0) {
    modelsDev = await getJson(MODELS_DEV_URL, HTTP_TIMEOUT_MS);
    note(`新模型 ${newIds.length} 个，元数据${modelsDev ? "已取到" : "不可得（用中性默认）"}`, args.verbose);
  }
  const metaFor = (id) => metadataFor(id, modelsDev);

  const planWithState = plan.map((f) => ({ ...f }));
  planWithState.prevState = prevState;
  const doc = buildDoc(existing, base, apiKey, planWithState, metaFor, prevState, { readd: args.readd });
  if (sameJson(doc, existing)) {
    note("目标与现状一致，未写盘", true);
    // 状态文件缺失时补齐，保证下次能识别用户隐藏
    const needState = plan.some((f) => f.ids !== null && f.ids.length > 0 && !prevState.families?.[f.key]?.providers);
    if (needState) {
      try { writeState(file, stateFromPlan(planWithState, prevState)); } catch { /* state is advisory */ }
    }
    return 0;
  }

  if (args.mode === "dry-run") {
    const before = asArray(existing?.config?.providerConfigRules?.providerRules).length;
    const after = asArray(doc?.config?.providerConfigRules?.providerRules).length;
    console.log(`将写入 ${file}`);
    console.log(`供应商规则：${before} → ${after}（自己的 ${PROVIDERS.length} 条，按可用清单更新）`);
    for (const p of PROVIDERS) {
      const r = doc.config.providerConfigRules.providerRules.find((x) => x.providerId === p.id);
      if (r) console.log(`  ${p.id}: ${r.config.api.type} · ${r.config.api.baseUrl} · ${r.config.personalModelIds.length} 个模型 · enabled=${r.enabled} · visibility=${r.config.visibility}`);
      else console.log(`  ${p.id}: （本次不更新，保留现状）`);
    }
    const problems = validateDoc(doc, { base, plan: planWithState, previous: existing });
    console.log(problems.length > 0 ? `自检：不过 → ${problems[0]}` : "自检：通过");
    return 0;
  }

  const problems = validateDoc(doc, { base, plan: planWithState, previous: existing });
  if (problems.length > 0) {
    fail(`待写内容自检不过（未落盘）：${problems[0]}`, 1, { quiet });
    return quiet ? 0 : 1;
  }

  let backup;
  try {
    backup = writeAtomic(file, doc);
  } catch (err) {
    fail(`写盘失败（配置保持原样）：${err.message}`, 1, { quiet });
    return quiet ? 0 : 1;
  }

  const reread = JSON.parse(fs.readFileSync(file, "utf8"));
  const post = validateDoc(reread, { base, plan: planWithState, previous: existing });
  if (post.length > 0) {
    const ok = restore(file, backup);
    fail(`写后回读不过${ok ? "，已从备份回滚" : "，且回滚失败！"}：${post[0]}`, 1, { quiet });
    return quiet ? 0 : 1;
  }

  // 记录本次实际写入的清单（逐 provider），供下次识别"用户隐藏"
  try { writeState(file, stateFromPlan(planWithState, prevState)); } catch { /* state is advisory */ }

  if (!quiet) {
    console.log(`已写入 ${file}`);
    for (const f of plan) {
      if (f.ids === null || f.ids.length === 0) continue;
      console.log(`  ${f.label}: ${f.ids.length} 个模型 × 3 条协议线`);
    }
    console.log(`  备份：${path.basename(backup)}`);
  }
  return 0;
}

main(process.argv.slice(2)).then(
  (code) => process.exit(code ?? 0),
  (err) => {
    // 钩子路径绝不因异常打断会话：打印一行，退出 0；人工模式由上面的分支给非零码
    console.error(`${TAG}: 内部错误（未改动 ZCode 配置）：${err?.message ?? err}`);
    process.exit(0);
  },
);
