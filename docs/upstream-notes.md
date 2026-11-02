# 上游 qoder2api 同步备忘

本文件保留上游 [Zhengyuuuui/qoder2api](https://github.com/Zhengyuuuui/qoder2api)
中与插件用户相关、但未放进精简版 README 的文档（2026-10-20 快照）。

## 国际版签到的账号差异（灰度投放）

国际版（`qoder.com`）的活动是**按账号灰度投放**的，不同账号拿到的活动**不重叠**。
实测两个账号：

| | 老号 | 新号 |
|---|---|---|
| 注册时间 / 方式 | 2026-07-29 · `sso.google` | 2026-10-11 · `dashboard.email_pwd` |
| **每日签到**（每天 100 Credits） | ✅ 可正常自动签到 | ❌ 服务端未下发 |
| **SOTA 专属积分**（2000，限 Sonus 模型） | ❌ 未下发 | ✅ 已领取 |

两点结论：

1. **老号签到正常，但没有 SOTA 专属积分** —— 自动签到只覆盖「每日签到」这一类活动
2. **新号无签到，但有 SOTA 积分** —— 用新号的 SOTA 包 ID 给老号代领会返回
   `CAMPAIGN_NOT_FOUND`（服务端校验活动归属），说明这是运营灰度，不是接口或指纹问题

> 即：**自动签到能领的活动范围，取决于服务端给该账号下发了什么**。
> 插件只负责把已下发的 `CLAIM_BENEFIT` 活动自动领掉，无法代领未下发的活动。

SOTA 专属积分等「专属资源包」会单独出现在额度查询结果里（`dedicated_packages`），
不并入套餐 / 加油包 / 组织额度。

## 模型总表（2026-10-20 实拉）

模型清单由上游 `GET /algo/api/v2/model/list` 在运行时动态拉取，下方为当时快照，仅供对照；
实际以 `/v1/models` 返回为准。`price_factor` 为相对倍率，`0` 表示限免。

> ⚠️ 同一 key 在**两区的推理支持与最大输出可能不同**（上游按区配置），故分表列出。

### 国内版 · 14 个

| Key | 显示名 | 上下文 | 最大输出 | 倍率 | 推理 |
|-----|--------|--------|----------|------|:----:|
| `auto` | Auto | 200,000 | 32,768 | 0.5 | ✅ |
| `qmodel_38max` | Qwen3.8-Max | 200,000 | 32,768 | **0.2** | ✅ |
| `qfmodel` | Qwen3.8-Flash | 200,000 | 32,768 | **0** 🆓 | ✅ |
| `qmodel_latest` | Qwen3.7-Max | 200,000 | 32,768 | 0.5 | ✅ |
| `qmodel` | Qwen3.7-Plus | 200,000 | 32,768 | 0.1 | ✅ |
| `q37fmodel` | Qwen3.7-Flash | 200,000 | 32,768 | 0.1 | ✅ |
| `dmodel` | DeepSeek-V4-Pro | 200,000 | 32,768 | **0.5** | ✅ |
| `dfmodel` | DeepSeek-Flash | 200,000 | 16,384 | **0** 🆓 | — |
| `gmodel` | GLM-5.3 | 200,000 | 32,768 | **0.8** | ✅ |
| `gfmodel` | GLM-5.3-Flash | 200,000 | 32,768 | 0.1 | ✅ |
| `gm51model` | GLM-5.2 | 200,000 | 32,768 | 0.6 | ✅ |
| `kmodel_latest` | Kimi-K3 | 200,000 | 16,384 | **1.4** | — |
| `kmodel` | Kimi-K2.8-Preview | 200,000 | 32,768 | **0.8** | ✅ |
| `mmodel` | **MiniMax-M3.1-Flash** | 200,000 | 16,384 | 0.1 | — |

### 国际版 · 15 个

| Key | 显示名 | 上下文 | 最大输出 | 倍率 | 推理 |
|-----|--------|--------|----------|------|:----:|
| `auto` | Auto | 200,000 | 16,384 | 0.5 | — |
| `ultimate` | Ultimate | 200,000 | 32,768 | 0.4 | ✅ |
| `performance` | Performance | **272,000** | 16,384 | 1.1 | — |
| `efficient` | Efficient | 200,000 | 16,384 | 0.3 | — |
| `qmodel_38max` | Qwen3.8-Max | 200,000 | 32,768 | **0.2** | ✅ |
| `qfmodel` | Qwen3.8-Flash | 200,000 | 32,768 | **0** 🆓 | ✅ |
| `qmodel_latest` | Qwen3.7-Max | 200,000 | 16,384 | 0.5 | — |
| `qmodel` | Qwen3.7-Plus | 200,000 | 16,384 | 0.1 | — |
| `dmodel` | DeepSeek-V4-Pro | 200,000 | 32,768 | **0.5** | ✅ |
| `dfmodel` | DeepSeek-Flash | 200,000 | 32,768 | **0** 🆓 | ✅ |
| `gmodel` | GLM-5.3 | 200,000 | 32,768 | **0.8** | ✅ |
| `gfmodel` | GLM-5.3-Flash | 200,000 | 32,768 | 0.1 | ✅ |
| `kmodel_latest` | Kimi-K3 | 200,000 | 16,384 | **1.4** | — |
| `kmodel` | Kimi-K2.8-Preview | 200,000 | 16,384 | **0.8** | — |
| `mmodel` | **MiniMax-M3.1-Flash** | 200,000 | 16,384 | 0.1 | — |

**区域差异速览**

| 差异 | 详情 |
|------|------|
| 国际独占 | `ultimate` / `performance`（272K 上下文，全库最大）/ `efficient` |
| 国内独占 | `q37fmodel`（Qwen3.7-Flash）/ `gm51model`（GLM-5.2） |
| 推理标记不同 | `auto` / `kmodel` / `qmodel` / `qmodel_latest` 国内支持、国际不支持；`dfmodel` 反之 |
| 最大输出不同 | 多数模型国内 32,768、国际 16,384；`dfmodel` 相反 |

### 本次模型清单变更

| Key | 显示名 | 变化 |
|-----|--------|------|
| `mmodel` | **MiniMax-M3.1-Flash** | 🔄 更名（原 MiniMax-M2.7） |
| `qmodel_38max` | Qwen3.8-Max | 💰 price 0.5 → **0.2**（降价） |
| `dmodel` | DeepSeek-V4-Pro | 💰 price 0.8 → **0.5**（降价） |
| `gmodel` | GLM-5.3 | 💰 price 0.6 → **0.8**（涨价） |
| `kmodel` | Kimi-K2.8-Preview | 💰 price 0.3 → **0.8**（涨价） |
| `kmodel_latest` | Kimi-K3 | 💰 price 0.8 → **1.4**（涨价） |
| `dfmodel` | DeepSeek-Flash | 🆓 price=**0**（免费） |
| `qfmodel` | Qwen3.8-Flash | 🆓 price=**0**（限免活动延续） |

> ⚠️ `mmodel` 只是**显示名**变化，key 未变，已有调用与映射无需调整。
> 若在控制台手工配过模型映射且使用了旧显示名，请改为 key。

---

## 国际版 · 2026-11-02 实测快照（DeepSeek 免费档消失）

用部署中的 9 个 Global 账号逐个调用 `/algo/api/v2/model/list`（legacy 协议，
无设备指纹）拉取实时清单，9 个账号完全一致：12 个模型，**只有一个 0 价模型**。

| Key | 显示名 | price_factor | max_input | max_output | 推理 |
|-----|--------|:------------:|:---------:|:----------:|:----:|
| `auto` | Auto | 0.5 | 200,000 | 16,384 | — |
| `ultimate` | Ultimate | **2.0** ↑ | 1,000,000 | 32,768 | ✅ |
| `efficient` | Efficient | 0.3 | 200,000 | 16,384 | — |
| `qmodel_38max` | Qwen3.8-Max | **0.5** ↑ | 180,000 | 32,768 | ✅ |
| `qfmodel` | Qwen3.8-Flash | **0** 🆓 | 180,000 | 32,768 | ✅ |
| `dmodel` | DeepSeek-V4-Pro | 0.5 | 1,000,000 | 32,768 | ✅ |
| `dfmodel` | DeepSeek-Flash | **0.1** 💰 | 1,000,000 | 32,768 | ✅ |
| `gmodel` | GLM-5.3 | 0.8 | 180,000 | 32,768 | ✅ |
| `gfmodel` | GLM-5.3-Flash | 0.1 | 1,000,000 | 32,768 | ✅ |
| `kmodel` | Kimi-K2.8-Preview | 0.8 | — | 16,384 | — |
| `kmodel_latest` | **Kimi-K3.1-Pro** | 1.0 | 180,000 | 16,384 | — |
| `mmodel` | MiniMax-M3.1-Flash | 0.1 | 180,000 | 16,384 | — |

**与 2026-10 快照的差异**

| Key | 变化 |
|-----|------|
| `dfmodel` | 💰 **price 0 → 0.1**：DeepSeek-Flash 不再是免费模型，国际版唯一 0 价模型只剩 `qfmodel` |
| `ultimate` | 💰 price 0.4 → **2.0** |
| `qmodel_38max` | 💰 price 0.2 → **0.5** |
| `kmodel_latest` | 显示名 Kimi-K3 → Kimi-K3.1-Pro，price 1.4 → **1.0**（上游 2026-10-26 已记录，本地兜底表此前未同步） |
| `performance` / `qmodel` / `qmodel_latest` | 🚫 国际版不再下发（国内清单仍可能有） |
| `dfmodel` / `dmodel` / `gfmodel` / `ultimate` | 上下文档位到 1,000,000（context tier 选择已支持） |

**账号侧配额**：9 个账号 `plan=Free`，额度都在 `addon` 桶里，实测剩余
12–289 积分/账号（合计约 967）。所以 DeepSeek 转为计价之后，Qoder 这条线
不再是"零成本"，消耗会直接吃这些 addon 积分。

**实测拥塞**：2026-11-02 用两个账号（余 100 / 余 289）各发一条最小请求，
`dfmodel`、`dmodel`、`qfmodel`、`gmodel` **全部**返回
`statusCodeValue=403 / subCode=CALL_COUNT_LIMIT`（`isQueued:true`、
`serviceAvailable:false`），即此刻整个 Qoder 上游对这些 Free 账号拥塞，
与选哪个 DeepSeek key 无关。切到付费 id 不会换来可用性。
