# restart-all.md — 重启网关全栈的标准动作

> 本文件是**重启**的权威步骤。启动/停止的脚本本身有注释说明，这里只讲「怎么安全地重启」以及「哪些坑会让你以为重启了其实没生效」。
> 最后更新：2026-11-13（BP-323 期间实测）

---

## 0. 一句话版本

```cmd
scripts\stop-all.cmd
scripts\start-all.cmd
scripts\verify-deploy.cmd
```

三条命令，缺一不可。**只跑 `start-all.cmd` 不会重启**（见 §2 第一个坑）。

---

## 1. 为什么必须 stop 再 start

`scripts\start-all.cmd` 的第 4 步是这样判断的：

```cmd
netstat -ano | findstr /C::13140 | findstr /C:LISTENING
if errorlevel 1 goto startgw
echo [ok] gateway already listening on 127.0.0.1:13140
goto done
```

**端口在监听就跳过启动**。它仍然会编译（第 2 步，`build.cmd` 每次都编译），但**不会替换正在运行的进程**。所以：

- 改完代码 → 只跑 `start-all.cmd` → exe 变新了、进程还是旧的 → 行为没变。
- 这是本项目历史上「源码修好了但行为没变」反复出现的根因（`build.cmd` 头注释里写了）。

`build.cmd` 本身**只编译、绝不停止或启动任何东西**——这是刻意设计：网关承载着跑这次会话的 LLM 流量，重启必须是明确的动作。

---

## 2. 四个会让你误判的坑

### 坑 1：只跑 start-all，进程没换

**症状**：`bargainproxy.exe` 时间戳很新，但行为是旧的。
**检查**：`scripts\verify-deploy.cmd` 的第 2、3 项（进程启动时间 vs exe 时间）。
**正确做法**：先 `stop-all.cmd`，等端口消失，再 `start-all.cmd`。

### 坑 2：PG 刚停就启，第一个 start-all 必失败

便携版 PostgreSQL 停止后，数据目录还会被锁几秒。在这个窗口内启动，`pg_ctl` 会等约 30 秒然后失败。

`scripts\pg\start.cmd` 里已经写了等待旧 PID 退出的循环（最多 20 × 1s），正常情况能挡住。但如果你是用别的办法强杀的 PG，可能仍会撞上。

**症状**：`start-all.cmd` 卡在 `[1/4] portable PostgreSQL` 或直接报错。
**做法**：等 5 秒，**再跑一次** `start-all.cmd`（幂等，会接着往下走）。

### 坑 3：stop-all 杀进程但不等于 PG 已停

`stop-all.cmd` 做两件事：停网关（按 13140 端口找 PID）+ 停 PG（`pg_ctl stop`）。

如果 PG 停止时报错，脚本仍会 `exit /b 0`。**重启后第一次写库失败**才会暴露。

**检查**：
```cmd
powershell -NoProfile -Command "Get-NetTCPConnection -LocalPort 55432 -State Listen -ErrorAction SilentlyContinue"
```
有输出 = PG 还在监听（正常）；无输出且网关启动后立刻报连库失败 = PG 没起来，重跑 `scripts\pg\start.cmd`。

### 坑 4：日志被截断，故障现场消失

`start-all.cmd` 在真正启动网关前会执行：

```cmd
move /Y logs\gateway.err.log logs\gateway.err.log.1
```

**只保留一代**。所以重启两次，第一次重启前的现场就永久没了。

**如果正在查故障**：重启前先把它拷走。
```cmd
copy logs\gateway.err.log logs\gateway.err.log.investigate-20261113
```

---

## 3. 完整步骤（含验证）

### 3.1 重启前（可选，查故障时必须）

```cmd
cd /d D:\Projects\BargainProxy

rem 拷走当前错误日志，别让它被 move 覆盖
copy logs\gateway.err.log logs\gateway.err.log.before-restart

rem 记下当前进程（重启后对比）
powershell -NoProfile -Command "Get-NetTCPConnection -LocalPort 13140 -State Listen | Select-Object OwningProcess"
```

### 3.2 停止

```cmd
scripts\stop-all.cmd
```

期望输出：
```
[1/2] gateway (port 13140)...
[ok] stopped gateway pid 36592
[2/2] portable PostgreSQL...
waiting for server to shut down.... done
server stopped
```

**确认端口已释放**（约 1–3 秒，PG 更久）：
```cmd
powershell -NoProfile -Command "Get-NetTCPConnection -LocalPort 13140 -State Listen -ErrorAction SilentlyContinue"
```
无输出 = 已停。

### 3.3 启动

```cmd
scripts\start-all.cmd
```

期望输出（四步）：
```
[1/4] portable PostgreSQL...
[pg] already running on 127.0.0.1:55432        ← 若刚才停了会显示 started
[2/4] gateway binary...
[build] go build ./cmd/bargainproxy
[ok] bargainproxy.exe rebuilt 2026-11-13 21:58:45 — newer than every .go source
[3/4] panel state...
[ok] panel open mode
[4/4] gateway on 127.0.0.1:13140...
[ok] gateway up. Panel: http://127.0.0.1:13140/panel/
```

**若第 2 步报 build failed**：脚本会拒绝启动旧二进制并 `exit /b 1`。先修编译错误，**不要**绕过。

### 3.4 验证（必做，四项）

```cmd
scripts\verify-deploy.cmd
```

期望：
```
[1] exe mtime       : 2026-11-13 21:58:45
    [ok] no .go source is newer than the exe
[2] running process : pid 36592 started 2026-11-13 21:58:49
    [ok] process started AFTER the exe was built - the new binary is live
[3] port 13140 owner: pid 36592 (bargainproxy)
[4] /v1/models      : HTTP 200 (3526 bytes)
```

**这四项分别证明**：
1. 编译进去了（exe 比所有 .go 新）
2. 跑的是新进程（进程启动晚于 exe）
3. 13140 确实是它（不是别的残留进程）
4. 路由表加载成功

**任何一项不过都不算重启成功。**

### 3.5 功能抽验（改了什么就验什么）

最小探针（不烧额度、不打上游大请求）：

```cmd
rem 健康检查
curl -s -o NUL -w "health=%%{http_code}\n" http://127.0.0.1:13140/healthz

rem 真实走一次模型（会花一点额度）
curl -s -X POST http://127.0.0.1:13140/v1/messages ^
  -H "Content-Type: application/json" -H "anthropic-version: 3" ^
  -d "{\"model\":\"deepseek-v4.1-flash\",\"max_tokens\":100,\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"你好\"}]}"
```

查请求日志（只读）：

```cmd
go run ./scripts/pgq -dsn "postgres://bargainproxy@127.0.0.1:55432/bargainproxy?sslmode=disable" ^
  -sql "SELECT to_char(ts,'HH24:MI:SS') t, model, channel, account_label, status, error_class, ttfb_ms, duration_ms FROM request_logs WHERE ts > now() - interval '10 minutes' ORDER BY ts DESC LIMIT 10"
```

---

## 4. 只看证据，不看印象

判断「改动是否生效」**只能**用下面三条进程事实，截图和接口行为都不算（它们分不出新老二进制）：

| 判据 | 命令 |
|---|---|
| exe 比所有源码新 | `verify-deploy.cmd` 第 1 项 |
| 进程启动晚于 exe | `verify-deploy.cmd` 第 2 项 |
| 端口归属正确 | `verify-deploy.cmd` 第 3 项 |

**不要把以下当成生效证据**：
- 面板数字变了（可能是缓存）
- 某个请求返回 200（旧代码也可能返回 200）
- `bargainproxy.exe` 文件时间新（编译了但没重启进程）

---

## 5. 相关脚本与日志

| 路径 | 作用 |
|---|---|
| `scripts\start-all.cmd` | 起 PG + 编译 + 起网关（**端口占用时跳过启动**） |
| `scripts\stop-all.cmd` | 停网关（按 13140）+ 停 PG |
| `scripts\build.cmd` | **只编译**，并证明 exe 比所有 .go 新；从不停止/启动 |
| `scripts\verify-deploy.cmd` | 只读检查四项进程事实 |
| `scripts\pg\start.cmd` | 便携版 PG 启动（含旧进程退出等待） |
| `scripts\pg\stop.cmd` | 便携版 PG 停止 |
| `scripts\autostart.vbs` | 计划任务 `BargainProxy_BootStart` 调用的静默启动器（端口空闲才拉起全栈） |
| `logs\gateway.log` | 网关 stdout |
| `logs\gateway.err.log` | 网关 stderr（**每次启动截断**，上一代进 `.1`） |
| `logs\autostart.log` | 计划任务启动记录 |
| `.tools\pg.log` | 便携版 PG 日志 |

---

## 6. 不要做的事

- **不要**在没停进程的情况下跑 `start-all.cmd` 然后宣称重启完成 —— 它跳过了启动。
- **不要**在排查故障期间重启而不先备份 `logs\gateway.err.log` —— 它会被 `move` 掉，只留一代。
- **不要**直接 `taskkill` PG —— 用 `scripts\pg\stop.cmd`，否则下次启动可能撞数据目录锁。
- **不要**用面板改「首字窗口 / 队列等待」之类的参数后以为需要重启 —— 那些是热更新（`handleRestartParams` 名字是历史遗留，实际立即生效）。只有 Go 代码改动才需要重启。
- **不要**在没有授权的情况下重启 —— 网关承载着当前会话的模型流量，重启会中断它。按项目规则，重启是独立的显式授权动作。

---

## 7. 开机自启与看门狗

| 计划任务 | 何时注册 | 行为 |
|---|---|---|
| `BargainProxy_BootStart` | 面板「服务管理」开关开启（默认开） | 登录后运行 `scripts\autostart.vbs`：探测 13140，**未监听才**拉起全栈 |
| `BargainProxy_Watchdog` | 面板开关开启（默认关） | 每 5 分钟巡检复活 |

两者都是**端口空闲才启动**，所以它们同样不会「重启」正在运行的网关。

查状态：
```cmd
powershell -NoProfile -Command "Get-ScheduledTask -TaskName 'BargainProxy*' | Select-Object TaskName,State"
```
