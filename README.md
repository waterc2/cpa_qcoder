# cpa_qcoder

[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) 的 **Qoder** 插件。
把 [Qoder](https://qoder.com) 作为 CPA 原生 provider 接入：账号、路由、调度、
用量统计、认证 UI 全部复用 CPA，插件只补 Qoder 特有的部分。

账号以普通 CPA auth 文件保存（一个 Qoder 账号一个文件），因此 CPA 的调度器、
认证界面、模型别名、用量统计都直接生效。

基于 [qoder2api](https://github.com/Zhengyuuuui/qoder2api) 的 bridge 核心二次开发。

## 功能

- OAuth 设备登录、PAT 导入，设备令牌自动刷新
- 模型列表（静态 + 按账号动态）、模型开关
- OpenAI `chat/completions`（流式 / 非流式）
- 额度查询（含专属资源包）、每日签到（手动 + 定时）
- **新版推理协议**：账号配置真实 `checkin_fingerprint` 后，`model/list` 额外解锁
  Sonus（`smodel`）/ Cantus（`cmodel`）/ Fast（`fast`）；未配置的账号保持旧协议
- 上游 `RESOURCE_LIMIT`、排队（`10605 isQueued`）等错误返回可读原因
- Token 续期沿用宿主的 `auth.refresh` 定时回调（OAuth 每 24 小时检查）
- 内嵌管理面板

## 构建

需要 Go（cgo）和 C 工具链：

```bash
scripts/build-cpa-plugin.sh 0.1.1
# -> dist/qoder-v0.1.1.so (+ .h)
```

## 安装

```bash
cp dist/qoder-v0.1.1.so /path/to/cli-proxy-api/plugins/linux/amd64/
```

在 `config.yaml` 里启用：

```yaml
plugins:
    enabled: true
    configs:
        qoder:
            enabled: true
            # 可选
            checkin_auto: false
            checkin_times: ["10:00"]
            checkin_on_start: false
            default_region: global
```

重启 CPA。面板地址：`/v0/resource/plugins/qoder/panel`。

## 添加账号

**OAuth（推荐）**：CPA 管理界面 **Add auth → Qoder**。插件驱动设备登录：返回登录
链接，浏览器授权后由宿主轮询拿到 token，最终存成普通 CPA auth 文件。

**PAT / 手动导入**：创建 auth 文件（或在面板导入）：

```json
{ "type": "qoder", "secret": "pt-xxxxxxxx", "region": "global" }
```

OAuth 凭证也可直接导入：

```json
{ "type": "qoder", "device_token": "dt-xxxx", "refresh_token": "drt-xxxx", "region": "global" }
```

凭证会先校验再保存；`region` 默认 `global`，国内账号用 `cn`。

## 管理 API

所有路由需要 CPA 管理密钥（`Authorization: Bearer <key>`），基址
`/v0/management/plugins/qoder`。

| Method | Path | 说明 |
|---|---|---|
| GET | `/accounts` | 账号列表（region、plan、签到天数） |
| POST | `/checkin` | 签到：`{account_id}` 单账号，留空全部 |
| GET | `/account/quota?account_id=…` | 单账号实时额度 |
| POST | `/import` | 导入 `{secret, region, name}` |
| POST | `/accounts/delete` | 删除 `{account_id}` |
| GET/POST | `/checkin/config` | 读取/修改自动签到配置 |
| GET/POST | `/models/config` | 读取/保存模型开关 `{"disabled":[…]}` |

## 模型开关与签到

面板 **模型管理** 打开全部可选模型的勾选网格；取消勾选即从 `/v1/models`
隐藏并不可路由。开关以 denylist 存在 `<data-dir>/models.json`，新发现的模型
默认可见。与宿主的 `oauth-excluded-models` 叠加生效。

签到走 `campaigns` 流程，支持手动/定时、本地连续天数统计。国际版账号可能需要在
auth 文件里提供官方客户端的 `checkin_fingerprint`；国内账号则**不能**发送自造指纹。

### 新版推理协议（Sonus / Cantus / Fast）

上游按 `(cosy-clienttype, cosy-version)` 下发模型清单。插件在账号 auth 文件里读到
真实设备指纹后，会把该账号切到官方桌面端同款的新版协议；没有指纹的账号继续走旧协议，
行为不变。

```json
{
  "type": "qoder",
  "secret": "dt-...",
  "region": "global",
  "checkin_fingerprint": {
    "machine_id": "抓包得到的 UUID",
    "machine_token": "与 machine_id 相同",
    "machine_type": "5",
    "client_ip": "抓包得到的 UUID",
    "client_type": "10",
    "version": "1.1.69",
    "scene": "app",
    "product": "app",
    "data_policy": "agree"
  }
}
```

`machine_token` 是切换开关；只填 `machine_id` 不会改变协议。指纹必须来自官方客户端
抓包，自造值会被上游以 `RESOURCE_LIMIT` 拒绝。切换后 `model/list` 会从 13 个模型
增加到 15 个，新增 Sonus / Cantus / Fast。Sonus 倍率为 8，热门时段返回
`10605 isQueued` 属正常容量排队，换模型或稍后重试即可。

## 数据布局

| 路径 | 内容 |
|---|---|
| `~/.cli-proxy-api/qoder-*.json` | 账号凭证（CPA auth 文件，权威存储） |
| `~/.qoder2api-cpa/models.json` | 模型开关 |
| `~/.qoder2api-cpa/plugin_settings.json` | 面板改过的配置 |
| `~/.qoder2api-cpa/checkin_history.json` | 本地签到历史 |
| `~/.qoder2api-cpa/logs/` | 插件日志 |

数据根可用 `QODER2API_HOME` 覆盖。

## 模块

```
cmd/cpa-plugin/
  main.go        C ABI + RPC 分发 + 注册 + 配置
  auth.go        凭证解析、auth.parse/login/refresh
  refresh.go     设备令牌刷新
  settings.go    面板配置持久化
  modelstate.go  模型开关 denylist
  models.go      model.static / model.for_auth
  executor.go    execute / execute_stream + host 流桥
  checkin.go     签到核心 + 调度
  management.go  管理路由 + host auth RPC
  panel.go/.html 内嵌面板
```

插件复用 `qoder2api/account`、`internal/cosy`、`internal/bridge`；上游核心只保留
最小补丁（`account/oauth.go`、`account/region.go`、`internal/bridge/*`、`go.mod`），
同步上游时按此清单解决冲突。

## 上游同步

- 2026-11-09：合并 `ce6ae9d..adfb822`（`feat(bridge): traffic guard, risk-control
  hardening + security audit fixes`——账号池风控 + 本地封控一整轮，含随提交一起进来的
  `docs/audit-2026-11.md` 安全审计 P1/P2/P3 修复；该审计自己的总结论是「**不建议直接上
  线**（P0=0，P1=5）」，adfb822 就是把那 5 个 P1 补上的提交）。merge `afc8e53`，7 处冲
  突（`README.md`、`account/oauth.go`、`account/region.go`、`checkin.go`、
  `internal/bridge/bridge.go`、`client.go`、`pool.go`）。版本 `0.1.6 → 0.1.7`
  （`cmd/cpa-plugin/main.go` `pluginVersion`），构建 `dist/qoder-v0.1.7.so`，**已于 2026-11-10 00:19 部署**——但**先**把下条「部署前必读」里那几项**显式**写进 settings.json 再装产物，所以这次部署并没有改变出站形态。
  **冲突处理（本地语义优先，§3 承重墙不动）**：
  1. `ShouldFailover` **不**对 `ErrTypeUpstreamQueue` 短路。上游把排队错误归进「一律不
     换号」，本地架构相反：池层保持可换号，「不换号」由同账号等待路径 +
     `queueWorthSameAccountRetry` 完成。上游为此新增的回归用例（凭证/额度类断言）按本地
     语义改写：`ErrTypeTokenExpired` / `ErrTypeModelNotAllowed` 仍然换号，停号交给
     `ClassifyFail` 的 `needs_reauth` / `FailClassModelNotAllowed`。
  2. 本地 `streamWithQueueRecovery` 保留，把上游新的端点候选与故障转移（`endpoints.go`）
     接进去；`callGetCtx` 保持 context 感知并嫁接上游端点 failover；`InferenceBase` 保留。
  3. `CallOpts` 本地字段全保留，新增上游 `SessionKey`（会话亲和的键）。
  4. 导出 API `account.BuildAccountFromToken` 与可测形态 `ParseQuotaPayload(result, plan)`
     保留（插件侧调用点依赖）。
  5. 上游区域感知 HTTP 客户端（`RegionHTTPClient`，P1-5：签到/换 token/查额度与对话共用
     Transport）采纳；本地 **per-slot 签到重试状态机**保留，并接上游 per-account 错峰
     `CheckinAllAt(base)`；`pollToken` 调用点补上新增的 `region` 参数。
  6. `queueEnvelopeFrame` 补 `queueCount` / `serviceAvailable` 两个字段——本地语义下
     **只有真队列才排队**，信封若不带这两个字段，上游形态的假队列
     （`queueCount=0 && serviceAvailable=false`）在本地会被一路等到客户端断开。
  7. 上游测试跟改：`streamWithSlot` 参数已是 `[]string` URL 列表；队列预算测试改为直接
     跑 `streamWithSlot`（插件路径不经他们的 HTTP 层）；顺手修了上游一处 `logger.Warn`
     双重格式化。
  验证：`go build ./...`、`go vet ./...`、`go test ./...` 全绿；`-race` 覆盖根包、
  `internal/bridge`、`cmd/cpa-plugin` 全绿；产物内版本用 `strings` 严格匹配确认 0.1.7。

  **部署前必读（`0.1.7` 会改变出站行为）**——上游把三个保护开关从 `bool` 改成 `*bool`
  并声明 **缺字段 = 默认开**（审计 P1-3：旧行为是「宣称有保护、实际默认没开」），而线上
  `~/.qoder2api-cpa/settings.json` 里**没有**这三个字段，所以「部署即开」：
  - **流量闸门**（`traffic_guard`）：默认 `DefaultConcurrencyPerAccount = 1`——**单账号完全
    串行**；相邻请求强制最小间隔 400ms + ≤120ms 真随机抖动；闸门等待默认 30s；过载时软
    放行上限 = limit×2（最少 2 个），超过即**拒流**。多客户端同时用 Qoder 时要先按现状
    并发把 `concurrency_per_account` 写进 settings.json，否则表现为「请求排队 30s 后被拒」。
  - **自适应节流**（`auto_throttle`）：生效并发 = min(用户配置, 自适应)，生效间隔 =
    max(用户配置, 自适应)——**只严不松**（窗口 300s、排队率 5%、频控率 2%、降档间隔
    1500ms）。
  - **上游自带会话亲和**（`session_affinity`，TTL 1800s，排队等待期间不换号即不换绑定）：
    与本地 `QODER2API_QUEUE_BREAKER_SESSIONS=2`「连续排队就换号」是两条独立机制，上线前
    必须先确认两者不打架。
  - `max_tokens` 默认夹到 **32768**（`ClampMaxTokens` 在共享 bridge 请求构造里，插件路径
    **会**经过，超限下调并打日志、不拒请求）；`role=tool` 结果默认**单条 32KB / 总和
    128KB 截断**（`BuildQoderMessages` → `truncateToolResults`，插件路径**会**经过）——
    编码代理的长工具输出会被静默截断，上线前要么调大、要么显式关。
  - `BodyLimitMiddleware`（29MB → 413）**不生效**：它只挂在他们自己的 HTTP server 上，
    插件没有监听器。**但「排队总预算」这条先前的判断是错的，已按实测纠正**：插件确实从不
    调 `ApplyQueueSettings`，所以取包内默认 1800s——而它管的是 `streamWithSlot` 里
    **同账号排队重试循环**的总时长，`QODER2API_QUEUE_MAX_WAIT=90s` 管的是**外层换号**，
    两条是不同的上限，不是同一条被谁盖住。部署后线上日志确认循环按 `budget 30m0s` 走；
    而 `0.1.6` 在同一条循环上**根本没有总预算**（`QueueWaitFirstToken=true`，只有 ctx
    取消才停），所以这条对我们是**收紧**，不是放松。
  - **设备指纹不漂移**：`machine_salt` 早在 `f8037f5` 就已在用、线上 settings.json 里
    2026-10-17 就有盐值，本次只是加 `QD_NO_MACHINE_SALT` 逃生口（P1-2，回退到与升级前
    逐字节一致的无盐基准）+ 指纹漂移告警，**无需设 `QD_DEVICE_ID_LEGACY`**。

  **部署与验证（2026-11-10 00:19）**：
  - 先备份 `~/.qoder2api-cpa/settings.json` → `settings.json.bak-pre-qoder-0.1.7-defaults-20261110-001900`
    （`plugin_settings.json` 同批备份），再写入显式值把出站形态钉在 `0.1.6`：
    `traffic_guard=false`（`guardApplies` 让个人 Global 账号**完全跳过租约**：不等、不隔、不拒流；
    仅团队/企业账号仍受保护）、`auto_throttle=false`、`session_affinity=true` + `ttl 1800`
    （**这一项有意开**：软偏好，绑定账号冷却/熔断时自然回落普通选号，与本地队列熔断不争；
    换账号 = 前缀缓存全废 = 全价积分）、`max_output_tokens=131072`、`tool_result_bytes=1048576`、
    `tool_result_total_bytes=4194304`。`machine_salt` 原样保留——丢掉它就是指纹漂移。
  - 产物：`qoder-v0.1.6.so` 改名 `qoder-v0.1.6.so.bak-pre-adfb822-traffic-guard-20261110-001905`
    （同目录只能有一个 `qoder-*.so`，改名同时完成备份与卸载），拷入 `qoder-v0.1.7.so`。
  - 重启：先 `tr '\0' '\n' < /proc/<旧pid>/environ` 读回线上 env（`QODER2API_QUEUE_MAX_WAIT=90s`、
    `PER_ACCOUNT_WAIT=30s`、`BREAKER_SESSIONS=2`、`BREAKER_COOLDOWN=10m`），`kill` + `nohup` 纯重启，
    旧 `app.log` 改名保存而不是截断。
  - 验证：`plugin registered plugin_id=qoder version=0.1.7`；9 个 Global 账号 auth 重新载入；
    `GET /v1/models` 200/69；**指纹审计 9 个账号全部 `stable derive`、零漂移告警**（证明与
    `0.1.6` 同一套派生，`QD_NO_MACHINE_SALT` 无需设置）；无 panic/error；闸门按配置对个人号
    不生效，日志里没有 guard/throttle 动作，符合预期。
  - 部署后实况：`qfmodel` / `dfmodel` 正被上游排队（`CALL_COUNT_LIMIT`，前方约 1 人、每次仍说
    还要 60s，与 2026-11-02 记的拥塞一致）。顺带实测确认：**客户端断开后 host 不会取消插件侧
    请求**，所以被弃的排队等待会一直跑到预算耗尽——本次冒烟超时留下的两个等待约在 00:50 自行结束。

**上游计价变化**（无新提交，`origin/main` 仍是 `8d49aa6`）。实时
  `/algo/api/v2/model/list`（9 个部署中的 Global 账号一致）显示 `dfmodel`
  DeepSeek-Flash `price_factor` **0 → 0.1**——国际版 DeepSeek 免费档已消失，
  唯一 0 价模型只剩 `qfmodel`（Qwen3.8-Flash）。同步动作：
  1. `fallbackModels` 兜底表按实测刷新（`dfmodel` 0.1、`ultimate` 2.0、
     `qmodel_38max` 0.5、`kmodel_latest` → Kimi-K3.1-Pro / 1.0），并补
     `TestFallbackDeepSeekNotFree` 防止再写回 0（写回会让面板把计价的
     DeepSeek 继续标成免费）。
  2. 运行时开关 `<data-dir>/models.json`：`dfmodel` 继续挂
     `deepseek-v4.1-flash`（现在本身就是计价模型，0.1 是最便宜的 DeepSeek），
     放开 `dmodel`（DeepSeek-V4-Pro，0.5）并挂别名 `deepseek-v4-pro`，
     需要 Pro 档时客户端显式点名。
  3. 实测拥塞：同一时刻 `dfmodel` / `dmodel` / `qfmodel` / `gmodel` 对 Free 计划
     账号一律 `subCode=CALL_COUNT_LIMIT` 排队，切付费 id 不解决拥塞。
  行为变更 → 构建 v0.1.6 部署重启。详见 `docs/upstream-notes.md`。


- 2026-10-30：合并 `6cae517..8d49aa6`（CALL_COUNT_LIMIT 排队报文被预筛选漏掉）。
  上游把 `parseUpstreamNestedError` 预筛选从匹配带引号的 `"subCode"` 改为匹配
  不带引号的 `subCode`——内层 JSON 嵌在外层 message 字符串里、引号被转义成
  `\"subCode\"`，导致与 `RESOURCE_LIMIT` 同构的 `CALL_COUNT_LIMIT`（带
  `isQueued:true`/`retryAfterSeconds`）被误判普通 `upstream_error`，误冷却+换号、
  同账号等待不生效。修复后真排队报文正确进入排队路径。
  **分歧处理（本地语义优先）**：上游新增回归用例
  `TestFriendlyUpstreamError_CallCountLimitQueued` 断言排队错误在
  `ShouldFailover` 一律不换号——与本地架构相悷（见上条 2026-10-27 记录），
  已改写为本地语义：池层保持可换号，不换号由同账号等待路径完成；
  并补充 `queueWorthSameAccountRetry`（真队列）断言。
  README 冲突按本地精简版解决，未并入上游完整 README。

- 2026-10-27：合并 `872d9b9..6cae517`（队列「首字前一直等」开关 + 最近排队等待
  记录）。上游把 10605 `isQueued` 从「按次数封顶重试」改为可无限等待，并新增
  `wait-log` 环形缓冲 + `/api/queue/log` 与控制台面板。
  **冲突处理（本地语义优先）**：上游版的 `streamWithSlot` 丢掉了
  `queueWorthSameAccountRetry` gate——「首字前一直等」一旦生效，假队列
  （`queueCount=0 && serviceAvailable=false`）会被它一直等到客户端断开，外层
  账号池就再也不换号了。本地保留 gate，并把它放在「首字前一直等」判定**之前**，
  假队列永远先上抛去换号；新增回归用例
  `TestStreamWithSlotFakeQueueIgnoresWaitFirstToken`（去掉 gate 即 3s 后失败）。
  README 冲突按本地精简版解决，未并入上游完整 README。
  注意：`ApplyQueueSettings` 签名新增第 4 个参数，本地调用点（`service.go`、
  `main.go`）已全部更新。

- 2026-10-26：合并 `62facdb..872d9b9`（纯文档/面板，无 Go 代码改动）。
  上游刷新模型清单快照：`kmodel_latest` Kimi-K3 → **Kimi-K3.1-Pro**（key 不变，
  倍率 1.4→1）、CN 移除 `qmodel`（11→10 个）、Global `gfmodel` max_output
  16,384→32,768、CN `qmodel_38max` 倍率 0.2→0.5；`webconsole/index.html` 的
  NewAPI 模型数组同步为当前 key。模型清单本就运行时从 `/algo/api/v2/model/list`
  动态拉取，故本次不改代码；本地静态兜底表（`cmd/cpa-plugin/models.go`）仍是
  2026-10-20 快照，仅在探测失败时使用，本次未同步（见下条待办）。

- 2026-10-23：合并 [qoder2api](https://github.com/Zhengyuuuui/qoder2api)
  `2671a21..62facdb`，含 **context 多档位（200K / 400K / 1M）** 与排队重试补全
  （`errors.go`/`pool.go`）。档位可经 `context_tier_default` / `context_tier_overrides`
  配置，按模型目录 5 分钟缓存解析为 token 数后写入 `model_config.max_input_tokens`
  （上游标注：该传递字段未实测验证）。插件侧保留本地 `AvailableCredits` 汇总与
  reasoning_effort / thinking_budget 透传，无行为冲突。

- 2026-10-23：合并 [qoder2api](https://github.com/Zhengyuuuui/qoder2api)
  `8e02f24..2671a21`，含新版推理协议（Sonus/Cantus/Fast）、Token 自动续期、
  `RESOURCE_LIMIT` 友好错误与注册研究文档。插件侧已把 `checkin_fingerprint`
  接入 `NewBridgeWithFP`，并在配置指纹时补充 Sonus/Cantus/Fast 的静态回退清单。
- 2026-10-20：合并 `4789816..8e02f24`，含专属资源包（活动积分）额度解析、
  账号签到灰度差异与模型清单快照。细节见 [docs/upstream-notes.md](docs/upstream-notes.md)。
- 2026-10-20：回退模型清单同步到上游快照（`mmodel` 更名、价格/推理标记/上下文），
  并把 CLIProxyAPI SDK 从 v8.0.32 升到 v8.0.35（与当前宿主一致）。