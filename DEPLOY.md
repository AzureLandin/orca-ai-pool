# 生产部署指引（新加坡服务器）

> 目标机：`root@43.160.254.193`（Debian 13，Docker 26.1.5 + Compose 2.26.1 已装好，镜像已预拉）
> 部署路径：`/opt/orca-ai-pool/`

---

## 第一步：填 `.env`

仓库里的 `.env.example` 复制为 `.env`，逐项填：

```bash
cp .env.example .env && vim .env
```

| 变量 | 填什么 | 生成方法 |
|---|---|---|
| `GATEWAY_HOST` | 服务器公网 IP | `43.160.254.193` |
| `GATEWAY_PORT` | 对外端口 | `8317` |
| `MASTER_API_KEY` | 客户端访问 CPA 的主 Key | `openssl rand -hex 32` |
| `CPA_ADMIN_SECRET` | CPAMC 管理面板/管理 API 密码 | `openssl rand -hex 24` |
| `OC2API_SERVER_KEY` | CPA→opencode2api 内网认证 key | `openssl rand -hex 24` |
| `OC2API_WEBUI_USER` | opencode2api WebUI 用户名 | `admin` |
| `OC2API_WEBUI_PASSWORD` | WebUI 密码（≥10字符） | `openssl rand -hex 12` |

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
cd plugin/opencode-go-quota && make install && cd ../..

# 4. 起栈
docker compose up -d

# 5. 验证（30 秒后）
docker compose ps
curl -s http://127.0.0.1:8317/v1/models \
  -H "Authorization: Bearer <MASTER_API_KEY>" | python3 -m json.tool | head -30
```

模型列表只含 `devin/swe-2`、`devin/swe-2-lightning`、`devin/gemini-4-argon`、`deepseek-v4.1-flash` 四个模型即成功。

## 第四步：面板访问

- **CPAMC 管理面板**：`http://<服务器IP>:8317/management.html`，密码 = `CPA_ADMIN_SECRET`
- **Go 多账号余量**：CPAMC 配额管理页的 `opencode-go-pool.json` 凭据卡片（综合三行 + 逐 key 分组）
- **opencode2api WebUI**：`http://127.0.0.1:18081`（只绑回环，需 SSH 转发：`ssh -L 18081:127.0.0.1:18081 root@43.160.254.193`），账号 `admin` / `OC2API_WEBUI_PASSWORD`

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

- 8317 公网已放行（实测 200）。如果只要管理面板用、不想暴露管理 API，把 `management.allow-remote` 改 `false` 后用 SSH 转发访问。
- 18081 已绑定 `127.0.0.1`，外部访问必须走 SSH 隧道。
