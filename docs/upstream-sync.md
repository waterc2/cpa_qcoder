# 上游同步指南：qoder2api → CPA 插件

> **这份文档的用途**：本仓库的 `cpa-plugin` 分支 = 上游 qoder2api + 本地插件层。
> 检查更新时先 fetch 上游，有新提交就按本文档合并——**尤其要保住 §3 列出的本地语义**，
> 那些是排队熔断架构的承重墙。README 的「上游同步」节是合并日志，本文档是操作手册。
>
> 部署总览见 `/home/brady/tool/README-cpa.md`。
>
> 最近执行：**2026-11-18**（同日两轮）。第一轮：`origin/main` 新提交 `c8e0aea`「显示名 → 真 key
> （issue #13）+ 控制台冷却表格修复」**已合并**（merge `0e43945`，1 处冲突：README 取本地精简版）。
> 第二轮：上游无新提交，修**本地**排队语义——真队列时 `streamWithSlot` 内的同账号等待
> 不受 `MaxQueueWait` / `PerAccountQueueWait` 封顶（实测连等 6 次 ≈2m30s 不上抛），导致
> host 拿不到失败信号、**无法回落到 CodeBuddy**；同时插件从未调用
> `bridge.ApplyQueueSettings`，`settings.json` 的 `queue_*` 字段在插件里是空转（见 §3 第 7、8 条）。
> 版本 `0.1.8 → 0.1.9 → 0.1.10`，**已于 2026-11-18 21:09:36 部署（pid 2466 → 16843），
> 21:14:20 仅改排队 env 再重启一次（→ 18885）**：
> `QODER2API_QUEUE_MAX_WAIT 90s→15s`、`PER_ACCOUNT_WAIT 30s→10s`（只修 deadline 不够，
> host 会逐 qoder 账号重试，90s × 9 个会先耗尽客户端）。上游仍在排队时实测：修复前客户端
> `499 | 2m50s`，现在首个 16.4s、后续 1.49s / 0.94s，均落到 CodeBuddy。
> ⚠️ `start_cpa.sh` 里是 `${QODER2API_QUEUE_MAX_WAIT:-"90s"}`，未 export 就默默回到 90s/30s。
> 逐轮明细（含哪半边在插件路径上是死代码、两处有意覆盖上游的本地分歧）见 README
> 「上游同步」节 2026-11-18 两条。
> 逐轮明细（含两处有意覆盖上游的本地分歧、上游哪半边在插件路径上是死代码）见 README
> 「上游同步」节 2026-11-18 条；上一轮 2026-11-14（`0.1.8`，**当前线上版本**）同节。
> 线上 `settings.json` 的钉值（`traffic_guard=false`、`auto_throttle=false`、
> `session_affinity=true`+1800s、`max_output_tokens=131072`、`tool_result_bytes=1048576`、
> `tool_result_total_bytes=4194304`、`machine_salt` 勿动）以文件本身为准。

---

## 1. 项目结构与来源

| 角色 | 路径 / remote | 说明 |
|------|---------------|------|
| 上游 | `origin` = https://github.com/Zhengyuuuui/qoder2api | 独立的 Qoder API 网关（Go），`main` 分支 |
| 本地插件层 | `/home/brady/tool/qoder2api`，分支 **`cpa-plugin`** | 在上游之上叠加：`cmd/cpa-plugin/`（C ABI 插件）、`internal/bridge/queue.go` + `cmd/cpa-plugin/queuebreaker.go`（会话感知半开熔断）、调度器（每账号并发上限、最高优先级非 Qoder 回退）、用量日志、模型别名/开关 |
| 纯上游镜像 | 本地 `main` 分支 | 跟踪 origin/main 的普通克隆，可 `git pull` 更新后作对比参考 |
| 自有 fork | `mine` = git@github.com:waterc2/cpa_qcoder.git | 备份与分发；与 cpa-plugin 保持快进同步 |

- 插件对上游核心只做**最小补丁**：`account/oauth.go`、`account/region.go`、
  `internal/bridge/*`、`go.mod`（见 README「模块」节）。同步上游时按此清单解冲突。
- 插件自有数据目录：`~/.qoder2api-cpa/`（`models.json` = 模型开关/别名、
  `plugin_settings.json`、`usage/`、`logs/`）。

## 2. 检查更新

```bash
cd /home/brady/tool/qoder2api
git fetch origin
git fetch mine                       # 确认 fork 无本地没有的东西
git log --oneline <上次合并点>..origin/main   # 新提交清单（<上次合并点> 见 README 上游同步节）
git status -sb                       # cpa-plugin 应干净且领先 mine/main
```

有新提交 → 试合并看冲突规模：

```bash
git merge --no-commit --no-ff origin/main
git status --short | grep '^UU'      # 冲突文件
# 评估完可先 git merge --abort 回滚再正式做
```

## 3. 合并时**必须保留**的本地语义（2026-10-23 合并的实际清单）

上游每次改动都可能试图覆盖这些；解冲突时逐条核对：

1. **`internal/bridge/pool.go` `ShouldFailover`**：上游若加
   "`ErrTypeUpstreamQueue` → 一律不换号"的短路，**不采纳**。本地架构里同账号
   等待已在 `streamWithSlot` 内有界完成；错误若仍上抛，说明是假队列或单账号
   排队预算（`PerAccountQueueWait`）耗尽，外层账号池**必须能换号**。
   ⚠️ 2026-11-18：上面那句「有界完成」在插件部署里**此前是假的**——
   `streamWithSlot` 的等待循环只认 `QueueWaitBudget`（默认 30 分钟），外层算好的
   `MaxQueueWait` / `PerAccountQueueWait` 截止时刻在这一层完全不生效，真队列时
   实测同账号连等 6 次 ≈2m30s 仍不上抛 → host 拿不到失败信号 → **无法回落其它
   provider**（见第 7 条与 README 2026-11-18 条）。现已用 `deadline` 参数修正。
2. **`internal/bridge/queue.go` `queueWorthSameAccountRetry`**：真队列
   （queueCount>0 或 serviceAvailable）才消耗同账号重试；假队列
   （queueCount=0 且 serviceAvailable=false）直接上抛给 `streamWithQueueRecovery`
   立即换号。
   ⚠️ 2026-10-27（`6cae517`）：上游新增的「首字前一直等」
   （`QueueWaitFirstToken`，默认开）**去掉次数上限**，上游版本不再调用本 gate
   ——照抄会让假队列一直等到 `ctx` 取消，换号永不发生。本地在
   `streamWithSlot` 的「首字前一直等」判定**之前**先保留 gate；回归用例
   `TestStreamWithSlotFakeQueueIgnoresWaitFirstToken` 守住（去掉 gate 即 3s 后失败）。
   该用例必须用**纯 JSON** detail（见第 6 条 `pureQueueDetail` helper）。
3. **`account/model.go` `QuotaInfo`**：本地超集（`OrgResourcePackage` +
   `AvailableCredits` 汇总 + 注释）。上游对这块的改动往往只是 gofmt 重排。
4. **`internal/bridge/bridge.go` `CallOpts`**：本地 `ReasoningEffort` /
   `ThinkingBudget` / `SkipModelMap` 与上游 `ContextTokens` 并存，不是二选一。
5. **README**：保留精简版；上游的完整 README 不并入，只在「上游同步」节
   追加一条合并记录（范围 + 要点）。
6. **测试**：上游新增断言若与本地架构冲突（例：`TestShouldFailover_Queue`
   曾断言"队列错误不换号"），按本地语义改写并在注释里注明分歧原因。
   ⚠️ 测试细节：`parseQueueError` 需要纯 JSON 的 detail（生产路径 Detail 是
   HTTP body 原文，无日志前缀）；`errors_test.go` 的 `nestedResourceLimitDetail`
   helper 带 `"upstream 403: "` 前缀，只供 `parseUpstreamNestedError` 类测试用，
   不能直接喂给队列解析。
7. **`streamWithSlot` 的 `deadline` 参数（本地新增，2026-11-18）**：上游签名里
   **没有** `deadline`，合并时不要丢。它把外层的 `MaxQueueWait` / `PerAccountQueueWait`
   截止时刻传进同账号等待循环，并在 **sleep 之前**判定（等完再看会白等一整个
   `retryAfter`）。零值 = 不限，只由 `QueueWaitBudget` 兜底——`queue_budget_test.go`
   就是靠传零值把这一层单独测出来。回归用例：`TestQueueSameAccountWaitRespectsSlotDeadline`
   （去掉修复会一路睡到测试超时）。
8. **`cmd/cpa-plugin/queue_settings.go` `applyQueueSettingsFromSettings`（本地新增）**：
   上游只在 `service.go` / `main.go`（独立服务）里调 `bridge.ApplyQueueSettings`，
   插件没有监听端口也没有那条路径 ⇒ `settings.json` 的 `queue_*` 字段在插件里
   原本是**纯装饰**。上游合并不会补这个调用，别把它当冗余删掉。回归：
   `TestApplyQueueSettingsWiredIntoConfigure`（钉住 `configure()` 里的调用点在
   early-return **之前**）。

合并提交信息里写清：合了什么、解了哪些冲突、做了哪些语义适配
（参考 `eb30419` 的提交信息格式）。

## 4. 构建 / 部署 / 验证

```bash
cd /home/brady/tool/qoder2api
go test ./...                                  # 必须全绿
scripts/build-cpa-plugin.sh <新版本号>          # → dist/qoder-v<版本>.so
# ⚠️ 版本号注入已修复为 -X main.pluginVersion（旧脚本用完整包路径，ldflags
#    从未生效过，注册版本一直是假的 0.1.1）。版本号要递增，宿主按文件名
#    版本识别新插件。

# 部署（宿主手动换 .so 不支持热加载，需重启）：
cd /home/brady/tool/cli-proxy-api-app/plugins/linux/amd64
cp -p qoder-v<旧>.so qoder-v<旧>.so.bak-pre-<描述>-$(date +%Y%m%d-%H%M%S)
cp /home/brady/tool/qoder2api/dist/qoder-v<新>.so .
rm -f qoder-v<旧>.so

# 重启宿主：/home/brady/tool/start_cpa.sh，或手动 kill + nohup
# —— 手动时必须带全环境变量（见 README-cpa.md §4），尤其是：
#   QODER2API_QUEUE_MAX_WAIT=90s      QODER2API_QUEUE_PER_ACCOUNT_WAIT=30s
#   QODER2API_QUEUE_BREAKER_SESSIONS=1 QODER2API_QUEUE_BREAKER_COOLDOWN=10m
```

**部署后验证**（`cli-proxy-api-app/app.log`）：

```
pluginhost: plugin loaded plugin_id=qoder version=<新版本> path=.../qoder-v<新>.so
pluginhost: plugin registered plugin_id=qoder plugin_name=Qoder version=<新版本> ...
```

loaded 与 registered 的版本一致才说明 ldflags 生效。

## 5. 推送

```bash
git push mine cpa-plugin:main    # fork 备份（快进）
```

## 6. 与宿主配置的联动（坑）

- **`config.yaml` 的 `model-alias`（qoder 节）每条必须带 `fork: true`**。
  宿主语义：无 fork = 别名**替换**原名。2026-10-23 事故复盘：qoder 的
  `deepseek-v4.1-flash` 被无 fork 地改名成 `qoder-auto/performance/ultimate/efficient`
  后，裸名从 qoder 的对外列表里消失，全部流量静默落到 workbuddy——插件侧
  （`~/.qoder2api-cpa/models.json` 的 `dfmodel → deepseek-v4.1-flash` 别名）
  一直是好的。改宿主 `model-alias` 前先想清楚"原名还要不要暴露"。
- 插件模型开关/别名在 `~/.qoder2api-cpa/models.json`（面板可编辑）；denylist
  的 key 是上游模型 key（`dfmodel`、`qfmodel`…），改名后旧 key 会失配。
- 队列行为由环境变量控制（见 §4），熔断器日志形如
  `qoder queue breaker: diverting new session ...`，属设计行为。
