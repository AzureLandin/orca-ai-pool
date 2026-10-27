# 生产部署指引（新加坡服务器）

> 目标机：`root@<YOUR_SERVER_IP>`（Debian 13，Docker 26.1.5 + Compose 2.26.1 已装好，镜像已预拉）
> 部署路径：`/opt/orca-ai-pool/`
>
> 相关文档：[说明文档](README.md) · [技术文档](docs/TECHNICAL.md) ·
> [容量规划](docs/capacity-planning.md)

```mermaid
flowchart LR
    A["系统依赖"] --> B[".env / accounts.yaml"]
    B --> C["poolctl render"]
    C --> D["构建插件"]
    D --> E["docker compose up"]
    E --> F["install-nginx.sh"]
    F --> G["模型列表 + 冒烟请求"]
```

---

## 第零步：系统依赖

```bash
sudo apt-get update
sudo apt-get install -y nginx python3-yaml golang-go build-essential openssl
```

`python3-yaml` 是 `poolctl render` 的运行时依赖；`nginx` 提供公网入口和 TLS；
`golang-go`/`build-essential` 用于构建 C ABI 插件（Go 1.21+）。

## 第一步：填 `.env`

仓库里的 `.env.example` 复制为 `.env`，逐项填：

```bash
cp .env.example .env && vim .env
```

| 变量 | 填什么 | 生成方法 |
|---|---|---|
| `GATEWAY_HOST` | 服务器公网 IP | `<YOUR_SERVER_IP>` |
| `GATEWAY_PORT` | 对外端口 | `8317` |
| `MASTER_API_KEY` | 客户端访问 CPA 的主 Key | `openssl rand -hex 32` |
| `CPA_ADMIN_SECRET` | CPAMC 管理面板/管理 API 密码 | `openssl rand -hex 24` |
| `OC2API_SERVER_KEY` | CPA→opencode2api 内网认证 key | `openssl rand -hex 24` |
| `OC2API_WEBUI_USER` | opencode2api WebUI 用户名 | `admin` |
| `OC2API_WEBUI_PASSWORD` | WebUI 密码（≥10字符） | `openssl rand -hex 12` |
| `DISABLE_AUTH` | 客户端是否免鉴权，生产**必须** `false` | 保持默认 `false` |

生成示例（在本机跑，把输出填进 .env）：

```bash
echo "MASTER_API_KEY=sk-$(openssl rand -hex 32)"
echo "CPA_ADMIN_SECRET=$(openssl rand -hex 24)"
echo "OC2API_SERVER_KEY=sk-$(openssl rand -hex 24)"
echo "OC2API_WEBUI_PASSWORD=$(openssl rand -hex 12)"
```

## 第二步：填 `accounts.yaml`

```bash
cp accounts.example.yaml accounts.yaml && vim accounts.yaml
```

逐段填：

**`devin_pool.accounts`** — 每个 Devin 号一条：
- `id`：凭据文件名（`devin-pro-1`、`devin-pro-2`…）
- `email`：账号邮箱（标记用）
- `plan`：`Pro` 或 `Max`
- `weight`：Pro=1，Max=7（可省略用默认）
- `session_token`：从 Devin 网页版 DevTools 抓 `devin-session-token$...` 整个值（含前缀）
- `sensitive_words.enabled`：`false`（想开零宽混淆再改 `true`）

**`opencode_go_pool`** — 4~10 个 Go 号：
- `proxies`：≤5 号保持 `["direct"]`；6~10 号写 2~3 个静态 `socks5://user:pass@ip:port`，**禁止 `#pool` 后缀**
- `keys`：OpenCode Go console 生成的 API key 列表

**`command_code_goat_pool.keys`** — 1~4 个 GOAT 号：
- `name`：标记名（`goat-1`…）
- `api_key`：Command Code Studio › API keys 创建的 key
- `weight`：默认 1

## 第三步：渲染 + 起栈

```bash
cd /opt/orca-ai-pool

# 1. 渲染配置（生成 cliproxy/config.yaml、auths/*.json、opencode2api/config.json）
python3 scripts/poolctl.py render

# 2. 预检 Go 号（每个 key 发一次真实请求，403/429 会明确提示）
python3 scripts/poolctl.py check-opencode

# 3. 构建余量监控插件（opencode-go-quota.so → cliproxy/plugins/linux/amd64/）
#    需要 Go 1.21+ 与 CGO；没有插件也能跑主网关，只是 CPAMC 少一张 Go 余量卡
cd plugin/opencode-go-quota && make install && cd ../..

# 3.1 可选：破甲模型（/v1/models 里多出 <模型名>-jailbreak）
#     先在 accounts.yaml 里把 jailbreak_channel.enabled 改成 true
python3 scripts/poolctl.py sync-jailbreaks
cd plugin/jailbreak-inject && make install && cd ../..
python3 scripts/poolctl.py render
# 若栈已经在跑：docker compose restart cliproxyapi；首次部署直接走第 4 步

# 4. 起栈
docker compose up -d
# 安装 nginx 入口（配置来自 render 产物；首次会生成自签证书）
sudo bash scripts/install-nginx.sh

# 5. 验证（30 秒后）
docker compose ps      # 两个容器：cliproxyapi / opencode2api（nginx 装在宿主机上）
curl -s http://127.0.0.1:8317/v1/models \
  -H "Authorization: Bearer <MASTER_API_KEY>" | python3 -m json.tool | head -30
# 绕过入口直连 CPA，用于区分「入口问题」和「CPA 问题」
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18317/v1/models
```

模型列表只含 `devin/swe-2`、`devin/swe-2-lightning`、`devin/gemini-4-argon`、`deepseek-v4.1-flash` 四个模型即成功。

## 第四步：面板访问

> 8317 现在是 **nginx 入口**（`nginx/conf.d/orca.conf`，由 `poolctl render` 生成并拷到
> `/etc/nginx/conf.d/`；公共代理片段在 `/etc/nginx/snippets/orca-proxy.conf`）。
> **只有管理面限流**：`/management.html`、`/assets/*`（burst 100）、`/v0/management/*`、
> `/v8/management/*`（burst 30），稳态 5 r/s per IP；插件配额端点单独 12 r/m（burst 5）。
> **模型面 `/v1/*` 不限速、不限并发**——SSE 长流和并行子 agent 会自己放大瞬时并发，
> 给模型面设桶最先打到的就是最该重试的请求。排障时走 `127.0.0.1:18317` 绕开入口。
> `install-nginx.sh` 首次会生成自签证书；通过 HTTPS 访问时需信任该证书，或在临时排障时用 `curl -k`。


- **CPAMC 管理面板**：`http://<服务器IP>:8317/management.html`，密码 = `CPA_ADMIN_SECRET`
- **Go 多账号余量**：CPAMC 配额管理页的 `opencode-go-pool.json` 凭据卡片；`poolctl quota`
  的第二张表会展开每个 key 的 rolling/weekly/monthly 余量
- **opencode2api WebUI**：`http://127.0.0.1:18081`（只绑回环，需 SSH 转发：`ssh -L 18081:127.0.0.1:18081 root@<YOUR_SERVER_IP>`），账号 `admin` / `OC2API_WEBUI_PASSWORD`

## 常用运维命令

| 操作 | 命令 |
|---|---|
| 看容器状态 | `docker compose ps` / `docker compose logs -f` |
| 加/删号后重新渲染 | `python3 scripts/poolctl.py render`（自动热载） |
| 查配额 | `python3 scripts/poolctl.py quota` |
| 切换 Max 混合模式 | `python3 scripts/poolctl.py switch-mode max-hybrid` |
| 导出本地客户端配置 | `python3 scripts/poolctl.py export-client` |
| 停栈 | `docker compose down` |
| 停栈并清数据卷 | `docker compose down -v`（会清 opencode2api 运行态） |

## 防火墙提醒

- **公网放行 `8317`；启用 TLS 时同时放行 `443`**。两者都走 nginx（仅管理面限速，
  `/v1` 纯透传）。
- `18317` 是 CPA 的宿主机回环端口（`127.0.0.1`），只用于排障，绝不要对公网开放。
- `18081` 已绑定 `127.0.0.1`，外部访问必须走 SSH 隧道；`8080` 未发布到宿主机。
- 回滚到「CPA 直接对外」：把 `docker-compose.yml` 的端口改回 `"8317:8317"`、
  `poolctl render` 前把 `GATEWAY_INTERNAL_PORT` 设成 8317（或直接改 `cliproxy/config.yaml` 的
  `server.port`），然后 `systemctl stop nginx && docker compose up -d cliproxyapi`。
  注意回滚后管理面**失去限流**，只剩 CPA 自己的「5 次失败封 IP 30 分钟」。

## 容量与选型

号数怎么定、缓存命中率为什么是决定性的、SWE-2 的免费口径冲突，见
[`docs/capacity-planning.md`](docs/capacity-planning.md)。
