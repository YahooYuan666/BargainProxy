# 跨国外网出口与 BBR2 调优（Google / OpenCode 路由出口）

> 适用场景：BargainProxy 跑的模型里有两条链路必须出境——
> **Google（Antigravity → Gemini / Claude）** 与 **OpenCode 免费模型（opencode.ai / models.dev）**；
> WorkBuddy / Trae / ZCode / AutoClaw / Qoder 走国内直连，**不要**把它们的流量丢进代理。
>
> 这份攻略解决的是「网关本身没问题，但出境请求慢到以为挂了」的那类问题。

---

## 1. 先分清两类症状

| 症状 | 常见误判 | 真实原因（多数情况） |
|---|---|---|
| 网页/测速下行几百 Mbps，但一发大 prompt 就卡十几秒到几十秒 | 以为 VPS 带宽不够 / TUN 有性能损耗 / 家宽上行是假的 | **上行方向**的 TCP 拥塞控制太保守（Windows 默认 Cubic），高 RTT + 偶发丢包时单流吞吐被压到 1 Mbps 级 |
| 只有 Google 或只有 OpenCode 慢，国模渠道正常 | 以为网关路由或驱动有 bug | 分流规则把目标域名送进了错误出口，或浏览器 DoH / QUIC 绕过了代理 |

两条链路要分开验证：**国模慢**看网关与上游限流（请求日志页有渠道/耗时）；**洋模慢**先看出口。

---

## 2. 根因：不对称的 TCP 拥塞控制

- 下行（VPS → 本机）：VPS 侧 Linux 默认 BBR，不以丢包判定拥塞，高延迟链路也能跑满。
- 上行（本机 → VPS）：Windows 默认 **Cubic**，靠丢包触发窗口减半。按
  `吞吐 ≈ 1.22 × MSS / (RTT × √丢包率)`，在 RTT 150~300ms、丢包 1% 的跨国链路上，
  单流上行只能到 **0.5~1 Mbps**。
- AI Agent 恰好是「上行主导」负载：每轮都要把全量上下文（单请求 1~5 MB）打上去。
  0.5 Mbps ≈ 20~80 秒，表现就是「模型像死了」。

## 3. 调优一：Windows 开启 BBR2（管理员终端，立即生效）

```powershell
# 1) 把默认拥塞控制从 cubic 换成 bbr2
netsh int tcp set supplemental template=internet congestionprovider=bbr2

# 2) 确认辅助特性
netsh int tcp set global autotuninglevel=normal
netsh int tcp set global timestamps=allowed
netsh int tcp set global rss=enabled

# 3) 验证
netsh int tcp show supplemental

# 回滚（恢复 Windows 默认）
netsh int tcp set supplemental template=internet congestionprovider=default
```

实测（2026-10，RTT 150~300ms 单节点）：2 MB 上行 18.2s → **3.1~3.8s**；
5 MB 上下文 约 45s → **11.7s**；TUN 与 HTTP 代理口差异 < 5%，**TUN 本身不是瓶颈**。

## 4. 调优二：分流规则只放行该出境的域名

以 Clash Verge Rev（mihomo 内核 + TUN + Fake-IP）为例，规则要点：

```yaml
rules:
  - GEOIP,private,DIRECT,no-resolve
  - DOMAIN,127.0.0.1,DIRECT
  - DOMAIN,localhost,DIRECT

  # Google 洋模链路（Antigravity / OAuth）
  - DOMAIN-SUFFIX,googleapis.com,PROXY
  - DOMAIN-SUFFIX,accounts.google.com,PROXY
  - DOMAIN-SUFFIX,oauth2.googleapis.com,PROXY

  # OpenCode 免费模型链路
  - DOMAIN-SUFFIX,opencode.ai,PROXY
  - DOMAIN-SUFFIX,models.dev,PROXY
  - DOMAIN-KEYWORD,opencode,PROXY

  # 国模与国内服务直连（不要让它们出境）
  - GEOSITE,cn,DIRECT
  - DOMAIN-SUFFIX,cn,DIRECT
  - GEOIP,CN,DIRECT

  - MATCH,PROXY
```

> 国模上游域名参考：`copilot.tencent.com`、`www.workbuddy.cn`、`www.workbuddy.ai`、
> `api.trae.cn`、`trae-api-cn.mchost.guru`、`zcode.z.ai`、`autoglm-acceleration-api.zhipuai.cn`、
> `gateway.qoder.com.cn`、`openapi.qoder.com.cn`。这些走代理只会更慢，还可能触发上游风控。

## 5. 调优三：浏览器与本地网络的两处坑

1. **关掉浏览器安全 DNS（DoH）**：它会绕过 TUN 的 Fake-IP 解析，导致部分域名解析失败或分流走错。
   Edge：设置 → 隐私、搜索和服务 → 关闭「使用安全的 DNS 指定如何查找网站的网络地址」。
2. **关掉实验性 QUIC（HTTP/3）**：跨国 UDP 443 常被限速/丢包，握手阶段就会卡住。
   Edge：`edge://flags/#enable-quic` → `Disabled`。

## 6. 验证流程（改完必做）

1. 先测上行：用同一代理口传一个 2 MB 文件，记录耗时；开 BBR2 前后对比应在 3~6 倍差距。
2. 再看网关：请求日志页筛渠道 = `Google（Antigravity）` / `OpenCode 免费通道`，
   看 TTFB 与总耗时是否恢复正常量级（洋模首字 > 30s 基本就是出口问题）。
3. 最后看国模：如果国模也慢，问题不在出口，去任务中心看是否全池冷却、账号是否过期。

## 7. 边界与备选

- BBR2 只解决「本机 → VPS」的上行；VPS → 上游（Google/OpenCode）那段由 VPS 的 BBR 负责。
- 若晚高峰丢包长期 > 10%，可评估 Hysteria 2（UDP/QUIC，Brutal 拥塞控制）作为同机备用端口；
  但部分运营商对境外 UDP 有 QoS，需实测，不要替代主链路前就全量切换。
- 本攻略只讨论网络调优，不涉及任何账号凭据；代理配置请自行保管，**不要提交到仓库**。
