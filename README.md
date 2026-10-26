# orca-ai-pool

AI 号池管理仓库：**nginx 限速入口 + CPA (CLIProxyAPI) 主网关 + opencode2api 副网关**，Docker Compose 部署在新加坡服务器。
对外只暴露一个 OpenAI 兼容端点 `http://<server>:8317/v1`，由 nginx 入口转发给 CPA（模型调用**不限速**，
只有 CPAMC 管理面按 IP 限速以挡口令爆破），内部再按模型名路由到
Devin (OAuth)、OpenCode Go、Command Code GOAT 三条上游，用「加权轮询 + 会话亲和」做负载均衡与缓存保护。
仓库还自带一个 CPA C ABI 插件（`plugin/opencode-go-quota`），把 OpenCode Go 各 key 的真实余量接进 CPAMC 配额页。

---

## 1. 架构

```
                          ┌────────────────────────────────────────────────────┐
   本地 Orca / 客户端     │  Authorization: Bearer sk-orca-master-key          │
        (OpenAI API)      └───────────────────────┬────────────────────────────┘
                                                  │  http://<server>:8317/v1
                                                  ▼
                                    ┌──────────────────────────────┐
                                    │  nginx ingress（唯一公网口）  │  /v1        不限速（透传）
                                    │  :8317 SSE 透传 / 管理面限速  │  /v0/管理   10 r/min + burst 5
                                    └───────────────┬──────────────┘  /management 共用同桶 burst 10
                                                    │ orca-net（内网）
                                                    ▼
                                 ┌───────────────────────┐
                                 │  cliproxyapi (CPA)    │  加权轮询 + 会话亲和
                                 │  :8317 (仅内网可达)   │  usage 统计 / 冷却 / 重试
                                 └───┬───────┬───────┬───┘
                                     │       │       │
              ┌──────────────────────┘       │       └──────────────────────┐
              │ oauth provider               │ openai-compatibility         │ openai-compatibility
              ▼                              ▼                              ▼
    ┌───────────────────┐        ┌──────────────────────┐        ┌────────────────────────┐
    │  Devin (OAuth)    │        │ opencode2api :8080   │        │ command-code-goat      │
    │  auths/*.json     │        │ (仅 orca-net 内网)   │        │ api.commandcode.ai     │
    │  swe-2 等免费模型 │        │ WebUI 127.0.0.1:18081│        │ provider/v1 (API key)  │
    └───────────────────┘        └──────────┬───────────┘        └────────────────────────┘
                                            │ go_keys + proxies
                                            ▼
                                 ┌──────────────────────┐
                                 │  OpenCode Go 订阅池  │
                                 │  opencode.ai/zen/go  │
                                 └──────────────────────┘
```

三条上游的定位：

| 上游 | 协议 | 凭据 | 作用 |
|---|---|---|---|
| Devin | OAuth session token | `cliproxy/auths/<id>.json` | 免费/Pro 模型主力（`swe-2` 系列、`gemini-4-argon`） |
| opencode2api → OpenCode Go | OpenAI 兼容 | `go_keys` + 代理 | DS4.1F 容量池（Argon 是 Google 原生协议，无法桥接，不进此池） |
| Command Code GOAT | OpenAI 兼容 | API key | 直连兜底，成本最低的 DS4.1F |

> `gemini-4-argon` 只能走 Devin OAuth：它上游是 Google 原生 `/inference/go/google/v1beta`，
> opencode2api 只做 chat / responses / anthropic 三种协议转换，桥接不了（提交 `5723659` 把它移出了 Go 池）。

---

## 2. 目录结构

```
orca-ai-pool/
├── docker-compose.yml                    # 两服务 + orca-net + opencode2api-state 卷
├── docker-compose.override.yml           # 叠加 nginx 入口：8317 归 ingress，CPA 退到 127.0.0.1:18317
├── nginx/
│   └── nginx.conf                        # 公网入口：/v1 透传不限速，仅管理面限速 + 429 JSON
├── plugin/
│   └── opencode-go-quota/                # 自研 CPA C ABI 插件（Go）：Go 池余量 → CPAMC 配额页
│       ├── main.go
│       ├── Makefile                      # make install → cliproxy/plugins/linux/amd64/
│       └── go.mod / go.sum
├── .env.example                          # 部署参数（复制为 .env）
├── .gitignore
├── accounts.example.yaml                 # 号池清单（复制为 accounts.yaml）
├── README.md
├── scripts/
│   └── poolctl.py                        # 唯一编排入口（5 个子命令）：render / check-opencode / quota / switch-mode / export-client
├── templates/
│   ├── cliproxy.config.v8.yaml.tmpl      # CPA v8 配置结构参考（手工兜底）
│   ├── opencode2api.config.json.tmpl     # opencode2api 配置结构参考（JSONC）
│   └── local-devin-config.json           # 本地 Devin 客户端配置（防免费池限流三件套）
├── cliproxy/
│   ├── config.yaml                       # ← render 产物（gitignore）
│   ├── auths/                            # ← render 产物：<id>.json 凭据（gitignore *.json）
│   ├── plugins/linux/amd64/              # ← make install 产物：opencode-go-quota.so（gitignore）
│   └── logs/                             # 运行日志（gitignore）
├── opencode2api/
│   └── config.json                       # ← render 产物（gitignore，作为 state 卷种子）
└── export/                               # export-client 产物（gitignore）
    └── env.sh
```

> `.gitignore` 只忽略 render **产物**（`config.yaml` / `config.json` / `auths/*.json`）。
> 结构参考全部放在 `templates/`；若你手工另存为 `cliproxy/config.yaml.example` /
> `opencode2api/config.json.example` 这类 `*.example` 文件，它们不会被忽略，可以放心入库。

---

## 3. 新加坡服务器首次部署 Checklist

按顺序执行，每一步都给出验证方式。

### 3.1 装 Docker

```bash
curl -fsSL https://get.docker.com | sh
sudo usermod -aG docker $USER   # 重新登录生效
docker compose version          # 需要 v2
```

### 3.2 clone 仓库

```bash
git clone <your-repo> orca-ai-pool
cd orca-ai-pool
```

### 3.3 填 `.env`

```bash
cp .env.example .env
```

| 变量 | 说明 | 生成方式 |
|---|---|---|
| `GATEWAY_HOST` | 服务器公网 IP | 控制台查看 |
| `GATEWAY_PORT` | CPA 端口，默认 8317 | — |
| `MASTER_API_KEY` | 客户端访问 CPA 的主 Key | `openssl rand -hex 32` → 加 `sk-orca-` 前缀 |
| `CPA_ADMIN_SECRET` | 管理接口 / CPAMC 面板密码 | `openssl rand -hex 24` |
| `OC2API_SERVER_KEY` | CPA → opencode2api 内网 key | `openssl rand -hex 32` |
| `OC2API_WEBUI_USER` | WebUI 用户名 | 自定 |
| `OC2API_WEBUI_PASSWORD` | WebUI 密码，**≥10 字符** | `openssl rand -base64 18` |
| `DISABLE_AUTH` | 设为 `true` 时 `access.api-keys` 输出空列表 = 客户端免鉴权。**只用于内网/测试**；生产保持 `false`，否则 8317 就是一个不设防的公网 LLM 出口 | 默认 `false` |

```bash
chmod 600 .env
```

> `DISABLE_AUTH` 不影响管理接口：`/v0/management` 仍要 `CPA_ADMIN_SECRET`（`secret-key` 置空会把管理端整段关掉，脚本不会这么做）。

### 3.4 填号池清单

```bash
cp accounts.example.yaml accounts.yaml
chmod 600 accounts.yaml
$EDITOR accounts.yaml     # 按第 4 节录入 session_token / go keys / GOAT keys
```

### 3.5 render

```bash
python3 scripts/poolctl.py render
```

产物：

- `cliproxy/config.yaml` —— CPA v8 配置（占位符已替换，含 `plugins.configs` 段）
- `cliproxy/auths/<id>.json` —— 每个 Devin 账号一个
- `cliproxy/auths/opencode-go-pool.json` —— 给 `opencode-go-quota` 插件的池凭据（`type: "opencode-go"`）
- `opencode2api/config.json` —— opencode2api 种子配置

验证：

```bash
grep -c "worker-ds41" cliproxy/config.yaml            # 应为 0：模型走真实 ID，旧的聚合别名已废弃
grep -A4 "opencode-go-quota:" cliproxy/config.yaml     # 能看到 opencode_api_keys 列表 = 插件配置已生成
python3 -c "import json;json.load(open('opencode2api/config.json'))"   # 纯 JSON 可解析
ls cliproxy/plugins/linux/amd64/                       # 装了插件才有 opencode-go-quota.so
```

### 3.6 预检 OpenCode Go key

```bash
python3 scripts/poolctl.py check-opencode
```

预期输出每个 key 的 `allowance`（200）。逐条对照：

- **403** → 该 Go 账号没开 Privacy Global（见 4.2），去网页改设置后重跑。
- **429** → 窗口冷却中（5h 或周额度耗尽），等窗口刷新或换号。

### 3.7 起服务

```bash
docker compose up -d
docker compose ps          # 两个服务都应为 healthy
```

首次启动日志检查：

```bash
docker compose logs -f opencode2api   # 应看到从 CONFIG_SEED_PATH 导入配置
docker compose logs -f cliproxyapi    # 应看到加载 auths/*.json 与 opencode-go-pool provider
```

### 3.8 端到端验证

```bash
export CPA_KEY=$(grep '^MASTER_API_KEY=' .env | cut -d= -f2)
export HOST=$(grep '^GATEWAY_HOST=' .env | cut -d= -f2)

# 模型清单（无需鉴权也能列，但带上 key 更贴近真实客户端）
curl -s http://$HOST:8317/v1/models -H "Authorization: Bearer $CPA_KEY" | python3 -m json.tool | head -40

# 冒烟：免费模型（Devin）
curl -s http://$HOST:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"devin/swe-2","messages":[{"role":"user","content":"ping"}],"max_tokens":8}'

# 冒烟：付费模型（OpenCode Go 链路）
curl -s http://$HOST:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"ping"}],"max_tokens":8}'

# 可选：同一个模型名打 20 次，观察 opencode-go-pool 与 command-code-goat 之间的轮询分布
for i in $(seq 1 20); do
  curl -s -o /dev/null -w '%{http_code} ' http://$HOST:8317/v1/chat/completions \
    -H "Authorization: Bearer $CPA_KEY" -H 'Content-Type: application/json' \
    -d '{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"ping"}],"max_tokens":4}'
done; echo
```

> `deepseek-v4.1-flash` 在 CPA 里由 **opencode-go-pool 与 command-code-goat 两个 provider 同名注册**，
> 所以没有独立的"GOAT 冒烟"——两条上游由 `weighted-round-robin` 分流，任一成功即说明聚合生效。
> 两段都返回 `choices` 即部署完成（第一条验证 Devin OAuth 链路，第二条验证 OpenAI 兼容聚合链路）。

### 3.9 防火墙

只放行 `8317`（nginx 入口）。**8080 / 18081 / 18317 都不要开**：

```bash
# ufw 示例
sudo ufw allow 22/tcp
sudo ufw allow 8317/tcp
sudo ufw enable
```

- `8080` 是 opencode2api 的 API，没有发布到宿主机（只在 `orca-net` 内被 CPA 访问）。
- `18081` 只绑 `127.0.0.1`，用 SSH 隧道：`ssh -L 18081:127.0.0.1:18081 user@server` → 本地开 `http://127.0.0.1:18081`。
- `18317` 是 CPA 的宿主机回环端口（见 3.10），用于绕过入口做排障（比如对比管理面 429），不要对公网开放。

### 3.10 公网入口限速（nginx ingress）

`docker-compose.override.yml` 会**自动叠加**到基础 compose 上（不需要 `-f`），拓扑变成：

```
公网 → nginx :8317（限速）→ cliproxyapi:8317（内网）
宿主机 127.0.0.1:18317 → cliproxyapi:8317（绕过限速，仅排障）
```

CPAMC 管理口**按设计对公网开放**（`management.allow-remote: true`）。入口的设计原则是：

> **只防管理面的口令爆破，不碰实际模型调用。**

实际模型调用的速率由上游账号额度 + CPA 的会话亲和 / 冷却负责，入口不参与——多 agent 并行、
长上下文连续调用都不会被入口掐掉。

| 路径 | 限速 | 突发 | 并发上限 | 说明 |
|---|---|---|---|---|
| `/v1/*` | **不限速** | — | **不限** | 客户端 LLM 接口，纯透传；SSE 不开缓冲、读超时 3600s |
| `/v0/management/*` | 10 r/min | 5 | 32/IP | 管理 API（`poolctl quota` 也走这里） |
| `/management*` | 10 r/min | 10 | 32/IP | CPAMC 登录页与静态资源，**与上一行共用同一个桶** |
| 其他 | 10 r/min | 5 | 32/IP | 管理入口兜底 |

共用桶是有意为之：若按路径分别计数，攻击者先打 `/management` 再打 `/v0/management` 就能各拿一份 burst。
现在所有管理入口共享 `orca_admin` 一个 zone，整体速率恒定在 ~0.17 次/秒/IP，口令爆破在速率上就不可行。

超限返回 `429` + `Retry-After: 1` + JSON body（仅管理面会出现）：

```json
{"error":{"message":"management endpoint rate limited by orca-ai-pool ingress","type":"rate_limit_exceeded","code":"rate_limit_exceeded"}}
```

> `poolctl quota` 每次跑会连发 4~6 条管理请求，在 10 r/min + burst 5 下够用；
> 若你加号后一次要刷十几个凭据，调大 `burst` 或改成 `rate=30r/m`。

调参：改 `nginx/nginx.conf` 顶部的 `rate=` / `burst=`，然后 `docker compose restart ingress`。
**回滚到"CPA 直接对外"**：`mv docker-compose.override.yml{,.disabled} && docker compose up -d`。

> 入口日志只记方法/URI/状态/耗时（`log_format orca`），**不记录 `Authorization` 头**，避免 key 进日志。
> 想再收紧管理口，可以打开 `location /v0/` 里注释掉的 `auth_basic` 段，加一层独立口令。

---

## 4. 账号录入指南

### 4.1 Devin session_token

1. 浏览器登录 `https://app.devin.ai`，确认该账号的订阅计划（Pro / Max）。
2. F12 → **Application / Storage → Cookies**（或 Network 里任一请求的 `Cookie` 头）。
3. 找名字为 `devin-session-token` 的 cookie，值形如 `devin-session-token$eyJhb...`。
4. 整串（**含 `devin-session-token$` 前缀**）粘进 `accounts.yaml`：

```yaml
- id: "devin-pro-1"                 # 任意唯一标识，将成为 cliproxy/auths/devin-pro-1.json
  email: "pro1@example.com"
  plan: "Pro"                       # Pro | Max，必须与实际订阅一致
  weight: 1                         # pro-only 模式全为 1；Max 用 7
  session_token: "devin-session-token$eyJhb..."
```

5. `poolctl render` 后 CPA 自动热载新凭据，无需重启容器。

> token 会过期（重新登录即失效）。出现 401 就重抓，改 `accounts.yaml`，重跑 render。

### 4.2 OpenCode Go 订阅与 **Privacy Global（必开）**

1. 订阅 OpenCode Go（月付 $10 起，见第 6 节配额）。
2. **进 `Settings › Privacy › Regions`，把区域切成 `Global`。**
   这是硬性要求：留在默认区域时，`deepseek-v4.1-flash` 会稳定返回 **403**（区域不支持）。
3. 拿 API key：账号设置里创建，形如 `oc_go_key_...`，填进 `opencode_go_pool.keys`。
4. 多号注意事项（**链式自邀是被风控重点盯的行为**）：
   - 不要用同一张卡 / 同一设备指纹批量开号；各号独立支付方式更稳。
   - 不要用 A 号的邀请链接开 B 号后又让 B 号去开 C 号（链式自邀），一旦关联会连坐封号。
   - 号与出口 IP 绑定：单号单出口，代理必须**静态**（见 8.2）。
5. 录完跑 `poolctl check-opencode`，全部 200 再投入生产。

### 4.3 Command Code GOAT API key

1. 登录 Command Code，进 **Studio › API keys** 创建 key（`cc_goat_...`）。
2. 填入 `command_code_goat_pool.keys`，`weight` 按需（额度充裕的号给更高权重）。

```yaml
command_code_goat_pool:
  keys:
    - name: "goat-1"
      api_key: "cc_goat_key_01..."
      weight: 1
```

3. Command Code **支持支付宝（Alipay）付款**，国内充值无需外币卡。

---

## 5. 模型路由表

对外只保留 4 个模型，客户端直接用真实 ID（不做别名映射，`GET /v1/models` 即路由表）：

| 客户端模型名 | 上游 | 典型用途 |
|---|---|---|
| `devin/swe-2` | Devin OAuth | 免费主力，通用编码/推理，量大管饱 |
| `devin/swe-2-lightning` | Devin OAuth | 低延迟版本，交互式补全、小任务 |
| `devin/gemini-4-argon` | Devin OAuth | 长上下文分析、架构评审 |
| `deepseek-v4.1-flash` | OpenCode Go / GOAT | 批量代码生成、脏活累活，性价比最高 |

> Devin 凭据的上游目录有几十个模型，`oauth.excluded-models` 按家族前缀收敛到上面三个
> （`poolctl.py` 的 `DEVIN_EXCLUDED_MODELS`，排除 claude-*/gpt-*/gemini-3-*/kimi-*/swe-1-* 等；
> 只支持通配符排除、不支持白名单，新家族上线会漏进列表，届时补一条前缀即可）。
> GOAT 上游模型 ID 为 `deepseek/deepseek-v4.1-flash`，对外统一暴露 `deepseek-v4.1-flash`。
> 同名模型出现在多个 provider 时，CPA 按 `weighted-round-robin` 在可用 provider 间分流，
> 429/500 命中 `request-scoped-errors` 的匹配串会「继续 + 冷却」而非直接失败。
>
> `max-hybrid` 模式下 Pro 号会额外带上凭据级 `excluded_models`（付费模型 + 家族前缀）；
> 注意**凭据级 excluded_models 会整表覆盖** `oauth.excluded-models`，所以 render 把两批前缀拼在一起写，
> 否则 Pro 号在混合模式下反而会暴露全部 Devin 模型。

---

## 6. 配额结构速查

金额为「等效 API 用量额度」，不是现金。

**OpenCode Go（订阅制，月付 $10 起，按账号实际套餐）**

| 项目 | 额度 |
|---|---|
| DeepSeek V4.1 Flash | **$80 / 月** |
| └ 5 小时滚动窗口 | $16 |
| └ 周窗口 | $40 |
| Gemini 4 Argon (`devin/gemini-4-argon`) | $30 / 月 |

**Command Code GOAT**

| 项目 | 额度 |
|---|---|
| DeepSeek V4.1 Flash | **$80 / 月** |
| └ 5 小时滚动窗口 | $14 |
| └ 周窗口 | $35 |

**Devin Pro / Max**：无公开的美元额度表，按请求数限流；免费模型（`swe-2`）有
「Reached free model rate limit / resource_exhausted」的独立限流，已由
`oauth.request-scoped-errors` 处理成「换号继续 + 该号冷却」。

实时额度：

```bash
python3 scripts/poolctl.py quota
```

Devin 侧走 `POST /v0/management/auth-files/refresh` → `GET /v0/management/auth-files`
读 `quota.signals`；OpenCode Go 侧走自研 `opencode-go-quota` 插件的
`/v0/management/plugins/opencode-go-quota/quota`（逐 key rolling(5h)/weekly/monthly
真实用量百分比，见 10.1）；GOAT 侧仍为容量估算。

---

## 7. 缓存保护（省钱的关键）

CPA 的 `routing` 段专门为 prompt cache 命中率服务：

```yaml
routing:
  strategy: "weighted-round-robin"
  session-affinity: true            # 同一会话粘同一凭据
  session-affinity-ttl: "2h"
  session-affinity-subagents: false # 子 agent 不继承亲和，避免打散主会话
```

**原理**：上游按「请求前缀」命中缓存，前缀一变就全量重算（cache_write 贵、cache_read 便宜）。
若同一会话的请求被轮询到不同 key/账号，前缀缓存直接作废。`session-affinity` 把会话绑定到
固定凭据 2 小时，配合 `support-prompt-cache-key: true` 透传 `prompt_cache_key`，命中率才稳。

**客户端侧必须配合**：

1. **前缀稳定**：system prompt、工具定义、文件头部顺序在整段会话内不要改动。任何把
   system prompt 里塞时间戳 / 随机 ID 的写法都会让缓存命中率归零。
2. **把 `prompt_cache_key` 固定在会话级**（Orca 里就是 session id），不要每次请求重新随机。
3. **监控 cache_read**：CPA 的 `observability.usage.usage-statistics-enabled: true` 会统计
   使用量，重点看 `cache_read` 占比。持续偏低说明前缀在变或被轮询打散：

```bash
docker compose logs --tail 200 cliproxyapi | grep -i cache_read
python3 scripts/poolctl.py quota            # 附带容量估算
```

4. 不要为「均衡」把 `session-affinity` 关掉 —— 省下的那点负载差异远小于缓存击穿的成本。

---

## 8. 扩容步骤

### 8.1 加 Devin Max 并切模式

`accounts.yaml` 加上 Max 账号（weight 7，对应 Max:Pro 额度比约 7:1）：

```yaml
devin_pool:
  mode: "max-hybrid"        # 或直接用下面的命令切换
  accounts:
    # ... 已有的 Pro 号 ...
    - id: "devin-max-1"
      email: "max1@example.com"
      plan: "Max"
      weight: 7
      session_token: "devin-session-token$..."
```

一条命令切换模式（会改 `accounts.yaml` 并自动重跑 render）：

```bash
python3 scripts/poolctl.py switch-mode max-hybrid
```

两种模式的行为差异：

| 模式 | 行为 |
|---|---|
| `pro-only` | 全部账号 weight=1 均衡，开放全部模型 |
| `max-hybrid` | Max 号 weight=7 承接付费模型；Pro 号仅跑免费的 `swe-2` |

回退：`python3 scripts/poolctl.py switch-mode pro-only`

### 8.2 OpenCode Go 从 4 个号扩到 10 个号

| 号数 | `proxies` 写法 |
|---|---|
| 4~5 | `["direct"]` —— 全部走服务器出口 |
| 6~10 | 2~3 个静态代理，轮流分配，每代理背 2~4 个号 |

```yaml
opencode_go_pool:
  proxies:
    - "socks5://user:pass@1.2.3.4:1080"
    - "socks5://user:pass@5.6.7.8:1080"
    - "socks5://user:pass@9.10.11.12:1080"
```

**规则（违反会出问题）**：

- 代理必须是**静态出口**，同一 key 永远走同一 IP —— 上游按 IP 做风控，跳 IP 会触发验证/封禁。
- **严禁**在代理 URL 末尾加 `#pool`。`#pool` 表示轮换池，会导致 IP 漂移，同时破坏
  HTTP/2 keep-alive（每次换 IP 重建连接），吞吐和 `allowance` 都会掉。
- 扩号后跑一次 `poolctl check-opencode`，确认每个 key 从自己的出口都是 200。

---

## 9. 敏感词开关（零宽混淆）

对应 CPA **原生字段** `oauth.providers.devin.sensitive-words`，不是自研中间件。

`accounts.yaml` 里控制：

```yaml
devin_pool:
  sensitive_words:
    enabled: true        # 打开
    extra: ["某个词", "另一个词"]   # 追加到内置词表
```

render 行为：

- `enabled: false`（默认）→ `sensitive-words: []`，完全不介入请求。
- `enabled: true` → 输出「内置词表 + extra」合并后的列表。

生效后，命中词表中的词会在词内插入**零宽字符**（U+200B 等），使上游关键词审计无法匹配字面量，
而模型显示层看不出差别。

内置词表随 `poolctl` 版本更新（见 `scripts/poolctl.py`）。注意事项：

- 只对 Devin 通道生效；OpenCode Go / GOAT 链路不经过这段逻辑。
- 会轻微增加 token 数（每个命中词 +1~2 token），高频命中敏感词的超长文本请评估成本。
- 这是**降低命中率**，不是保证不被识别；不要把它当成绕过条款的手段。

---

## 10. CPAMC 管理面板

CPA 自带管理面板（CPAMC），由 `management` 段启用：

```yaml
management:
  allow-remote: true
  secret-key: "{{CPA_ADMIN_SECRET}}"
```

访问：浏览器开 `http://<server>:8317`，用 `.env` 里的 `CPA_ADMIN_SECRET` 登录。

可做：查看各凭据额度/冷却状态、手动禁用账号、看 usage 明细、触发 auth 刷新。
管理 API 也可直接调（脚本 `poolctl quota` 就是这么做的）：

```bash
curl -s http://<server>:8317/v0/management/auth-files \
  -H "Authorization: Bearer $CPA_ADMIN_SECRET"
```

> `allow-remote: true` 意味着管理口对公网开放（同 8317 端口）。**这是本仓库的预期设计**：
> 公网可达性由 nginx 入口保留，风险由入口限速 + `CPA_ADMIN_SECRET` 承担（见 3.10）：
> 所有管理路径共用 10 r/min + burst 5 的同一个桶，爆破在速率上就打不动；`/v1` 不限速。
> 若某天想彻底关掉，改成 `false` 并只从容器内网 / SSH 隧道访问，同时把 override 里的
> `18317` 回环端口一起收紧。

---

### 10.1 OpenCode Go 多账号余量监控（自研 `opencode-go-quota` 插件）

仓库自带一个 CPA C ABI 插件（`plugin/opencode-go-quota/`，Go），实现 CPA 的
**`AuthProvider` + `QuotaProvider`** 双能力：

- `auth.parse` 把 render 生成的 `auths/opencode-go-pool.json`（`type: "opencode-go"`）
  注册成 CPAMC **配额管理页的一等凭据**——和 Devin 凭据并列的原生卡片。
- `quota.fetch` 并发拉取 `GET https://opencode.ai/zen/go/v1/usage`（每 key rolling(5h)/weekly/monthly
  三窗口），聚合后返回 `subscription + summary + groups`：卡片顶部是三窗口平均余量
  （「所有 key 综合」行），失败 key 单列错误行。
- 数据走原生 `/v0/management/plugins/opencode-go-quota/quota` 管理端点，
  `poolctl quota` 的第二张表也读它。

启用链路：

```bash
cd plugin/opencode-go-quota && make install   # CGO_ENABLED=1 -buildmode=c-shared
python3 scripts/poolctl.py render             # 写凭据文件 + plugins.configs
docker compose restart cliproxyapi            # 插件只在 CPA 启动时加载
```

产物落到 `cliproxy/plugins/linux/amd64/opencode-go-quota.so`（gitignore），
`render` 自动把 `opencode_go_pool.keys` 写进 `plugins.configs.opencode-go-quota`。
加/减 key 后 render + restart 即自适应；`monitoring.enabled: false` 可关闭插件配置生成。

注意事项：

- 插件的用量请求从 CPA 容器出口发出，**不经过** accounts.yaml 里给推理配的代理——
  `/usage` 是只读接口，通常无风控问题；如果某 key 的 `check-opencode` 正常而插件查询
  失败，先怀疑出口 IP 差异。
- `.so` 未安装但 `plugins.enabled: true` 时 CPA 只会在日志里报插件加载失败，不影响主网关。

---

## 11. poolctl 用法

`scripts/poolctl.py` 是唯一编排入口。注意 **CLI 只有五个子命令**（`render` / `check-opencode` / `quota` /
`switch-mode` / `export-client`）；插件构建是 `make` 的活，不在 CLI 里：

### `render`

```bash
python3 scripts/poolctl.py render
```

读 `accounts.yaml` + `.env`，写：

- `cliproxy/auths/<id>.json`（每个 Devin 账号）
- `cliproxy/auths/opencode-go-pool.json`（插件池凭据，让 CPAMC 配额页出现综合卡片）
- `cliproxy/config.yaml`（CPA v8，含 `plugins.configs.opencode-go-quota`）
- `opencode2api/config.json`（种子配置）

若 `opencode2api` 容器在跑，会额外 `docker compose cp` 进容器并调 `/api/config/reload`
（带 WebUI 登录 session）使其立即生效。

> ⚠️ 这一步是**必需**的：opencode2api 只在 state 卷首次初始化时读种子文件，之后必须靠
> cp + reload。raw `docker compose restart` 不会重新导入新配置。

CPA 侧不需要 reload —— `cliproxy/config.yaml` 与 `cliproxy/auths/*.json` 是挂载文件，变更自动热载。

### `check-opencode`

```bash
python3 scripts/poolctl.py check-opencode
```

对每个 Go key 发一条最小请求：

```
POST https://opencode.ai/inference/go/openai/v1/chat/completions
Authorization: Bearer <go_key>
User-Agent: opencode-cli/1.0
x-opencode-session: ses_healthcheck01
body: {"model":"deepseek-v4.1-flash","max_tokens":1,...}
```

结果解读：

| 状态码 | 含义 | 处理 |
|---|---|---|
| 200 | 可用 | 打印 `allowance`（剩余额度） |
| 403 | 区域不支持 | 去 `Settings › Privacy › Regions` 切 `Global` |
| 429 | 窗口冷却 | 5h / 周额度耗尽，等刷新或换号 |

### `quota`

```bash
python3 scripts/poolctl.py quota
```

Devin：`POST /v0/management/auth-files/refresh` 触发刷新，再 `GET /v0/management/auth-files`
读 `quota.signals`，打成配额表。
OpenCode Go：调自研 `opencode-go-quota` 插件的 quota.fetch 端点取逐 key
真实用量（rolling / weekly / monthly 剩余百分比 + 重置倒计时），输出第二张表。
GOAT：仍是按公开额度线性累加的**估算**表，不接实时接口。

三张表的输出顺序：Devin 实时配额 → OpenCode Go 实时余量 → 执行池容量估算。

### 插件构建（不是 CLI 子命令）

```bash
cd plugin/opencode-go-quota
make          # CGO_ENABLED=1 go build -buildmode=c-shared
make install  # → ../../cliproxy/plugins/linux/amd64/opencode-go-quota.so
make test     # go vet ./...
```

依赖 Go 1.21+ 与 CGO（c-shared 产物）。`INSTALL_DIR` 可覆盖，默认
`cliproxy/plugins/linux/amd64/`，compose 已把 `./cliproxy/plugins` 挂到 `/CLIProxyAPI/plugins`。
装完 `render` + `docker compose restart cliproxyapi` 生效（**.so 只在 CPA 启动时加载**）。
插件源在 `plugin/opencode-go-quota/`，与 `poolctl` 的耦合点只有一处：插件读
`plugins.configs.opencode-go-quota.opencode_api_keys`，而 render 把 `opencode_go_pool.keys` 原样写进去。

### `switch-mode`

```bash
python3 scripts/poolctl.py switch-mode pro-only
python3 scripts/poolctl.py switch-mode max-hybrid
```

改 `accounts.yaml` 的 `devin_pool.mode`，然后自动重跑 `render`（含 opencode2api 的 cp + reload）。

### `export-client`

```bash
python3 scripts/poolctl.py export-client
```

> 该子命令**总是**写 `export/env.sh`（权限 600）并同时打印到 stdout，没有 `--write` 开关。

产物：

- `export/env.sh`：`OPENAI_BASE_URL` / `OPENAI_API_KEY` 等客户端环境变量
- 本地 Devin `config.json` 模板（防免费池限流三件套，见第 12 节）

---

## 12. 本地 Devin 配置（防免费池限流）

`templates/local-devin-config.json`：

```json
{
  "subagents": { "enabled": false },
  "reasoningEffort": "high",
  "fastMode": false
}
```

复制到 `~/.config/devin/config.json`。三个字段的意义：

| 字段 | 值 | 原因 |
|---|---|---|
| `subagents.enabled` | `false` | 关掉子 agent 并发扇出 —— 免费池限流按请求数算，subagent 会把一个任务放大成 N 个请求，最容易撞 429 |
| `reasoningEffort` | `"high"` | 单次想清楚，减少来回试探的轮次（轮次 = 请求数） |
| `fastMode` | `false` | fast 模式不减少额度消耗却仍按请求计费，免费池不划算 |

> 文件为**纯 JSON**（不含 `_comment` 字段）—— 目标程序会拒绝未知字段，说明只放在本 README。

---

## 13. 故障排查 FAQ

**Q1：`curl :8317/v1/models` 连不上？**
先 `docker compose ps`：现在有 **三个**服务（`ingress` / `cliproxyapi` / `opencode2api`），全都要 healthy。
然后分层排：

```bash
docker compose logs --tail 50 ingress       # nginx 配置/上游解析问题
docker compose logs --tail 50 cliproxyapi   # config.yaml 语法错误会在这里退出
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18317/v1/models   # 绕过入口
python3 -c "import yaml;yaml.safe_load(open('cliproxy/config.yaml'))"
```

18317 通、8317 不通 → 问题在 ingress（看 `nginx -t` 与 `env` 里的 `resolver`）；
两个都不通 → 问题在 CPA 本体。

**Q11：`poolctl quota` 报 `HTTP 429` / 管理接口被限速？**
这是入口的管理面限速（10 r/min + burst 5），不是 CPA 出问题。加号后一次刷十几个凭据会撞。
处理：① 等一分钟重跑；② 把 `nginx/nginx.conf` 里 `orca_admin` 的 `rate=` 调到 30r/m、
`/v0/` 的 `burst=` 调到 20，然后 `docker compose restart ingress`；
③ 排障时可临时用 `127.0.0.1:18317` 直连 CPA 绕开入口。

> `/v1`（模型调用）**不受任何入口限速影响**——如果你看到 `/v1` 返回 429，
> body 里一定有上游限流特征（`Reached free model rate limit` / `resource_exhausted`），见 Q4 / Q9。

**Q12：`poolctl quota` 报 "未发现 opencode-go 凭据"？**
插件链路没走通，按顺序查：① `make install` 装过 `.so` 没有（`ls cliproxy/plugins/linux/amd64/`）；
② `render` 后有没有 `docker compose restart cliproxyapi`（插件只在启动时加载）；
③ `accounts.yaml` 里 `monitoring.enabled` 是不是被关成 `false`（关了就不输出 `plugins` 段）；
④ `docker compose logs cliproxyapi | grep -i plugin` 看加载失败原因。

**Q2：改了 `opencode2api/config.json` 但行为没变？**
这是最常见的坑。opencode2api **只在 state 卷为空时**导入种子。必须走
`poolctl render`（自动 cp + reload），或手工：

```bash
docker compose cp opencode2api/config.json opencode2api:/run/config/opencode2api.json
# 然后 WebUI / 管理 API 触发 reload
```

想彻底重来：`docker compose down && docker volume rm orca-ai-pool_opencode2api-state && docker compose up -d`
（会清空运行态，慎用）。

**Q3：`deepseek-v4.1-flash` 稳定 403？**
Go 账号没开 Privacy Global。见 4.2，改完 `poolctl check-opencode` 复验。

**Q4：`devin/swe-2` 间歇 429，日志里有 `Reached free model rate limit`？**
Devin 免费池限流。`request-scoped-errors` 已配置成「换下一个凭据 + 当前号冷却」，正常现象。
持续 429 说明 Pro 号数量不够，扩号或加 Max 走 `max-hybrid`。

**Q5：上游 401 / token 失效？**
Devin session token 过期。重抓（4.1）→ 改 `accounts.yaml` → `poolctl render`。

**Q6：cache_read 占比很低，成本远超预期？**
① `session-affinity` 是否被关掉；② 客户端 system prompt 是否含时间戳/随机串；
③ 是否每次请求换了 `prompt_cache_key`。见第 7 节。

**Q7：`poolctl render` 报 `KeyError` / 配置校验失败？**
`.env` 或 `accounts.yaml` 缺字段。`accounts.example.yaml` 是完整 schema 参考，
逐字段比对；`session_token` 必须含 `devin-session-token$` 前缀。

**Q8：WebUI 打不开？**
它只绑 `127.0.0.1:18081`（故意的）。用 SSH 隧道：
`ssh -L 18081:127.0.0.1:18081 user@server`，本地访问 `http://127.0.0.1:18081`。

**Q9：新加的 Go key 一直 429？**
先确认是不是同一个出口 IP 上号太多（IP 级限流）。按 8.2 分配静态代理，一号一出口。
另外检查是不是 `#pool` 轮换代理导致 IP 漂移。

**Q10：想临时摘掉某个号？**
CPA 面板（第 10 节）里禁用即可，不影响其他号；长期移除再从 `accounts.yaml` 删掉并 render。
