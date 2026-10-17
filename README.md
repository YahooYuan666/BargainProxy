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
3. **本项目由纯 AI 制作**，主要集成两个开源项目：`agentpoints`（workbuddy2api-panel 一系）与
   `AIClient2API`（Google / Antigravity 反代规范），详见 [NOTICE](NOTICE)。
   作者没有能力对全部路径做海量测试验证——上游接口改动、模型增删、Agent 版本差异都可能导致某个功能报错。
   **欢迎提 issue / 报 bug**（请附请求日志页的错误分类与渠道，不要贴对话内容与凭据）。
   如果你只是想把模型稳定用起来，**更建议直接使用 agentpoints 或 AIClient2API**——它们维护更久、社区验证更多；
   本项目定位是「自己折腾，能省一点是一点」。
4. **本仓库只发布源代码**，不含编译产物、运行数据与账号信息。遇到问题时，推荐直接把你的报错与相关源码丢给 AI 逐段排查——
   这也是本项目坚持发源码、不发 exe 的原因。

---

## 🫡 致敬

这个项目不是从零开始，它基本是站在两个开源项目的肩膀上：

- **[agentpoints](https://github.com/linguo2625469/workbuddy2api-panel)**（workbuddy2api-panel 一系，MIT）——
  号池成本分层调度、模型级冷却、WorkBuddy/Trae/ZCode 池、签到与成长任务等大量实现思路和代码来自这里，**代码级移植**。
- **[AIClient2API](https://github.com/justlovemaki/AIClient-2-API)**（GPLv3）——
  Google / Antigravity 反代、协议转换与配额窗口的行为规范参考（按许可边界只做清洁室重写，未复制 GPL 代码）。

向两位作者和维护者致敬 🫡。没有他们，就没有这个「穷鬼自建中转」。
如果你需要更稳定、维护更久的选择，请优先使用上面两个原项目。

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

## 添加账号

面板 → 「添加账号」，直接粘贴凭据 JSON（面板会自动识别各来源格式）：

- **antigravity**：AIClient2API 的 `*_oauth_creds.json`（access_token/refresh_token/expiry）
- **workbuddy**：workbuddy2api-panel 的 `auths/workbuddy-*.json` 原格式（`{account, auth}`）或扁平格式
- **trae**：`{"access_token": ..., "uid": ..., "device_id": ...}`
- **zcode**：`{"jwt": "...", "device_mid": "..."}`
- **autoclaw**：面板「添加账号 → AutoClaw」走登录流程；或粘贴 z.ai 客户端抓到的 token（账号以钱包身份为准）。
  > 登录/签到/钱包等接口需要 `config.json` 里 `channels.autoclaw.app_secret`（官方客户端内置的签名常量）。
  > **源码不随附该常量**，请从你使用的客户端版本自行提取后填入；未配置时这几个接口会报明确的配置错误，聊天转发不受影响。
- **qoder**：`{"pat": "pt-..."}`（Qoder 个人设置 → 服务集成 → 个人访问令牌）
- **openaicompat**：`{"api_key": "..."}`（配合 config 里 `openaicompat.base_url`）

**账号身份规则（重要）**：账号主键始终取自上游稳定身份——`workbuddy/trae` 用 **uid**、`antigravity` 用邮箱（无邮箱时用 refresh_token 哈希）、`zcode` 用 JWT 的 `sub`、`openaicompat` 用 api_key 哈希。"备注名"和上游昵称只写入展示字段 `nickname`，**永不参与主键**：改备注名、上游改名、token 轮换后重新导入，都只会更新原账号（upsert），不会产生重复行。缺 uid 的 workbuddy/trae 凭据会被明确拒绝（缺失时无法保证身份稳定）。

> **每个号池建议最多 3 个账号**：同一上游账号过多不会更快，反而更容易触发同 IP 风控与关联封禁。

凭据一律 AES-256-GCM 加密后落库（主密钥：环境变量 `BARGAINPROXY_MASTER_KEY` 优先，否则 `data/master.key` 自动生成）。

## 调度规则（省钱优先）

- **Antigravity（窗口型）**：按「剩余配额 / 距重置秒数」的贪婪系数排序，优先烧即将重置的窗口；单号并发 3，会话粘性强绑定，429 按上游给出的精确重置时间冷却。
- **WorkBuddy / Trae / ZCode（积分型）**：健康过滤 → 成本分层（实测限免 > 未知 > 实测收费，ε-greedy 定期探索未知档）→ 快过期积分加权 → Top5 洗牌加权随机 → 0.1s 并发避让；6004 模型级冷却只屏蔽该模型，402 冷却至次日 04:00（北京时间）。
- **跨渠道**：同名模型绑定多个渠道时按「能力 → 免积分 → 渠道优先序 → 会话粘性」选择；渠道忙/冷却时自动降级并回写 `X-BP-Served-Channel`。
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
internal/tasks        任务中心（签到/成长/探测/刷新/清理）
internal/store        PostgreSQL / 内存存储
internal/cryptox      AES-256-GCM 凭据信封
```

## 文档

- [跨国外网出口与 BBR2 调优](docs/跨国外网出口与BBR2调优.md)：Google / OpenCode 两条出境链路慢的根因与三步调优（BBR2、分流规则、DoH/QUIC），含回滚与验证流程。国模走国内直连，不要放进代理。
- [ZCode 同步插件](plugins/bargainproxy-opencode/README.md)：把网关模型同步进 ZCode 的 9 个个人供应商。

## 许可与声明

自研代码 MIT；移植与参考的第三方项目署名见 [NOTICE](NOTICE)。

本项目仅限个人自用（管理自己的账号），**严禁用于营业性商业目的**，不授权、不支持任何形式的对外售卖、代充、批量注册与共享池运营。

本项目**由纯 AI 制作**，主要集成 `agentpoints` 与 `AIClient2API` 两个开源项目，未对全部代码路径做海量测试；上游服务、模型清单与 Agent 行为随时可能变化，任何一处变化都可能导致功能失效。**如果你追求稳定，请直接使用 agentpoints 或 AIClient2API。**
