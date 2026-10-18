# BargainProxy 模型（ZCode 插件）

独立插件：把**本机 BargainProxy 网关的模型**同步进 ZCode 的个人 Provider 配置，
生成 **9 个供应商 = 3 个模型族 × 3 条协议线**，ZCode 启动时自动刷新：

| 模型族 | 来源 | 内容 |
| --- | --- | --- |
| OpenCode 免费 | `/panel/api/opencodezen` | 面板同口径的免费模型（候选里没被判定失败；429/超时等临时限流仍保留） |
| 国模 | `/v1/models` 中 `owned_by != antigravity` | GLM / DeepSeek / Qwen / Kimi 等 |
| Gemini/Claude | `/v1/models` 中 `owned_by == antigravity` | gemini-3.8-flash 系与 claude 系 |

每个族生成 Chat / Response / Anthropic 三条协议线（api.type 与 baseUrl 口径见下表）。

它和 BargainProxy 项目**没有代码耦合**：只通过 HTTP 读网关，项目侧不需要为它改任何东西；
反过来，项目的面板里也不会出现任何 ZCode 相关的界面。

## 生成什么

| 供应商（每族三条） | api.type | baseUrl | ZCode 自动追加 |
| --- | --- | --- | --- |
| `bargainproxy-<族>-chat` | `openai-chat-completions` | `http://127.0.0.1:13140/v1` | `/chat/completions` |
| `bargainproxy-<族>-responses` | `openai-responses` | `http://127.0.0.1:13140/v1` | `/responses` |
| `bargainproxy-<族>-anthropic` | `anthropic-messages` | `http://127.0.0.1:13140` | `/v1/messages` |

（`<族>` = `opencode` / `cn` / `gemini`）

三条线的 baseUrl 拼接口径是实测钉死的（anthropic 线**不带** `/v1`，另外两条**带**）。
模型元数据（上下文 / 最大输出 / 图像 / 推理档位）优先取自 models.dev 的官方 provider 条目；
取不到时用中性默认，不编造数值。已写入过的模型声明会被复用，避免每次刷新制造无谓差异。

## 用户隐藏 / 关闭会被保留

- **关闭或隐藏一个供应商**（ZCode 里的 enable / visibility）：同步时沿用原值，不会被重新打开。
- **整组删掉一个供应商**：同步不会再把它自动重建；确实想加回来时，显式跑
  `node scripts/session-start.mjs --readd`。
- **隐藏某个模型**：按**每一条协议线**独立判断。上一次同步写过、之后从该条线的
  模型清单里消失的模型，视为你在这条线上隐藏了它，后续同步不会加回来；
  你在 ZCode 里重新加回去，下一轮自动解除。三条线互不影响。
- **调整模型顺序**：你在 ZCode 里排好的顺序会保留，新模型只追加到末尾。
- 判据落盘在同目录的 `bargainproxy-opencode.state.json`（v2：逐 provider 记录
  已写清单 / 隐藏清单），不写进 ZCode 配置，避免污染 schema。
- 从旧版 state（v1，按族记录）升级时，隐藏记录会自动迁移，不需要手工处理。

## 安装（ZCode 侧，一次性）

1. ZCode → **插件市场 → 添加 → 添加插件市场**，粘贴目录：
   `D:\Projects\BargainProxy\plugins`
2. 在 **Personal → bargainproxy-local → BargainProxy OpenCode 免费模型 → 安装**
3. 新开一个会话（或重启 ZCode）即触发一次同步；之后每次启动都会自动刷新。

前提：BargainProxy 网关在运行；面板里「加入网关路由」保持勾选（清单从这里来）。

## 手动命令（排查用）

```bash
node "D:\Projects\BargainProxy\plugins\bargainproxy-opencode\scripts\session-start.mjs" --status
node "D:\Projects\BargainProxy\plugins\bargainproxy-opencode\scripts\session-start.mjs" --dry-run --verbose
node "D:\Projects\BargainProxy\plugins\bargainproxy-opencode\scripts\session-start.mjs" --remove
```

- `--status`：打印配置文件路径、网关可达性、可用模型与当前安装状态（不改动任何东西）
- `--dry-run`：打印将要发生的改动并做自检（不落盘）
- `--remove`：只摘掉本插件的三个供应商，其余原样
- 追加 `--config <path>` / `--gateway <url>` / `--api-key <key>` 可覆盖默认值

## 安全轨（对着两次真实事故逐条设防）

1. **只动自己的三个 providerId**：其余供应商、模型声明、排序原样保留，写后逐条深比对。
2. **数组永远是数组**：所有列表写成 `[]`，绝不出现 `null`（事故根因：nil 切片被序列化成 null）。
3. **api.type 只用合法枚举**：`openai-chat-completions` / `openai-responses` / `anthropic-messages`
   写死在常量里（事故根因：写过非法的 `anthropic`）。
4. **写前带时间戳备份**（`provider_config.json.bak-<毫秒>`），临时文件 + rename 原子替换。
5. **写后回读严格校验**（数组形状 / 枚举 / baseUrl / 清单一致 / 他人条目未变），任何一项
   不过就从备份自动回滚。
6. **宁可不写不写坏**：网关不可达、目录形状不认识、清单为空、配置读不动、schemaVersion
   不是 1 —— 一律原地不动。
7. **没变就不写**：内容与现状一致时连备份都不会产生。

配置路径解析顺序：`--config` → 环境变量 `ZCODE_PROVIDER_CONFIG` →
`%ZCODE_DATA_BASE_DIR%\.zcode\v2\provider_config.json` → `%USERPROFILE%\.zcode\v2\provider_config.json`。

## 卸载

1. 先摘掉供应商（避免留下指向网关的死条目）：
   `node ...\scripts\session-start.mjs --remove`
2. 再到 ZCode **设置 → 插件**里卸载本插件。
