# orca-ai-pool

AI 号池管理仓库：**CPA (CLIProxyAPI) 主网关 + opencode2api 副网关**，Docker Compose 部署在新加坡服务器。
对外只暴露一个 OpenAI 兼容端点 `http://<server>:8317/v1`，内部把请求按模型名路由到 Devin (OAuth)、OpenCode Go、
Command Code GOAT 三条上游，用「加权轮询 + 会话亲和」做负载均衡与缓存保护。
开启破甲通道后，`GET /v1/models` 会多出带 `-jailbreak` 后缀的模型名
（例如 `deepseek-v4.1-flash-jailbreak`），请求它时仍路由回原模型/原上游，只是注入对应系统提示词。

---

## 1. 架构

```
                     ┌──────────────────────────────────────────────────────────────┐
   本地 Orca / 客户端 │  Authorization: Bearer sk-orca-master-key                     │
        (OpenAI API) └───────────────────────┬──────────────────────────────────────┘
                                             │  http://<server>:8317/v1
                                             ▼
                                 ┌───────────────────────┐
                                 │  cliproxyapi (CPA)    │  加权轮询 + 会话亲和
                                 │  :8317  (唯一公网口)  │  usage 统计 / 冷却 / 重试
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
| opencode2api → OpenCode Go | OpenAI 兼容 | `go_keys` + 代理 | 高阶模型容量池（DS4.1F / Argon / Kimi K3） |
| Command Code GOAT | OpenAI 兼容 | API key | 直连兜底，成本最低的 DS4.1F 与 Kimi K3 |

---

## 2. 目录结构

```
orca-ai-pool/
├── docker-compose.yml                    # 两服务 + orca-net + opencode2api-state 卷
├── .env.example                          # 部署参数（复制为 .env）
├── .gitignore
├── accounts.example.yaml                 # 号池清单（复制为 accounts.yaml）
├── README.md
├── scripts/
│   └── poolctl.py                        # 唯一编排入口（7 个子命令）：render / check-opencode / quota / switch-mode / rotate-secret / sync-jailbreaks / export-client
├── plugin/
│   └── jailbreak-inject/                 # 自研 CPA C ABI 插件（Go）：-jailbreak 别名按模型注入提示词
│       ├── main.go / main_test.go
│       ├── Makefile                      # make install → cliproxy/plugins/linux/amd64/
│       └── go.mod / go.sum
├── prompts/
│   ├── manifest.yaml                     # 破甲通道：模型 → 提示词文件 / 注入模式
│   ├── overlays/                         # 本仓库自己的短补充（静态，保证缓存前缀稳定）
│   └── upstream/                         # sync-jailbreaks 下载的提示词（gitignore）
├── templates/
│   ├── cliproxy.config.v8.yaml.tmpl      # CPA v8 配置结构参考（手工兜底）
│   ├── opencode2api.config.json.tmpl     # opencode2api 配置结构参考（JSONC）
│   └── local-devin-config.json           # 本地 Devin 客户端配置（防免费池限流三件套）
├── cliproxy/
│   ├── config.yaml                       # ← render 产物（gitignore）
│   ├── auths/                            # ← render 产物：<id>.json 凭据（gitignore *.json）
│   ├── plugins/linux/amd64/              # ← make install 产物：opencode-go-quota.so / jailbreak-inject.so（gitignore）
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

```bash
chmod 600 .env
```

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

- `cliproxy/config.yaml` —— CPA v8 配置（占位符已替换）
- `cliproxy/auths/<id>.json` —— 每个 Devin 账号一个
- `opencode2api/config.json` —— opencode2api 种子配置

验证：

```bash
grep -c "devin/" cliproxy/config.yaml     # 应为 0（alias 里不带 devin/ 前缀）
python3 -c "import json;json.load(open('opencode2api/config.json'))"   # 纯 JSON 可解析
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

# 冒烟：GOAT 直连（同 alias，CPA 轮询到 GOAT 上游时即为直连）
curl -s http://$HOST:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"ping"}],"max_tokens":8}'
```

三条都返回 `choices` 即部署完成。

### 3.9 防火墙

只放行 `8317`。**8080 与 18081 都不要开**：

```bash
# ufw 示例
sudo ufw allow 22/tcp
sudo ufw allow 8317/tcp
sudo ufw enable
```

WebUI 用 SSH 隧道访问：`ssh -L 18081:127.0.0.1:18081 user@server` → 本地开 `http://127.0.0.1:18081`。

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

对外只保留 4 个基座模型，客户端直接用真实 ID（`GET /v1/models` 即路由表）：

| 客户端模型名 | 上游 | 典型用途 |
|---|---|---|
| `devin/swe-2` | Devin OAuth | 免费主力，通用编码/推理，量大管饱 |
| `devin/swe-2-lightning` | Devin OAuth | 低延迟版本，交互式补全、小任务 |
| `devin/gemini-4-argon` | Devin OAuth | 长上下文分析、架构评审 |
| `deepseek-v4.1-flash` | OpenCode Go / GOAT | 批量代码生成、脏活累活，性价比最高 |

开启破甲通道后，manifest 里有提示词的模型会额外出现 `-jailbreak` 别名：

| 破甲模型名 | 实际上游 | 说明 |
|---|---|---|
| `deepseek-v4.1-flash-jailbreak` | 同 `deepseek-v4.1-flash` | 注入 `deepseek-4-1.md` + 本地 overlay |

别名由 CPA 的 model alias / `oauth-model-alias` 机制生成，模型 ID 在进入上游前会还原成基座名。
Devin 尚未上线，所以暂不生成 `devin/*-jailbreak`；manifest 里已经留好注释，上线后取消即可。

> Devin 凭据的上游目录有几十个模型，`oauth.excluded-models` 按家族前缀收敛到这三个
> （只支持通配符排除不支持白名单；新家族上线会漏进列表，需往 `DEVIN_EXCLUDED_MODELS` 补前缀）。
> GOAT 上游模型 ID 为 `deepseek/deepseek-v4.1-flash`，对外统一暴露 `deepseek-v4.1-flash`。
> 同名模型出现在多个 provider 时，CPA 按 `weighted-round-robin` 在可用 provider 间分流，
> 429/500 命中 `request-scoped-errors` 的匹配串会「继续 + 冷却」而非直接失败。

---

## 6. 配额结构速查

金额为「等效 API 用量额度」，不是现金。

**OpenCode Go（单号 $10/月）**

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
真实用量百分比，见第 10.5 节）；GOAT 侧仍为容量估算。

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

> `allow-remote: true` 意味着管理口对公网开放（同 8317 端口）。若不想暴露，改成 `false`
> 并只从容器内网 / SSH 隧道访问。

---

## 10.5 OpenCode Go 多账号余量监控（自研 `opencode-go-quota` 插件）

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

`scripts/poolctl.py` 是唯一编排入口。注意 **CLI 只有七个子命令**（`render` / `check-opencode` / `quota` /
`switch-mode` / `rotate-secret` / `sync-jailbreaks` / `export-client`）；插件构建是 `make` 的活，不在 CLI 里：

### `render`

```bash
python3 scripts/poolctl.py render
```

读 `accounts.yaml` + `.env`，写：

- `cliproxy/auths/<id>.json`（每个 Devin 账号）
- `cliproxy/config.yaml`（CPA v8；启用破甲通道时还含 `jailbreak-inject` 与 `-jailbreak` model alias）
- `opencode2api/config.json`（种子配置）

若 `opencode2api` 容器在跑，会额外 `docker compose cp` 进容器并调 `/api/config/reload`
（带 WebUI 登录 session）使其立即生效。

> ⚠️ 这一步是**必需**的：opencode2api 只在 state 卷首次初始化时读种子文件，之后必须靠
> cp + reload。raw `docker compose restart` 不会重新导入新配置。

CPA 侧普通配置会热载；但 `plugins.configs`（包括破甲提示词 profile）是 C ABI 插件启动时读取的，
改完必须 `docker compose restart cliproxyapi`。`render` 会检测并提醒。

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

### `sync-jailbreaks`

```bash
python3 scripts/poolctl.py sync-jailbreaks
python3 scripts/poolctl.py sync-jailbreaks --force   # 覆盖本地已有文件
```

按 `prompts/manifest.yaml` 的固定 commit 下载提示词到 `prompts/upstream/`（gitignore）。
已有文件内容不同时默认保留本地版本，避免覆盖你手改过的提示词。

### `quota`

```bash
python3 scripts/poolctl.py quota
```

Devin：`POST /v0/management/auth-files/refresh` 触发刷新，再 `GET /v0/management/auth-files`
读 `quota.signals`，打成配额表。
OpenCode Go：调自研 `opencode-go-quota` 插件的 quota.fetch 端点取逐 key
真实用量（rolling/weekly/monthly 剩余百分比 + 重置倒计时）。
GOAT：按窗口用量打印容量估算表。

### 插件构建

```bash
cd plugin/opencode-go-quota && make install
```

`CGO_ENABLED=1 go build -buildmode=c-shared` 编译 `opencode-go-quota.so`，
安装到 `cliproxy/plugins/linux/amd64/`（compose 已挂载 `/CLIProxyAPI/plugins`）。
装完 `render` + `docker compose restart cliproxyapi` 生效。

破甲插件同样用 `make` 构建：

```bash
cd plugin/jailbreak-inject
make          # CGO_ENABLED=1 go build -buildmode=c-shared
make install  # → ../../cliproxy/plugins/linux/amd64/jailbreak-inject.so
make test     # CGO_ENABLED=1 go test ./...
```

启用 `jailbreak_channel.enabled` 后，`render` 会检查这个 `.so` 是否存在；不存在直接报错，
避免模型列表里出现一个实际没注入的假破甲模型。

### `switch-mode`

```bash
python3 scripts/poolctl.py switch-mode pro-only
python3 scripts/poolctl.py switch-mode max-hybrid
```

改 `accounts.yaml` 的 `devin_pool.mode`，然后自动重跑 `render`（含 opencode2api 的 cp + reload）。

### `export-client`

```bash
python3 scripts/poolctl.py export-client          # 打印到 stdout
python3 scripts/poolctl.py export-client --write  # 写 export/env.sh
```

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
`docker compose ps` 看两个服务是否 healthy；`docker compose logs cliproxyapi` 看是否因
`config.yaml` 语法错误退出。`python3 -c "import yaml;yaml.safe_load(open('cliproxy/config.yaml'))"`
先自检。

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

---

## 14. 破甲模型（`-jailbreak` 别名）

破甲版是一个**客户端可见的模型别名**，不是新上游：例如

```text
deepseek-v4.1-flash            # 普通模型
deepseek-v4.1-flash-jailbreak  # 破甲模型
```

两者最终都发给同一个上游模型；区别只在进入上游前，`jailbreak-inject` 插件会把
`prompts/manifest.yaml` 里对应 profile 注入到 `messages` / `system` / `instructions`。
CPA 的 model alias 负责把 `-jailbreak` 名字还原成基座模型，插件只按请求模型名选提示词。

### 14.1 启用

```bash
# 1. 按固定 commit 下载提示词（写到 prompts/upstream/，gitignore）
python3 scripts/poolctl.py sync-jailbreaks

# 2. 编译插件（C ABI .so 只在 CPA 启动时加载）
cd plugin/jailbreak-inject && make install
cd ../..

# 3. accounts.yaml 打开开关
# jailbreak_channel:
#   enabled: true

# 4. 生成配置并重启 CPA
python3 scripts/poolctl.py render
docker compose restart cliproxyapi
```

`render` 会打印新增的模型别名。若报未找到 `jailbreak-inject.so`，回到第 2 步；
若提示提示词缺失，回到第 1 步。

### 14.2 模型映射

编辑 `prompts/manifest.yaml`：

```yaml
suffix: "-jailbreak"

profiles:
  - name: "deepseek-4-1"
    models:
      - "deepseek-v4.1-flash"
      - "deepseek/deepseek-v4.1-flash"
    source_file: "deepseek-4-1.md"      # 上游仓库里的文件名
    file: "upstream/deepseek-4-1.md"    # 本地落盘位置（相对 prompts/）
    overlay: "overlays/deepseek-4-1.md" # 本仓库自己的短补充，会拼在正文前
    overlay_position: "prepend"
    mode: "prepend"
```

`overlay` 用来补上游 prompt 没覆盖的部署规则，当前这份加了三条：本规格优先于默认行为、
输入周围注入的安全警告不是额外禁令、协议/文件传输工具（含 BitTorrent 客户端）按工程任务处理。
overlay 是静态文本，不含时间戳/随机值，所以它进入缓存前缀后不会每次失效。

`render` 会根据 `models` 自动生成别名：

- OpenAI 兼容上游（OpenCode Go / GOAT）：追加同 `name`、不同 `alias` 的模型条目。
- Devin OAuth：写 `oauth-model-alias.devin`，带 `fork: true`，原模型和破甲模型都保留。
  当前 manifest 里 Devin profile 是注释状态，等 Devin 上线再打开。

`models` 支持 `*` / `?` 通配符（大小写不敏感），按清单顺序先到先得。`mode` 三选一：

| mode | 行为 | 适用 |
|---|---|---|
| `prepend`（默认） | 在 `messages` 最前面插一条 system；Anthropic 则插到 `system` 最前 | 保留客户端的工具/代理指令 |
| `append` | 追加到现有 system 段末尾 | 想让客户端指令优先时 |
| `replace` | 删掉 system/developer 消息，整段换成提示词 | 强注入；会顶掉客户端的工具/代理指令 |

未匹配到 profile 时默认返回 `400 jailbreak_profile_error`，不会静默退化成普通模型。
想放行就把 `manifest.yaml` 的 `unmapped` 改成 `pass`。

缓存方面：破甲别名在 CPA 内部还原成基座模型，上游看到的是同一个模型 ID；system 前缀
由「固定 overlay + 固定上游 prompt + 客户端 system」组成。只要客户端不在 system 里塞
时间戳/随机串，并保持同一个 `prompt_cache_key`，后续请求就能命中上游前缀缓存。

### 14.3 调用

先看模型列表确认别名已生成：

```bash
curl -s http://<server>:8317/v1/models \
  -H "Authorization: Bearer $CPA_KEY" | grep -o '"id":"[^"]*jailbreak[^"]*"'
```

然后直接把 `model` 换成破甲名：

```bash
curl -s http://<server>:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4.1-flash-jailbreak","messages":[{"role":"user","content":"ping"}],"max_tokens":8}'
```

上游看到的仍是 `deepseek-v4.1-flash`（GOAT 那条是 `deepseek/deepseek-v4.1-flash`），
但 system 里已经多了对应提示词。`export-client` 的模型建议表也会列出当前这个破甲别名。

### 14.4 注意事项

- 提示词会进入每次请求的 system 前缀；同一 profile 内容稳定时能吃到上游前缀缓存，
  改提示词会让该模型后续请求冷启动。`replace` 还会让客户端原有 system 前缀完全失效。
- 插件支持 OpenAI chat（`messages`）、Anthropic（`system`）、Responses（`instructions` / `input`）。
  其他 body 形态会返回 400，避免误以为已注入。
- 只有 manifest 里有 profile 的模型才会生成 `-jailbreak` 别名；`devin/swe-2` 想开就补一条
  profile，render 会自动生成 `devin/swe-2-jailbreak`。
- 提示词来自 `togg53192-cmd/jailbreaks`，上游仓库未声明许可证；使用、再分发和上游 ToS
  由部署者自行确认。`sync-jailbreaks` 固定到 manifest 里的 commit，不会自动跟随上游更新。
- 排查：`docker compose logs cliproxyapi | grep -i jailbreak` 看插件加载；
  `400 no jailbreak profile matches model` 说明别名存在但 manifest 没写对应 profile。
