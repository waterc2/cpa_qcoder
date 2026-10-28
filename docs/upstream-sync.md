# 上游同步指南：qoder2api → CPA 插件

> **这份文档的用途**：本仓库的 `cpa-plugin` 分支 = 上游 qoder2api + 本地插件层。
> 检查更新时先 fetch 上游，有新提交就按本文档合并——**尤其要保住 §3 列出的本地语义**，
> 那些是排队熔断架构的承重墙。README 的「上游同步」节是合并日志，本文档是操作手册。
>
> 部署总览见 `/home/brady/tool/README-cpa.md`。
>
> 最近执行：**2026-10-27**，合并上游 `872d9b9..6cae517`（队列「首字前一直等」
> 开关 + 最近排队等待记录）。上游改动与本地语义 #2 直接冲突，已加回
> `queueWorthSameAccountRetry` gate 并补回归用例
> `TestStreamWithSlotFakeQueueIgnoresWaitFirstToken` → 行为变更，已重新构建。
> 上一次实代码合并为 2026-10-23 的 `2671a21..62facdb`（context 档位
> 200K/400K/1M + 排队重试补全）。

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
