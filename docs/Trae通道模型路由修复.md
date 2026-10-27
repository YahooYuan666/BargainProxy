# Trae 通道模型路由修复（BP-137）

> 这份文档记录一个**已修复的上游行为误解**，供后续维护者与使用者查阅。
> 如果你在用本项目接 Trae 渠道，并且觉得"模型好像不太对"，请先读完本文。

## 症状

Trae 渠道的请求返回 200、内容正常，但**回答的模型不是你请求的那个**。

最容易观察到的现象：请求任意模型名——包括一个根本不存在的模型名——都会得到正常回复。

## 根因

Trae 的 chat 接口（`POST /api/agent/v3/llm_utils_chat`）**按 `config_name` 字段路由模型**。

本项目此前只发送 `model` 字段，没有发送 `config_name`。结果是：上游忽略 `model`，所有请求都由同一个固定的火山引擎（Volcengine）端点回答。

## 如何自行验证

Trae 的 SSE 响应里有一个 `event:timing_cost` 帧，其中的 `provider_model_name` 字段是**上游自报的实际提供方模型**。这是最直接的判据。

修复前的实测（2026-10-27，CN 账号，每次只改 model 一个变量）：

| 请求 model | `provider_model_name` |
| --- | --- |
| `kimi-k3` | `ep-20260802210216-sl4tw` |
| `Doubao-Seed-2.1-Pro` | `ep-20260802210216-sl4tw` |
| `kimi-k2.6` | `ep-20260802210216-sl4tw` |
| `glm-5.3-flash` | `ep-20260802210216-sl4tw` |
| `___definitely_not_a_model___` | `ep-20260802210216-sl4tw` |

五个不同的名字（含一个不存在的）全部落到同一个端点。`ep-…` 是火山引擎的 endpoint id，不是模型名。

修复后（请求体同时携带 `model` 与 `config_name`）：

| 请求 model | `provider_model_name` |
| --- | --- |
| `kimi-k3` | `ali-kimi-k3` |
| `glm-5.3-flash` | `glm-5.3-flash` |
| `deepseek-v4.1-flash` | `ep-20261020221437-cfj9b`（正常：DeepSeek 在 Trae 侧本就由火山托管） |

## 修复

`internal/driver/trae/trae.go` 的 `buildSOLOBody` 同时发送：

```go
"model":       call.UpstreamModel,
"config_name": call.UpstreamModel,
```

两个字段必须携带同一个 id（即路由绑定里的上游模型名，不是客户端看到的规范名）。

回归测试：`internal/driver/trae/bp137_config_name_test.go`。

## 排查方法论（踩过的坑）

**不要用"模型自述身份"判断路由。** 本项目走的是 Trae 的 SOLO **agent 函数**（`solo_work_lite` / `chat_v3` / `solo_agent` 等），agent 自带身份提示词。问它"你是什么模型"，得到的是那段提示词的答案，不是路由结果——早期正是因为看到它答"豆包"，误判成"Trae 不校验模型名、静默回落默认模型"，进而把 `config_name` 从请求体里删掉了。

正确的判据是上游响应里自报的 `provider_model_name`（或 `model_config.model_name`）。

## 上游参考项目的同类做法

| 项目 | 做法 |
| --- | --- |
| magpie `packages/trae` | `chatBody` 同时发送 `config_name` + `model` |
| traework | `mapModel`：未知名字直接 400，不静默塞默认模型；`pickOnly` 类模型必须**删除** `config_name`，否则上游回 4001 |
| traecn | 实现 `_check_provider_model`，provider 家族不匹配直接返回 502 `upstream_model_mismatch`；实际模型名从 `timing_cost.provider_model_name` 读取 |
| traehub | 官方客户端 state 快照里 `Doubao-Seed-Code` 标 `is_default: true`——"默认是豆包"指的是**客户端默认选中项**，不是服务端忽略 model 时的回落 |

## 影响范围

- 修复前，Trae 渠道的所有请求都可能由非请求的模型回答，且返回 200、内容看起来正常。
- "假模型名也能返回 200" 这一长期现象的解释：不是上游宽容，是路由没有读取我们的模型名。
- Trae 上 kimi / glm 的表现与 WorkBuddy、Lobster 不一致，也源于此。
