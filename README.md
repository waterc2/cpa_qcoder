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

- 2026-11-18（同日第二轮）：上游 `origin/main` **无新提交**（`c8e0aea` 已是最新）。
  本轮修**本地语义缺陷**——现场：调用方请求 `deepseek-v4.1-flash` 挂 2.5 分钟后自己断开
  （`app.log`: `499 | 2m27s`），而同一时刻 `deepseek-v4.1-flash-sg`（CodeBuddy）实测 **200 / 1.77s**。
  **两个独立问题**：

  1. **真队列时同账号等待不封顶 → host 无法回落其它 provider**。
     `streamWithSlot` 的等待循环只看 `QueueWaitBudget`（默认 **30 分钟**），而外层
     `CallQoderWithOpts` 算好的 `queueDeadline`（`QODER2API_QUEUE_MAX_WAIT`）与
     `slotDeadline`（`QODER2API_QUEUE_PER_ACCOUNT_WAIT`）传进了 `streamWithQueueRecovery`
     却**从没传进这个循环**。插件日志坐实：
     `20:42:29 try 1 … budget 30m0s` → `20:45:02 try 6, waited ~2m30s, budget 30m0s`。
     插件不返回错误，host 就**拿不到失败信号**，永远轮不到 CodeBuddy；而 breaker
     （`BREAKER_SESSIONS=2`）只能转**新会话**，管不了已挂住的那个。
     **修**：给 `streamWithSlot` 加 `deadline` 参数，在 **sleep 之前**判定
     （等完再看会白等一整个 `retryAfter`）；零值 = 不限，保持 `queue_budget_test.go`
     能单独测预算这一兜底。效果：上游真排队时最多等 `PerAccountQueueWait`/`MaxQueueWait`
     就把 503 交给 host，由 host 回落到 CodeBuddy（对照组：15:25:59 假队列 → 503 →
     **5.1s 内 host 落到 workbuddy 拿 200**）。
     回归：`TestQueueSameAccountWaitRespectsSlotDeadline`（**拆掉修复就会一路睡到测试超时**）、
     `TestQueueDeadlineDoesNotCutShortWhenGenerous`（反向护栏：宽松截止时仍照常重试，
     不能把“逢队列就秒失败”当成修好了）、`TestQueueRecoverySurfacesQueueErrorToCaller`。
  2. **`settings.json` 的 `queue_*` 字段在插件模式里是纯装饰**。
     `bridge.ApplyQueueSettings` 是改这些包级变量的唯一入口，而它**只被 `service.go` /
     `main.go`（独立服务）调用**，`cmd/cpa-plugin/` 里从没调过；插件也没有监听端口/控制台
     去触发它。⇒ operator 写进 `settings.json` 的排队参数全部静默失效，行为永远等于包级默认
     （`QueueWaitFirstToken=true`、`QueueWaitBudget=30m`）。
     **修**：`applyQueueSettingsFromSettings()` 在 `configure()` 里下发（**在 `len(raw)==0`
     的 early-return 之前**，空 config 也要生效）+ 启动日志把 `max_wait` / `per_account_wait`
     一起打出来。**没改任何默认值**（缺字段仍回 bridge 默认，`TestApplyQueueSettingsAbsentKeepsBridgeDefaults`
     钉住），只是让显式配置真的能到插件。
     回归：`TestApplyQueueSettingsFromSettings` + `TestApplyQueueSettingsWiredIntoConfigure`
     （后者钉住**接线本身**——把 `configure()` 里的调用注掉就失败，因为真正的回归就是“忘了调”）。

  **注意**：这不是“跳过 Qoder”。Qoder 仍是裸名 `deepseek-v4.1-flash` 的首选（`dfmodel` 单价更低），
  只是排队时不再占住请求 30 分钟。要调快/调慢：`QODER2API_QUEUE_MAX_WAIT` /
  `QODER2API_QUEUE_PER_ACCOUNT_WAIT`（env，需重启）或 `settings.json` 的 `queue_*`（本轮起生效）。
  §3 承重墙未动，并已把新增的 `deadline` 参数与 `queue_settings.go` 列入§3 第 7、8 条（上游签名里没有，
  合并时不要丢）。行为变更 ⇒ `0.1.9 → 0.1.10`。

  **部署（2026-11-18 21:09 / 21:14）**：`qoder-v0.1.8.so` 改名为
  `qoder-v0.1.8.so.bak-pre-queue-deadline-20261118-210932.so`，拷入
  `qoder-v0.1.10.so`（sha256 `db91d1ce…020aeba4`，与构建产物一致；`go version -m`
  为 `vcs.revision=1387348`、`vcs.modified=false`、`-X main.pluginVersion=0.1.10`，
  产物内 `0.1.9`/`0.1.8` 字样均为 0）。**两次重启**（均从 `/proc/<pid>/environ` 读回原环境，
  日志先备份，均经 `cpa-restart.sh` 同语义路径）：
  1. 21:09:36 加载新插件：pid 2466 → **16843**，`plugin loaded` 与 `registered` 都是 **0.1.10**，
     host 8.0.78 不变（`/proc/<pid>/exe` 无 `(deleted)`），healthz 200，`/v1/models` 64。
  2. 21:14:20 **仅改排队 env**：pid 16843 → **18885**。
     `QODER2API_QUEUE_MAX_WAIT 90s → 15s`、`QODER2API_QUEUE_PER_ACCOUNT_WAIT 30s → 10s`
     （breaker 两项不变：`2` / `10m`）。因为只修 deadline 还不够：host 拿到 503 后会
     **接着试下一个 qoder 账号**，9 个账号 × 90s 会把客户端耗死（实测第一次裸名请求
     150s 超时、插件侧 `499 | 2m50s`）。

  **效果（实测，上游仍在排队期间）**：
  | 阶段 | 结果 |
  |---|---|
  | 修复前（0.1.8，真队列） | 同账号 `try 6 ≈2m30s` 不上抛，客户端 `499 \| 2m50s`；`-sg` 同刻 200/1.77s |
  | 只修 deadline（MAX_WAIT=90s） | 200，但 **137s** 才落到 CodeBuddy（host 逐账号累加） |
  | + env 收紧（15s/10s） | 首个请求 **16.4s**，后续 **1.49s / 0.94s**，全部 `served by deepseek-v4.1-flash-sg` |
  | breaker | 21:17:03 `level=1 cooldown_until=21:27:03`；21:17:17 `diverting new session … auth=workbuddy-095b7a22…` |

  即：**Qoder 仍是首选**（breaker 冷却 10m 到点后会再试，上游恢复就拿回流量），只是排队时
  不再拿住请求。启动日志现在会直接打出生效值：
  `queue slot deadline for this plugin: max_wait=15s per_account_wait=10s`。

  ⚠️ **两个坑，下次重启前必看**：
  - `/home/brady/tool/start_cpa.sh` 写的是 `${QODER2API_QUEUE_MAX_WAIT:-"90s"}` 这类**默认值**。
    `cpa-restart.sh` 继承存活进程环境（会带走 15s/10s），但若有人直接跑 `start_cpa.sh`
    且 shell 里没 export，就会**默默回到 90s/30s**。本轮**没有改** `start_cpa.sh`，要么 export
    要么自己改默认值。本轮只修 deadline 不收紧 env 也能回落，只是会慢到分钟级。
  - 重启会清空 session-affinity（插件侧 1h TTL）与 breaker 内存态：本轮上游拥塞期间，
    重启后第一个请求要重新走一遍「排队→放弃→回落」（实测 ≈16s）才能重新让 breaker 转走。

- 2026-11-18：合并上游 `5bd4083..c8e0aea`（`fix(bridge): map display names to real keys
  (issue #13) + fix cooldown table layout`，上游 2026-11-19）。merge `0e43945`，**1 处冲突**
  （README 取本地精简版）。**已构建 `dist/qoder-v0.1.9.so`，尚未部署**（等确认）。
  **修的是什么**（issue #13「调模型经常排队甚至 429；即使调用成功 qoder 也不显示有额度」）：
  `MapModel` 以前只认 Claude/GPT/Gemini 关键字，其余一律 `ToLower` 后原样发给上游。
  客户端传**显示名**（`Qwen4-Max-Preview`）时上游收到的是一个不存在的 key，而**上游对未知
  key 走兜底返回内容、但不把这次调用计入 quota**——所以「请求成功却没用量」，且这类流量
  没有真实配额支撑，更容易被限流 → 排队 / 429。新增第 6 步 `upstreamDisplayNameAliases`
  （显示名 / 简写 → key，歧义时宁可不猜）+ `IsKnownUpstreamKey`（未知 key 打 **WARN**，
  只告警不拦截）。
  **本地语义零受损**：§3 承重墙（`pool.go` `ShouldFailover` / `queue.go`
  `queueWorthSameAccountRetry` / `account/model.go` `QuotaInfo` / `CallOpts`）本轮上游零改动，
  `bridge.go` 自动合并后 `SkipModelMap` 与新增 WARN 的先后关系正确（WARN 在映射之后，
  插件路径 `SkipModelMap=true` 时校验的是插件解析出来的真 key）。
  **两处本地分歧（有意覆盖上游）**：
  1. `knownUpstreamKeys` 补 `performance` / `qmodel`。上游快照漏了这两个真实 SKU，而插件
     回退清单（`cmd/cpa-plugin/models.go`）就在 advertise 它们（Global 当前不下发、CN 仍会
     协商到）。不补的话，一旦重新启用 `performance` 就会**每请求**打一条「不计 quota」的
     假警报，把真信号淹成噪声。`TestAdvertisedModelsAreKnownKeys` 锁死这条不变式。
  2. `defaultModelMapping` 上「本表故意不认显示名」的旧注释已改正（显示名现在由第 6 步兜底）。
  **新增测试**：`internal/bridge/model_alias_test.go`（上游）+ `cmd/cpa-plugin/model_knownkey_test.go`
  （本地：本插件 advertise 的每个 key 必须是已知上游 key；每个 advertise 的**显示名**必须落回
  自己那一档的 key——落错档会烧别的模型的额度，比原 bug 更糟）。
  **本轮不生效的部分**：`webconsole/index.html` 的冷却态表格错位 / `16m14s` 时长格式 / 模型
  冷却列明细——插件没有监听端口，控制台那半边在插件路径上是死代码（合入只为不让分支与
  上游越拉越远）。上游别名表里指向已下线 key 的三行（`qwen3.7-plus`→`qmodel`、
  `qwen3.7-flash`→`q37fmodel`、`glm-5.2`→`gm51model`）保留但不进 `knownUpstreamKeys`：
  命中它们时那条 WARN 是**对的**（下线的 key 本就不计费）。
  **部署前须知**：本轮无新增「缺字段=默认开」的配置项（上游只动 `bridge.go` /
  `model_alias_test.go` / `webconsole` / README，`settings.json` 字段零变化）；`go test ./...`
  全绿（`internal/bridge` 82.9s）。

- 2026-11-14：合并上游 `adfb822..5bd4083`（`feat(bridge): refresh upstream model list
  + fix re-auth wiping device fingerprint`，上游 2026-11-13 CN / 2026-11-14 Global
  实拉）。merge `7b8cbb3`，3 处冲突。**已于 2026-11-14 19:49:47 部署**：旧产物改名
  `qoder-v0.1.7.so.bak-pre-5bd4083-catalog-20261114-194924`，拷入
  `qoder-v0.1.8.so`（sha256 `666f1f9b…ce4816`，与构建产物一致），从
  `/proc/2595/environ`+`cmdline` 读回原环境/原 argv 后 `kill -TERM` + 纯重启（不经
  `start_cpa.sh`，不触发 host 更新检查；pid 2595 → **85036**，`app.pid` 已更新，
  `QODER2API_QUEUE_*` 四项原样：`MAX_WAIT=90s` / `PER_ACCOUNT_WAIT=30s` /
  `BREAKER_SESSIONS=2` / `BREAKER_COOLDOWN=10m`）。重启前先备份
  `app.log.bak-pre-deploy-0.1.8-20261114-194924`。
  **验证**：`plugin loaded` 与 `plugin registered` 都是 **`version=0.1.8`**（ldflag 生效）；
  4 个插件全部重新注册（qoder 0.1.8 / workbuddy 0.16 / clinepassbridge 0.4.7 /
  grok2api-egress 1.2.0）；3002 在听；`GET /v1/models` 200/63；9 个 qoder auth 全部载入；
  本轮**无任何 error/panic**（`model registration … superseded during computation` 那条
  warn 上一轮启动就有，host 侧噪声，与插件版本无关）；**指纹零漂移**——重启前后 9 个
  `machineid` 完全一致（这正是本轮合入的 `preserveDeviceIdentity` 需要盯的点）。
  **`jmodel` 实际并没有暴露**（与上面「部署前必读」的担心相反）：`/v1/models` 63 个
  条目里没有 `jmodel`——插件拉到的**活**清单（`ListAvailableModels`，本轮新改的逐账号
  区域限域试错路径，启动时对 9 个账号都跑通了）本来就没有下发 `jmodel`，运行时
  `models.json` 的 `disabled` 是否列它就无关紧要了；新兜底表只在「首查前 / 上游不可
  达」时生效。所以**不需要**改 `models.json`。
  **部署当下上游拥塞**：拿 `qfmodel` 发了一条 8-token 试探请求，上游返
  `403 / CALL_COUNT_LIMIT`（`前方约 1 人`，真队列），插件按本地语义**同账号等 30s 重试**
  （预算 30m0s，会话亲和保持），客户端 150s 超时断开后**重试循环自己停了**（最后一
  次 try 6 与断开时刻对齐）。这修正了 2026-11-10 那条「客户端断开后 host **不会**取消
  插件侧请求，被弃的等待会跑到预算耗尽」——在 host **8.0.71** + `0.1.8` 上取消是会传播
  的。拥塞与选哪个 key 无关（2026-11-02 / 11-10 都记过同一现象）。
  **插件路径真正受用的只有两条**：
  1. `ListAvailableModels` 现在会在**本桥区域内**逐账号试错（上游以前只用池里第一个
     坑位，一个死凭证 `403 Signature invalid` 就把 `/v1/models` 拖到静态兜底表）。
     插件的 `model.for_auth`（`models.go`）和 `modelstate.go` 的目录刷新都走它。
  2. `preserveDeviceIdentity()`：上游的 bug 在**独立服务**路径——
     `buildAccountFromToken` 造全新 `Account` 后直接覆盖账号文件，抹掉
     `checkin_fingerprint`/`device_identity` ⇒ 退回旧协议（5 / 1.0.10）⇒
     Sonus/Cantus/Fast 静默消失。本地把构造拆到了 `BuildAccountFromToken`（插件共
     用），所以调用落在**唯一会覆盖账号文件的** `buildAccountFromToken` 上。插件本身走
     不到这里：凭证存在 host auth store，且 host 保存时会把旧文件里非凭证类的 key 合
     并回来（v8.0.43 源：`saveTokenRecord` → `mergeExistingAuthFileMetadata` →
     `MergeExistingAuthMetadata`，只跳过 `IsAuthTokenPayloadKey`），而线上 9 个账号根本
     没有抓包指纹（bridge 用 `settings.json` 的 `machine_salt` 稳定派生）。⇒
     **插件侧无需额外端口**；但若以后给某个 Intl 账号写入抓包指纹，这条要在真 8.0.7x
     host 上重验一次。
  3. `claude.go` `HandleListModels` 的兜底 key 清单改成 CN 快照（故意不含
     `ultimate`/`efficient`/`smodel`/`cmodel`/`fast`）：不在插件路径上（只有
     `service.go` 用它），插件自己的兜底表另见下条。
  **按上游快照刷新本地兜底表 `fallbackModels`**（行为变更，与 2026-11-02 那次同
  类）：`qfmodel` 0 → 0.1（限免结束 ⇒ **表里已无任何 0 价模型**，留着 0 就是面板/首查
  前清单宣称免费而实际记账）、`qmodel_latest` 实为 Qwen4-Max-Preview（0.1 / 推理 /
  32,768，旧行是 Qwen3.7-Max 0.5 非推理 16,384）、新增 `jmodel` Step-5-Preview
  （0.3 / 推理 / 32,768）、`mmodel` 输出 16,384 → 32,768 且改为推理。新增
  `TestFallbackCatalogHasNoFreeModel`（回退到 0 价即失败）与
  `TestFallbackCatalogMatches20261114Snapshot`，`TestFallbackDeepSeekNotFree` 去掉已过
  时的 `qfmodel=0` 断言。快照来自上游实拉，本仓库未独立重拉（首次真查询会用活清单覆
  盖兜底表），细节见 `docs/upstream-notes.md`。
  **部署前必读（本条已实测推翻，见本条开头的验证段）**：当时担心运行时
  `~/.qoder2api-cpa/models.json` 的 `disabled` 里**没有** `jmodel`，装上 0.1.8 后它会对
  客户端默认可见（上游当时报 `qmodel_latest`/`jmodel` 正拥塞，`10605 isQueued`，属
  容量不是故障）。实际装完 `/v1/models` 63 条里**没** `jmodel`：插件拉到的活清单本来
  就没下发它，新兜底表只在首查前/上游不可达时生效，所以 `models.json` 无需改。本轮**没有**新的「缺字段 = 默认开」配置项（上游只动
  `oauth.go`/`bridge.go`/`claude.go`/`service.go`/控制台，`settings.json` 字段零变化）。
  冲突处理：README 取本地精简版（上游 1036 行版丢弃）；`account/oauth.go` 保住
  `BuildAccountFromToken` 导出形态；`bridge.go` 两个结构体字段都留（上游
  `modelListURL` + 本地 `email`），本地 `Identity()`/`Email()`/`Region()`（含 nil 守
  卫、`RegionGlobal` 默认）盖掉上游裸访问器。§3 承重墙所在文件（`pool.go`/
  `queue.go`、`account/model.go`、`errors.go`、`cmd/cpa-plugin/` 除 `models.go`）本轮
  上游零改动。验证：`go build/vet ./...`、`go test ./...` 全绿（含上游新增
  `oauth_fingerprint_test.go`、`modellist_failover_test.go`），`-race` 见下；产物内
  `0.1.8` 命中 4 次、`0.1.7` 命中 0 次。

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