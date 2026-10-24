# BargainProxy

> 😭 **穷鬼自建中转** —— 力争做全网最尊重国模的中转

把廉价/免费的高质量模型上游聚合成一个统一 API 的自托管网关（个人自用）。

**输入渠道**：Google Pro（Antigravity → Gemini）、腾讯 CodeBuddy/WorkBuddy（积分制中国模型，含 Trae CN 与 Z.AI ZCode）、AutoClaw、Qoder、OpenCode 免费模型、任意 OpenAI 兼容中转站（兜底）。
**输出协议**：同一端口同时兼容 **OpenAI Chat Completions**、**OpenAI Responses**、**Anthropic Messages**、**Gemini v1beta** 四种请求形态，流式与非流式俱全。
**附带能力**：Web 管理面板、账号池自动调度（配额窗口贪婪 / 积分成本分层）、每日签到与成长任务自动化、跨渠道 fallback、凭据加密落库。

---

## ⚠️ 使用前必读

1. **严禁用于营业性商业目的。** 本项目只用于管理**你自己的**账号，把个人额度接到自己的客户端上。
   不提供、也不允许任何形式的对外售卖、代充、批量注册、共享号池运营或二次转卖 API。
   用它做生意导致的上游封号、欠费与法律风险由使用者自行承担。
2. **每个号池建议最多绑定 3 个账号。** 同一上游（WorkBuddy / Trae / ZCode / AutoClaw / Qoder / Google）
   塞太多账号不会更快，反而更容易撞上同 IP 风控与关联封禁；超过 3 个后收益极小、风险成倍上升。
   先用 1~2 个跑顺，需要更多并发再逐个加。
3. **本项目由纯 AI 制作**，主要集成 `agentpoints`（workbuddy2api-panel 一系）与
   `AIClient2API`（Google / Antigravity 反代规范），并参考了 magpie、sub2api、CLIProxyAPI 等项目，
   完整名单见下方「致敬」与 [NOTICE](NOTICE)。
   作者没有能力对全部路径做海量测试验证——上游接口改动、模型增删、Agent 版本差异都可能导致某个功能报错。
   **欢迎提 issue / 报 bug**（请附请求日志页的错误分类与渠道，不要贴对话内容与凭据）。
   如果你只是想把模型稳定用起来，**更建议直接使用 agentpoints 或 AIClient2API**——它们维护更久、社区验证更多；
   本项目定位是「自己折腾，能省一点是一点」。
4. **本仓库只发布源代码**，不含编译产物、运行数据与账号信息。遇到问题时，推荐直接把你的报错与相关源码丢给 AI 逐段排查——
   这也是本项目坚持发源码、不发 exe 的原因。

---

## 🫡 致敬

这个项目不是从零开始。下面每一个项目都实实在在贡献过思路或代码，向各位作者和维护者致敬 🫡。
没有他们，就没有这个「穷鬼自建中转」。

**代码级移植（MIT）**

- **[agentpoints / workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)**（MIT）——
  号池成本分层调度、模型级冷却、WorkBuddy / Trae / ZCode 池、签到与成长任务，大量实现直接移植自这里。
  原始上游是 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)（已删库，署名保留）。
- **[misakano7545/workbuddy2api-panel](https://github.com/misakano7545/workbuddy2api-panel)**（MIT）——
  同一血统里 2026-10 之后仍在每日更新的主线，WorkBuddy 单池的后续行为对照都看这里。

**行为规范参考（只学做法，不复制代码）**

- **[AIClient2API](https://github.com/justlovemaki/AIClient-2-API)**（GPLv3）——
  Google / Antigravity 反代、协议转换与配额窗口的行为规范。按许可边界做清洁室重写，未复制 GPL 代码。
- **[sub2api](https://github.com/Wei-Shaw/sub2api)**（LGPL-3.0）——
  会话指纹、三态准入（可调度 / 仅粘性 / 不可调度）、401 宽限自愈，均为清洁室重写。
- **[yetone/magpie](https://github.com/yetone/magpie)**（MIT）——
  同为 Go 多上游网关，是最贴近的同类项目。原生会话头优先、失败原因与冷却时长分开、工具 schema 补类型，都借鉴自这里。
- **[magpie-community/plugins](https://github.com/magpie-community/plugins)**（MIT）——
  社区插件仓，里面的 trae、autoclaw、qoder 三个包正好对应我们三个自研渠道：
  [trae](https://github.com/magpie-community/plugins/tree/main/packages/trae) 的 function 四档阶梯、
  [autoclaw](https://github.com/magpie-community/plugins/tree/main/packages/autoclaw) 的 CN 托管网关通道、
  [qoder](https://github.com/magpie-community/plugins/tree/main/packages/qoder) 的 10605 排队口径。
- **[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)**（MIT）与
  **[gemini-openai-proxy](https://github.com/zhu327/gemini-openai-proxy)**（MIT）——
  「协议适配 → 统一中间表示 → 号池 → 上游驱动」的分层，以及 Gemini 与 OpenAI 的结构映射。
- **[shuishuipingan/qoder2api-hub](https://github.com/shuishuipingan/qoder2api-hub)**（MIT）与
  **[autumnsentiment/Trae2api-cn](https://github.com/autumnsentiment/Trae2api-cn)**（MIT）——
  Qoder 排队协议与 TRAE 模型名、多模态形状的协议参考。

**只记行为、不取代码（原仓库没有许可证）**

- [Zhengyuuuui/qoder2api](https://github.com/Zhengyuuuui/qoder2api)、
  [Zhengyuuuui/autoclaw2api](https://github.com/Zhengyuuuui/autoclaw2api)——
  设备指纹派生与通道清单只作为行为记录，没有复制任何代码。

如果你需要更稳定、维护更久的选择，请优先使用 agentpoints 或 AIClient2API。

---

## ZCode 插件（可选）

仓库自带 [`plugins/bargainproxy-opencode`](plugins/bargainproxy-opencode/README.md)：把本机网关的模型
自动同步成 ZCode 的 **9 个个人供应商**（OpenCode 免费 / 国模 / Gemini-Claude × Chat / Responses / Anthropic），
ZCode 启动时自动刷新；你在 ZCode 里隐藏或关闭的供应商/模型会被保留，不会被重新打开。
安装方式见插件自己的 README；它与网关无代码耦合，只通过 HTTP 读取模型清单。

---

## 快速开始（Windows 本机，推荐路径）

不需要安装任何系统级软件：PostgreSQL 用**项目内便携版**（`.tools/` 目录，可整目录删除），网关是一个 exe。

```bash
# 一键启动（幂等：重复跑秒退；exe 已存在则跳过编译）
scripts\start-all.cmd
# 首次运行会交互式让你设定面板密码（写库一次，之后不再询问），
# 完成后直接打开 http://127.0.0.1:13140/panel/ 登录添加账号。
# 一键停止（网关 + 便携版 PG）
scripts\stop-all.cmd
# 密码遗忘/想更换：bargainproxy.exe -config config.json -set-password '新密码' 后重启服务
```

分步等价写法（排障用）：

```bash
# 1) 启动项目内便携版 PostgreSQL（首次自动初始化；只监听 127.0.0.1:55432）
scripts\pg\start.cmd

# 2) 编译网关；config.json 已指向便携版 PG
go build ./cmd/bargainproxy
./bargainproxy -config config.json                            # 启动（127.0.0.1:13140）

# 停止数据库：scripts\pg\stop.cmd
```

**开机自启 / 看门狗（面板可控）**：面板「服务管理」卡——
- **开机自启**（默认开）：开启即注册 Windows 计划任务 `BargainProxy_BootStart`（登录后静默运行 `scripts\autostart.vbs`：幂等探测 13140 端口，未监听才拉起整栈）；关闭即删除任务。
- **看门狗**（默认关）：`BargainProxy_Watchdog` 每 5 分钟巡检复活；默认不注册（对齐"只留开机自启、不装看门狗"的习惯）。
- 卡片实时显示两个计划任务的真实状态（Ready/未注册）；任务注册/删除只在面板拨动开关时发生，网关启动路径从不静默改系统。
- 网关日志：`logs\gateway.log` + `logs\gateway.err.log`；注册计划任务要求网关从项目根目录启动（`scripts\start-all.cmd` 即满足）。

**便携版 PostgreSQL 说明**：官方 16.4 构建（22MB）在 `.tools/postgres/`，数据在 `.tools/pgdata/`，日志 `.tools/pg.log`；trust 认证仅限本机回环监听。删掉整个 `.tools/` 即彻底卸载。想换真实 PostgreSQL（或 Docker 里的）：把 `config.json` 的 `database` 改成对应连接串即可，`store` 保持 `"postgres"`。
> 不想跑数据库：把 `config.json` 的 `store` 改成 `"memory"`（功能可用，但重启后账号/日志清空）。

## 快速开始（Docker，可选）

只在服务器/其他机器上用容器部署时使用：

```bash
cp config.example.json config.json      # 改 listen/database 等
docker compose up -d --build

# 首次设置面板密码（bcrypt 哈希写入数据库）
docker compose exec bargainproxy /app/bargainproxy -config /app/config.json -set-password '你的密码'
open http://127.0.0.1:13140/panel/
```

数据持久化原理：业务态（账号、请求日志、任务台账、设置）入 PostgreSQL；在途并发、冷却与粘性绑定在内存维护，进程重启自然清零（崩溃不会留下永久占用的账号；冷却窗口与签到台账从库中恢复）。

## 客户端接入

| 客户端 | base_url | 说明 |
|---|---|---|
| OpenAI SDK / Cline / 任意 OpenAI 客户端 | `http://127.0.0.1:13140/v1` | chat completions + responses |
| Claude Code | `http://127.0.0.1:13140` | Anthropic `/v1/messages`（`ANTHROPIC_BASE_URL`） |
| Gemini SDK | `http://127.0.0.1:13140` | `/v1beta/models/{model}:generateContent` |

模型名用**规范化裸名**（`glm-5.3`、`gemini-3-pro`、`deepseek-v4-flash`…），渠道选择是网关内部决策；`GET /v1/models` 可看全部可用模型及其渠道绑定。密钥：config 里 `api_key` 非空时要求 `Authorization: Bearer <key>`（默认本地不鉴权）。

### WorkBuddy 桌面版接入（一键）

本机装有 [WorkBuddy](https://www.workbuddy.cn) 桌面版时，网关模型可以直接登记进它的自定义模型列表：

- **面板**：设置页 →「登记模型进 WorkBuddy」→ 点一下即可（可先「预览」）。你自己手动加过的模型不会被覆盖；网关路由变动后重新点一次同步。
- **命令行**（等价）：`go run .\scripts\wbmodels -write`（不带 `-write` 只预览）。

登记后 WorkBuddy 的模型选择里会出现网关的全部可用模型（chat 协议，带上下文/最大输出/识图/思考档位规格），请求走本机网关、由网关统一选号与降级。写入前自动备份 `models.json`，WorkBuddy 约 1 秒内自动加载，无需重启。

## 添加账号

面板 → 「添加账号」，直接粘贴凭据 JSON（面板会自动识别各来源格式）：

- **antigravity**：AIClient2API 的 `*_oauth_creds.json`（access_token/refresh_token/expiry）
- **workbuddy**：workbuddy2api-panel 的 `auths/workbuddy-*.json` 原格式（`{account, auth}`）或扁平格式
- **trae**：`{"access_token": ..., "uid": ..., "device_id": ...}`
- **zcode**：`{"jwt": "...", "device_mid": "..."}`
- **autoclaw**：面板「添加账号 → AutoClaw」→ **「从本机客户端自动导入」**（推荐）。先用官方 AutoClaw 桌面客户端登录一次（国内版 / 国际版均可），点一下按钮即可：网关自己扫描客户端目录、解密凭据并入库，按来源自动区分国内区 / 国际区两条线路，不需要找文件或粘贴任何内容。也支持面板短信登录（仅国内区）。
  > 登录/签到/钱包等接口需要 `config.json` 里 `channels.autoclaw.app_secret`（官方客户端内置的签名常量）。
  > **源码不随附该常量**，请从你使用的客户端版本自行提取后填入；未配置时这几个接口会报明确的配置错误，聊天转发不受影响。
  > 国际区（autoglm.ai）与国内区是**两套独立账号与积分**，国际区应用内授权已被上游边缘防护关闭，只能靠本机客户端导入；导入后在「设置」里开启「国际版账号」开关才会参与调度。
- **qoder**：`{"pat": "pt-..."}`（Qoder 个人设置 → 服务集成 → 个人访问令牌）
- **openaicompat**：`{"api_key": "..."}`（配合 config 里 `openaicompat.base_url`）

**账号身份规则（重要）**：账号主键始终取自上游稳定身份——`workbuddy/trae` 用 **uid**、`antigravity` 用邮箱（无邮箱时用 refresh_token 哈希）、`zcode` 用 JWT 的 `sub`、`openaicompat` 用 api_key 哈希。"备注名"和上游昵称只写入展示字段 `nickname`，**永不参与主键**：改备注名、上游改名、token 轮换后重新导入，都只会更新原账号（upsert），不会产生重复行。缺 uid 的 workbuddy/trae 凭据会被明确拒绝（缺失时无法保证身份稳定）。

> **每个号池建议最多 3 个账号**：同一上游账号过多不会更快，反而更容易触发同 IP 风控与关联封禁。

凭据一律 AES-256-GCM 加密后落库（主密钥：环境变量 `BARGAINPROXY_MASTER_KEY` 优先，否则 `data/master.key` 自动生成）。

## 调度规则（省钱优先）

- **Antigravity（窗口型）**：按「剩余配额 / 距重置秒数」的贪婪系数排序，优先烧即将重置的窗口；单号并发 3，会话粘性强绑定，429 按上游给出的精确重置时间冷却。
- **WorkBuddy / Trae / ZCode（积分型）**：健康过滤 → 成本分层（实测限免 > 未知 > 实测收费，ε-greedy 定期探索未知档）→ 快过期积分加权 → Top5 洗牌加权随机 → 0.1s 并发避让；6004 模型级冷却只屏蔽该模型，402 冷却至次日 04:00（北京时间）。
- **跨渠道**：同名模型绑定多个渠道时按「能力 → 免积分 → 渠道优先序 → 会话粘性」选择；渠道忙/冷却时自动降级并回写 `X-BP-Served-Channel`。
- **Trae 两条通道**：带 tools 的请求走 `llm_utils_chat`（上限 262,144 token，工具可正常往返）；不带 tools 且超过 262,144 的请求自动切 `chat_sessions`（上限 1,000,000，但那条路上游跑自己的 agent、不认请求里的 tools）；带 tools 又超限的请求在发出去之前就判定本渠道装不下、直接交给下一个渠道，不再白等必然的 4026。
- **不轮换的情形**：内容级拒绝（含 HTTP 200 哨兵正文、`first_buddy` 类业务门槛）不换号，直接按协议返回错误。

## 任务中心

面板「任务中心」或后台定时：WorkBuddy 每日双轮签到（09:05/21:05 北京时间）+ 连登档位兑换 + 抽奖、Trae 每日签到、成长任务管道（活跃上报、桌面端事件链、专家召唤/使用、模板/画布/灵感案例、领养与猫猫旅行、外观主题）、余额探测（30 分钟）、Token 预刷新（1 小时）、请求日志过期清理（1 小时）。所有任务按 `(账号, 任务, 日期)` 幂等记录，重启不重复执行。

## 安全默认值

- 默认只监听 `127.0.0.1`；面板与数据面 API 分开鉴权（面板用独立密码 + HttpOnly/SameSite=Strict Cookie + CSRF Token，登录限速）。
- 请求日志**只记录元数据**（协议/模型/渠道/账号/耗时/Token 数），不落 prompt/completion/凭据；默认保留 14 天后自动清理。
- 不做服务端抓取外部图片 URL（防 SSRF）；`data:` 内联图片正常透传。
- 所有 SQL 参数绑定；上游并发上限按渠道硬约束（Trae=1、Antigravity=3、WorkBuddy=2、ZCode=2）。

## 目录结构

```
cmd/bargainproxy      入口（-config / -set-password）
internal/config      配置
internal/ir           统一中间表示
internal/protocol     openai / responses / anthropic / geminiv1 四种协议
internal/pool         账号池与调度（贪婪/成本分层/冷却/粘性）
internal/driver       antigravity / workbuddy / trae / zcode / autoclaw / qoder / opencodezen / openaicompat
internal/gateway      请求编排（路由→取号→驱动→降级→记账）
internal/server       HTTP 端点（四协议 + /v1/models + /healthz）
internal/panel        管理面板（embed 单页）
internal/wbregister   把网关模型登记进本机 WorkBuddy 桌面版（面板按钮与 CLI 共用）
internal/tasks        任务中心（签到/成长/探测/刷新/清理）
internal/store        PostgreSQL / 内存存储
internal/cryptox      AES-256-GCM 凭据信封
```

## 文档

- [跨国外网出口与 BBR2 调优](docs/跨国外网出口与BBR2调优.md)：Google / OpenCode 两条出境链路慢的根因与三步调优（BBR2、分流规则、DoH/QUIC），含回滚与验证流程。国模走国内直连，不要放进代理。
- [ZCode 同步插件](plugins/bargainproxy-opencode/README.md)：把网关模型同步进 ZCode 的 9 个个人供应商。

## 换电脑 / 重装时怎么把调好的设置带过去

模型路由表（哪些模型走哪些渠道、优先级）和调度参数都存在本机数据库里，`git clone` 带不走；`config.json` 里还有本机密钥，本来就不入库。所以换机器后要么用模板默认值，要么搬一次设置：

1. 老机器：面板「设置 → 设置迁移 → 导出设置文件」，得到 `bargainproxy-settings.json`（含路由表、模型目录白名单、并发/冷却/首字窗口/任务时点等调度参数；**不含**账号凭据、面板密码、登录态、请求日志）。
2. 新机器：`scripts\start-all.cmd` 起来，面板「设置 → 设置迁移 → 选择文件 → 导入并立即生效」。

`config.example.json` 里固化的就是本机这套路由与显示白名单，所以新机器不导入也能得到一致的模型清单；导入用于覆盖你在老机器上后来调整过的部分。

## 许可与声明

自研代码 [MIT](LICENSE)；移植与参考的第三方项目署名见 [NOTICE](NOTICE)。

本项目仅限个人自用（管理自己的账号），**严禁用于营业性商业目的**，不授权、不支持任何形式的对外售卖、代充、批量注册与共享池运营。

本项目**由纯 AI 制作**，主要集成 `agentpoints` 与 `AIClient2API`，并参考了 magpie、sub2api、CLIProxyAPI 等项目（完整名单见上方「致敬」），未对全部代码路径做海量测试；上游服务、模型清单与 Agent 行为随时可能变化，任何一处变化都可能导致功能失效。**如果你追求稳定，请直接使用 agentpoints 或 AIClient2API。**
