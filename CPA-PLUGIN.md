# Qoder CPA 插件 — 构建 / 来源 / 路线图 / 更新手册

本文档说明：

1. 这个项目由哪些部分组成、每个模块来自哪里；
2. 怎么构建、部署、回滚；
3. 哪些还没完成（技术债）；
4. **当上游 `qoder2api` 或官方 Qoder CLI 更新时，如何把变化同步进本项目。**

> 相关文档：插件自身的功能说明见 [`cmd/cpa-plugin/README.md`](cmd/cpa-plugin/README.md)；
> 官方 CLI 逆向分析见 `/home/brady/tool/qoder-cli-analysis/ANALYSIS.md`。

---

## 0. TL;DR

- 本项目 = **上游 `qoder2api` 的核心** + **我们自研的 CPA 插件层**。
- 上游核心（`account/`、`internal/cosy/`、`internal/bridge/`）负责"怎么和 Qoder 说话"；
  插件层（`cmd/cpa-plugin/`）负责"怎么接进 CLIProxyAPI"。
- 我们只对上游核心打了 **4 处小补丁**（见 §2.4）。更新上游时，**这 4 处是唯一需要盯的冲突点**。
- 官方 CLI 更新时，改动集中在 **端点表 / 请求头 / 签名 / 指纹** 四类，映射见 §6.3。

---

## 1. 系统组成与架构

```
                         ┌────────────────────────────────────────────┐
   客户端 (OpenAI/Claude/ │  CLIProxyAPI  v8.0.32  (宿主, :3002)         │
   Codex 协议)  ────────▶ │  · 路由 / 调度 / 会话亲和 / 用量统计          │
                         │  · OAuth 认证 UI / auth 文件管理              │
                         │  · 管理密钥鉴权                               │
                         └───────────────┬────────────────────────────┘
                                         │ C ABI (c-shared .so)
                                         ▼
                         ┌────────────────────────────────────────────┐
                         │  qoder 插件  cmd/cpa-plugin  (我们自研)      │
                         │  · AuthProvider    OAuth/PAT 登录、刷新      │
                         │  · ModelProvider   模型目录 + 模型开关        │
                         │  · Executor        请求 → 上游                │
                         │  · ManagementAPI   账号/签到/额度/面板        │
                         └───────────────┬────────────────────────────┘
                                         │ 进程内函数调用（Go package）
                                         ▼
                         ┌────────────────────────────────────────────┐
                         │  qoder2api 核心  (上游)                      │
                         │  · internal/bridge/   上游对话桥 / 队列       │
                         │  · internal/cosy/     COSY 签名 / 加密 / 指纹 │
                         │  · account/           账号模型 / OAuth / 端点 │
                         └───────────────┬────────────────────────────┘
                                         │ HTTPS (COSY 签名头)
                                         ▼
                         ┌────────────────────────────────────────────┐
                         │  Qoder 上游                                  │
                         │  api1/api2.qoder.sh · openapi.qoder.sh       │
                         │  center.qoder.sh · (CN) *.qoder.com.cn       │
                         └────────────────────────────────────────────┘
```

关键点：

- 插件是 **单进程内** 链接 qoder2api 核心（`import "qoder2api/..."`），不是独立服务。
- 账号凭证存进 **CPA 的 auth 文件**（`~/.cli-proxy-api/qoder-*.json`），
  不用 qoder2api 自带的文件存储。CPA 的调度/用量/别名因此自动生效。
- qoder2api 自带的独立服务（根目录 `main.go`/`service.go`/`webconsole/`）**本项目不使用**，
  但共享同一套 `internal/` 与 `account/` 代码。

---

## 2. 目录与模块来源（provenance）

### 2.1 完全自研（新增，上游没有）

| 路径 | 作用 |
|---|---|
| `cmd/cpa-plugin/` | 整个插件目录（详见 §2.5） |
| `internal/bridge/queue.go` (+`_test.go`) | 模型队列轮询 / `RESOURCE_LIMIT` 恢复 |
| `scripts/build-cpa-plugin.sh` | c-shared 构建脚本 |
| `dist/` | 构建产物（已 gitignore） |

### 2.2 改编自上游（上游有同名文件，但内容已改）

| 路径 | 来源 | 改动 |
|---|---|---|
| `cmd/cpa-plugin/checkin.go` | 根 `checkin.go` | 去掉账号文件存储依赖，改读 CPA auth store；补本地 streak 历史 |
| `cmd/cpa-plugin/baseprompt.json` | 根 `baseprompt.json` | **逐字节复制**（`cmp` 校验相同） |

> 注意：`baseprompt.json` 是**复制**而不是引用。上游改了根 `baseprompt.json` 时，
> 需要手动同步到 `cmd/cpa-plugin/baseprompt.json`（见 §6.1）。

### 2.3 上游原样复用（未改动）

| 路径 | 作用 |
|---|---|
| `account/model.go`, `paths.go`, `secret.go`, `store.go` | 账号模型 / 数据目录 / 密钥 / 文件存储 |
| `internal/cosy/auth.go`, `encoding.go`, `fingerprint.go`, `session.go`, `signature.go` | COSY 认证、编码、机器指纹、会话、签名 |
| `internal/bridge/` | 上游对话桥。其中 `bridge.go` / `client.go` 含 §2.4 补丁，其余文件（`bridge_pool.go`, `chat.go`, `claude.go`, `codex.go`, `cn_probe.go`, `delta.go`, `errors.go`, `messages.go`, `pool.go`）未改动 |
| `logger/logger.go` | 日志 |
| `auth.go`, `checkin.go`, `main.go`, `service.go`, `webconsole/` | qoder2api 独立服务（本项目**不编译**进插件） |

### 2.4 对上游核心打的补丁（**更新上游时必须保留 / 解决冲突**）

这 4 处是我们为了插件能力而对上游核心做的最小改动：

| 文件 | 补丁 | 为什么需要 |
|---|---|---|
| `account/oauth.go` | 新增 `PollLoginOnce()`、`BuildAccountFromToken()`；`buildAccountFromToken` 拆出落盘逻辑 | 插件由 CPA 宿主驱动轮询节奏，且凭证存 host 不落 qoder2api 文件 |
| `account/region.go` | `Endpoints` 新增 `InferenceBase` 字段（global/cn） | 队列轮询需要推理网关根地址 |
| `internal/bridge/bridge.go` | 新增 `Identity()`、`Region()`；改用 `streamWithQueueRecovery`；补 `x-request-set-id`/`x-session-id` 头 | 队列恢复 + 请求头对齐官方 |
| `internal/bridge/client.go` | 新增 `callGetCtx()`（`callGet` 委托给它） | 队列轮询需要可取消的 GET |
| `go.mod` | `go 1.22 → 1.26.0`，新增 `CLIProxyAPI/v8 v8.0.32` 依赖 | 插件 ABI 需要 |

**上游更新后的自检命令**（应全部有输出）：

```bash
grep -n "PollLoginOnce\|BuildAccountFromToken" account/oauth.go
grep -n "InferenceBase" account/region.go
grep -n "streamWithQueueRecovery\|func (b \*Bridge) Identity" internal/bridge/bridge.go
grep -n "callGetCtx" internal/bridge/client.go
```

### 2.5 插件内部模块（`cmd/cpa-plugin/`）

| 文件 | 职责 |
|---|---|
| `main.go` | C ABI 导出、RPC 分发、`plugin.register`、配置读取 |
| `auth.go` | 凭证解析、`auth.parse` / `auth.login` / `auth.refresh` |
| `authfile.go` | CPA auth 文件文档结构（顶层 `type`/`provider`/`logo`） |
| `models.go` | `model.static` / `model.for_auth`，模型目录 |
| `modelstate.go` | **模型开关**（`models.json` denylist） |
| `executor.go` | `execute` / `execute_stream` + host 流桥；内嵌 `baseprompt.json` |
| `checkin.go` | 签到核心 + 定时调度 |
| `management.go` | 管理路由 + host auth RPC |
| `panel.go` / `panel.html` | 内嵌管理面板 |
| `plugin_test.go` | 单测 |
| `README.md` | 插件功能与 API 说明 |

---

## 3. 构建

### 3.1 依赖

| 依赖 | 版本（当前） | 说明 |
|---|---|---|
| Go | 1.26.x（`go env GOVERSION`） | 需要 cgo |
| C 工具链 | gcc/clang | c-shared 必需 |
| `CLIProxyAPI/v8` | v8.0.32 | Go module 依赖，`go.mod` 里 |
| Node.js | ≥18（可选） | 仅在下载官方 CLI 时用 |

### 3.2 构建命令

```bash
cd /home/brady/tool/qoder2api

# 单测 / 静态检查
go build ./... && go vet ./... && go test ./...

# 构建插件（参数为版本号，写入 pluginVersion）
./scripts/build-cpa-plugin.sh 0.1.0
# 产物：dist/qoder-v0.1.0.so (+ .h)
```

构建脚本要点：

```
go build -buildmode=c-shared \
  -ldflags "-X qoder2api/cmd/cpa-plugin.pluginVersion=<version>" \
  -o dist/qoder-v<version>.so ./cmd/cpa-plugin/
```

> ⚠️ c-shared 注意事项：
> - 返回 `dlopen` 时**不能**在宿主线程上跑 goroutine；
> - `cliproxyPluginShutdown` 必须是 no-op。

### 3.3 部署与重启

```bash
APP=/home/brady/tool/cli-proxy-api-app
VER=0.1.0

# 1. 部署 .so
cp /home/brady/tool/qoder2api/dist/qoder-v${VER}.so \
   ${APP}/plugins/linux/amd64/

# 2. 重启 CPA
cd ${APP}
kill "$(cat app.pid)" 2>/dev/null; sleep 3
kill -9 "$(cat app.pid)" 2>/dev/null; sleep 1
: > app.log
nohup ./cli-proxy-api --config ./config.yaml > app.log 2>&1 &
echo $! > app.pid
sleep 7

# 3. 验证加载
grep "plugin_id=qoder" app.log
```

### 3.4 配置（`${APP}/config.yaml`）

```yaml
plugins:
    enabled: true
    dir: "plugins"
    configs:
        qoder:
            enabled: true
            # 可选：
            checkin_auto: false
            checkin_times: ["10:00"]
            checkin_on_start: false
            default_region: global
```

模型别名（避开与 workbuddy 的 `auto` 冲突）：

```yaml
oauth-model-alias:
    qoder:
        - { name: "auto",        alias: "qoder-auto" }
        - { name: "performance", alias: "qoder-performance" }
        - { name: "ultimate",    alias: "qoder-ultimate" }
        - { name: "efficient",   alias: "qoder-efficient" }
```

---

## 4. 数据与运行时布局

| 路径 | 内容 |
|---|---|
| `~/.cli-proxy-api/qoder-*.json` | 账号凭证（CPA auth 文件，权威存储） |
| `~/.qoder2api-cpa/models.json` | 模型开关 denylist（`{"disabled":[...]}`） |
| `~/.qoder2api-cpa/checkin_history.json` | 本地签到历史（streak 统计） |
| `~/.qoder2api-cpa/logs/` | 插件日志 |
| `${APP}/plugins/linux/amd64/qoder-v*.so` | 插件产物 |
| `${APP}/app.log` | CPA 主日志 |

数据根可用环境变量 `QODER2API_HOME` 覆盖（见 `cmd/cpa-plugin/main.go:initDataDir`）。

---

## 5. 未完成 / 技术债

> 结论先行：**当前功能完全可用**。以下都是"未雨绸缪"，不是"救火"。
> 旧端点 `/algo/...` 官方 CLI 自己还在用，短期不会下线。

| # | 事项 | 现状 | 影响 | 难度 |
|---|---|---|---|---|
| 1 | 新对话端点 `/model/v1/chat/completions` 鉴权 | 探测 401（COSY bearer）/ 403（原始 token），未解 | 官方若下线旧端点则被动 | 中（需抓包） |
| 2 | `umid` 真实机器身份 | 仍是 `md5("machine:"+uid+salt)` 伪造 | 风控差异，非功能问题 | 高（需逆向 Rust/ELF） |
| 3 | `Cosy-Organization-Id` / `AI-CLIENT-TIMESTAMP` 头 | 未发送 | 请求指纹与官方有差 | 低 |
| 4 | 队列最大等待 | 默认 10min（官方 1h） | 长队可能提前超时 | 低（可配置化） |
| 5 | `cmd/cpa-plugin/` 未纳入 git | 仍是 untracked | 误操作可能丢失 | 低 |
| 6 | 无 CI / 自动构建 | 手动构建 | 依赖人工 | 低 |
| 7 | 插件 logo | 指向 `webconsole/favicon.ico`，该文件不存在 → 404 | 面板图标缺失 | 低 |
| 8 | 版本号无关联 | 插件 0.1.0 与上游/官方版本无关 | 追溯困难 | 低 |

### 建议优先级

1. **#5 先做**：把插件提交到 git 分支，避免丢失（§6.1 的推荐布局）。
2. **#3 / #4 / #7**：低风险，随手可补。
3. **#1**：需要抓一次官方 CLI 的真实请求（`mitmproxy` 或分析 `ctx.py`），是唯一"需要专门投入"的项。
4. **#2**：投入产出比最低，可长期搁置。

---

## 6. 更新流程

### 6.1 上游 `qoder2api` 更新了

上游仓库：`https://github.com/Zhengyuuuui/qoder2api`

**推荐的 git 布局**（避免本地补丁和上游更新打架）：

```
main        ── 只跟踪上游（只允许 fast-forward）
cpa-plugin  ── main + 我们的 4 处核心补丁 + cmd/cpa-plugin/ + queue.go 等
```

首次整理（把当前未提交的改动固定到分支）：

```bash
cd /home/brady/tool/qoder2api

# 1. 备份当前可用产物
cp dist/qoder-v0.1.0.so /tmp/qoder-v0.1.0.so.bak 2>/dev/null

# 2. 把本地改动提交到独立分支
git checkout -b cpa-plugin
git add -A
git commit -m "feat: CPA plugin layer (cmd/cpa-plugin, queue recovery, core patches)"
```

之后的常规更新：

```bash
cd /home/brady/tool/qoder2api

# 1. 看上游有什么新东西
git fetch origin
git log --oneline HEAD..origin/main

# 2. 更新 main（快进）
git checkout main
git merge --ff-only origin/main

# 3. 把我们的分支 rebase 到新 main 上
git checkout cpa-plugin
git rebase main
#    → 冲突点只会在 §2.4 那 4 个文件里，按"保留我们的补丁 + 接受上游其它改动"解决

# 4. 自检 4 处补丁仍在
grep -n "PollLoginOnce\|BuildAccountFromToken" account/oauth.go
grep -n "InferenceBase" account/region.go
grep -n "streamWithQueueRecovery\|func (b \*Bridge) Identity" internal/bridge/bridge.go
grep -n "callGetCtx" internal/bridge/client.go

# 5. 同步 baseprompt.json（如果上游改过）
cmp baseprompt.json cmd/cpa-plugin/baseprompt.json \
  || cp baseprompt.json cmd/cpa-plugin/baseprompt.json

# 6. 验证 + 构建 + 部署
go build ./... && go vet ./... && go test ./...
./scripts/build-cpa-plugin.sh 0.1.1
cp dist/qoder-v0.1.1.so /home/brady/tool/cli-proxy-api-app/plugins/linux/amd64/
# 然后按 §3.3 重启
```

> 如果上游把 `internal/bridge/bridge.go` 大改，优先用 `git rebase` 而不是 merge，
> 这样每处补丁都能单独确认。

### 6.2 官方 Qoder CLI 更新了

官方 CLI 通过 npm SDK 分发，SDK 的 `postinstall` 再从 CDN 下载 CLI 二进制：

```
SDK:      @qoder-ai/qoder-agent-sdk          (npm)
CLI CDN:  https://download.qoder.com/qodercli/releases/<ver>/qodercli-<target>
CN CDN:   https://static.qoder.com.cn/qoder-cli-cn/releases/<ver>/...
```

**步骤：**

```bash
# 1. 看有没有新版
npm view @qoder-ai/qoder-agent-sdk version

# 2. 拉取 SDK 并查看它绑定哪个 CLI 版本
mkdir -p /tmp/qoder-new && cd /tmp/qoder-new
npm pack @qoder-ai/qoder-agent-sdk@latest
tar xzf qoder-ai-qoder-agent-sdk-*.tgz
cat package/dist/runtime-manifest.json     # 里面的 qoderCliVersion
cat package/scripts/postinstall.cjs        # CDN URL 模板

# 3. 下载对应 CLI 二进制（按 runtime-manifest 里的版本/平台）
#    例如 linux-x64：
#    https://download.qoder.com/qodercli/releases/<ver>/qodercli-linux-x64
#    然后 sha256 校验

# 4. 存进分析目录
mv qodercli-linux-x64 /home/brady/tool/qoder-cli-analysis/qodercli-<ver>
cd /home/brady/tool/qoder-cli-analysis
strings -a qodercli-<ver> > qodercli-<ver>.strings

# 5. 定位关键字符串（ctx.py 是现成的字节上下文提取器）
python3 ctx.py '/model/v1'
python3 ctx.py '/algo/api/v2/service/pro/sse'
python3 ctx.py 'Cosy-'
python3 ctx.py 'qoder_auth_wasm'

# 6. 解包内嵌 WASM / umid（方法见 ANALYSIS.md）
#    - qoder_auth_wasm_bg.wasm  → 签名/加密逻辑
#    - umid-*.elf               → 机器指纹采集

# 7. 对比新旧差异，按 §6.3 映射表改动
diff <(strings -a qodercli | sort -u) <(strings -a qodercli-<ver> | sort -u) | less
```

**重点看这 5 类变化**（对应 §6.3）：

1. 端点 URL / 路径 / 域名 → `account/region.go`
2. 请求头集合 → `internal/bridge/client.go` 的 `buildHeaders`
3. 签名 / COSY 算法 → `internal/cosy/signature.go`、`session.go`
4. 机器指纹算法 → `internal/cosy/fingerprint.go`
5. 新协议（如队列、新对话端点）→ 新建/修改 `internal/bridge/*.go`

### 6.3 变化 → 改哪个文件（映射表）

| 上游/官方变化 | 需要改的文件 |
|---|---|
| API 域名 / 路径 / 新增端点 | `account/region.go`（`Endpoints` 结构 + global/cn 两张表） |
| 请求头集合 | `internal/bridge/client.go` → `buildHeaders`；`cmd/cpa-plugin/checkin.go` → `checkinHeaders` |
| COSY 签名 / 加密 | `internal/cosy/signature.go`、`internal/cosy/session.go` |
| 机器指纹 / 机器码 | `internal/cosy/fingerprint.go` |
| 模型列表字段 / 排序 | `internal/bridge/bridge.go` → `ListAvailableModels` |
| 对话请求体格式 | `internal/bridge/chat.go`、`baseprompt.json`（两处都要同步） |
| 队列协议 | `internal/bridge/queue.go` |
| 签到流程 / campaign 字段 | `cmd/cpa-plugin/checkin.go` |
| OAuth 登录 / 刷新 | `account/oauth.go` + `cmd/cpa-plugin/auth.go` |
| 额度 / 计划接口 | `account/region.go` + `cmd/cpa-plugin/management.go` |
| 插件 ABI（CPA 升级） | `cmd/cpa-plugin/main.go`（C ABI）+ `go.mod` 的 `CLIProxyAPI/v8` 版本 |

---

## 7. 回归测试清单

每次更新（上游或官方）后，按此清单验证：

```bash
APP=/home/brady/tool/cli-proxy-api-app
K="<你的 CPA API key>"          # config.yaml 里 access.api-keys 的 sk-...

# 1. 插件加载
grep "plugin_id=qoder" ${APP}/app.log

# 2. 模型列表（应含 qoder-* 与 Qoder 原生模型）
curl -s -H "Authorization: Bearer $K" http://127.0.0.1:3002/v1/models \
  | python3 -c "import json,sys;print([m['id'] for m in json.load(sys.stdin)['data'] if m.get('owned_by')=='qoder'])"

# 3. 非流式对话
curl -s http://127.0.0.1:3002/v1/chat/completions \
  -H "Authorization: Bearer $K" -H "Content-Type: application/json" \
  -d '{"model":"qoder-efficient","messages":[{"role":"user","content":"reply exactly: PONG"}],"max_tokens":20}'

# 4. 流式对话（应看到 SSE 增量 + [DONE]）
curl -sN http://127.0.0.1:3002/v1/chat/completions \
  -H "Authorization: Bearer $K" -H "Content-Type: application/json" \
  -d '{"model":"qoder-efficient","stream":true,"messages":[{"role":"user","content":"count 1 to 3"}]}'

# 5. 管理路由存在（无密钥应 401）
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:3002/v0/management/plugins/qoder/accounts

# 6. 面板可访问
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:3002/v0/resource/plugins/qoder/panel
```

签到（需要真实账号）可用临时测试跑通（用完删除）：

```go
// cmd/cpa-plugin/zz_live_checkin_test.go
// 详见 git 历史 / 本文档 §6 讨论；核心是 parseStored + checkinStored
LIVE_CHECKIN=1 go test -run TestLiveCheckin -v ./cmd/cpa-plugin/
```

**单测**：`go test ./...`（`internal/bridge`、`internal/cosy`、`cmd/cpa-plugin` 均有覆盖）。

---

## 8. 回滚

```bash
APP=/home/brady/tool/cli-proxy-api-app
# 保留旧 .so，出问题时切回
cp ${APP}/plugins/linux/amd64/qoder-v0.1.0.so ${APP}/plugins/linux/amd64/qoder-v0.1.1.so
# 重启（§3.3）
```

建议每次部署前：

```bash
cp ${APP}/plugins/linux/amd64/qoder-v<old>.so /tmp/qoder-v<old>.so.bak
```

配置回滚：`config.yaml` 的历史备份就在 `${APP}/` 下（`config.yaml.bak-*`）。

---

## 9. 排障

| 现象 | 排查 |
|---|---|
| 插件未加载 | `grep qoder app.log`；确认 `.so` 在 `plugins/linux/amd64/` 且 `configs.qoder.enabled: true` |
| 模型列表为空 | 账号是否有效；`qoder-efficient` 等别名是否配置 |
| 对话 403 `RESOURCE_LIMIT` | 队列恢复会等待；若超 10min 则失败（见 §5 #4） |
| 对话 401 | 凭证过期，重新 OAuth 登录或刷新 |
| 签到 `no_campaign` | 正常：今日已领 / 无活动窗口（非故障） |
| 管理 API 401 | 缺 `Authorization: Bearer <management-key>` |
| 面板打不开 | 资源路由是 `/v0/resource/plugins/qoder/panel` |

日志位置：`${APP}/app.log`（宿主）、`~/.qoder2api-cpa/logs/`（插件）。

---

## 附：关键版本快照

| 组件 | 版本 |
|---|---|
| CLIProxyAPI 宿主 | v8.0.32（commit `911f49cb`） |
| `CLIProxyAPI/v8` Go module | v8.0.32 |
| 上游 qoder2api | `main` @ `4789816`（2026-10-16） |
| 官方 Qoder CLI | v1.1.73（SDK `@qoder-ai/qoder-agent-sdk` 1.0.53） |
| qoder 插件 | 0.1.0 |
| Go | 1.26.x |
