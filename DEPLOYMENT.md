# WorkBuddy2API 网关部署与升级

本文档对应本仓库当前发布版本，说明如何在全新环境下部署网关，以及如何在升级时保留账号和统计数据。

## 1. 组件关系

本仓库是网关本体，默认监听 `7863`。管理页面是独立项目 `workbuddy2api-gui-enhanced`，默认监听 `8787`。两者通过 HTTP API 和共享的 `auths/`、`config.json` 协作。

推荐目录结构：

```text
workbuddy-stack/
├── workbuddy2api/       # 本仓库
└── workbuddy2api-gui/   # 管理页面仓库
```

## 2. 环境要求

- Docker Desktop 或 Docker Engine API 24+
- Docker Compose v2
- 一个已授权、可登录的 CodeBuddy 国内版或国际版账号
- 可为国际版上游 `www.workbuddy.ai` 提供正常出站网络

Windows PowerShell、macOS 和 Linux 的 Docker 命令相同，只有宿主路径写法不同。

## 3. 全新部署

### 3.1 获取源码

```bash
git clone https://github.com/1731373663/workbuddy2api-global-fix.git workbuddy2api
cd workbuddy2api
```

### 3.2 创建配置

```bash
cp config.example.json config.json
```

至少修改 `config.json` 中的：

```json
{
  "api_key": "请换成足够长的随机密钥",
  "global": {
    "enabled": true
  }
}
```

`api_key` 为空时不鉴权，不要把无鉴权端口暴露到公网。`config.example.json` 中的 `test_key` 只是占位符。

### 3.3 构建并启动网关

```bash
docker compose up -d --build
```

当前仓库的 `docker-compose.yml` 使用 `build: .`，会从本仓库源码构建 `workbuddy2api` 镜像。不要只执行 `docker compose up -d` 后误以为已更新；升级源码时必须带 `--build`。

检查状态：

```bash
docker compose ps
curl -s http://127.0.0.1:7863/healthz
```

预期 `/healthz` 返回 HTTP 200；没有可用账号时会返回 503。

### 3.4 添加账号

默认部署目录下执行：

```bash
docker compose exec -it wb2api bash -c './login.sh'
docker compose restart wb2api
```

也可以按原项目方式在宿主机执行 `./login.sh`。登录脚本会在 `auths/` 生成 `workbuddy-<uid>.json`。

如果宿主机用户不是容器内的 `10001`，需要让目录归属正确：

```bash
sudo chown -R 10001:10001 ./auths ./data
```

Windows Docker Desktop 绑定挂载通常由 Docker Desktop 自动映射，但升级、恢复或从另一台机器复制数据后仍应检查容器日志中的载入账号数量。

### 3.5 验证网关

```bash
curl -s http://127.0.0.1:7863/v1/models \
  -H "Authorization: Bearer 你的_api_key"

curl -s http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer 你的_api_key" \
  -H "Content-Type: application/json" \
  -d '{"model":"cn:deepseek-v4.1-flash","messages":[{"role":"user","content":"ping"}],"stream":false}'
```

国际版请求把模型改为 `global:deepseek-v4.1-flash`。第一次请求失败时，先查看日志，再判断账号、网络和上游状态，不要只看 `/status` 的 `healthy` 字段。

## 4. 数据与持久化

必须保留以下内容：

| 路径 | 内容 | 是否可重新生成 |
|---|---|---|
| `auths/*.json` | 账号凭证和刷新令牌 | 否，必须备份 |
| `config.json` | 网关配置和 API 密钥 | 否，必须备份 |
| `data/state.json` | 账号池冷却、熔断、成功统计 | 建议备份 |
| `data/metrics.json` | 请求统计和最近详情 | 建议备份 |
| `data/` 其他文件 | 运行时状态 | 视情况备份 |

不要提交或公开上传 `auths/`、`data/`、真实 `config.json`。仓库本身的 `.gitignore` 已排除这些路径。

## 5. 升级

升级前先备份：

```bash
docker compose stop
cp -a auths auths.backup-$(date +%Y%m%d)
cp -a data data.backup-$(date +%Y%m%d)
cp config.json config.json.backup-$(date +%Y%m%d)
```

Windows PowerShell 可用：

```powershell
docker compose stop
Copy-Item -Recurse auths "auths.backup-$(Get-Date -Format yyyyMMdd)"
Copy-Item -Recurse data "data.backup-$(Get-Date -Format yyyyMMdd)"
Copy-Item config.json "config.json.backup-$(Get-Date -Format yyyyMMdd)"
```

升级步骤：

```bash
git fetch origin
git pull --ff-only
docker compose up -d --build
docker compose ps
docker compose logs --tail=200 wb2api
```

确认 `data/metrics.json` 存在，并请求 `GET /v1/stats`。当前版本会在启动时读取并导入已有的 `metrics.json`，不需要手工删除统计文件。

## 6. 管理页面部署

管理页面位于另一个仓库：

```text
https://github.com/1731373663/workbuddy2api-gui-enhanced
```

在推荐目录结构下，网关目录为 `workbuddy2api/`，GUI 目录为 `workbuddy2api-gui/`。GUI 的 Compose 默认使用：

```yaml
WBGUI_GATEWAY_URL: "http://host.docker.internal:7863"
```

并将 `../workbuddy2api/auths` 和 `../workbuddy2api/config.json` 挂载到面板容器。部署前请按实际相对位置修改这两个挂载路径，然后执行：

```bash
cd ../workbuddy2api-gui
docker compose up -d --build
```

访问 `http://127.0.0.1:8787`。首次部署后立即修改默认面板口令。

## 7. Docker 代理注意事项

如果 Docker 容器设置了宿主代理：

```text
HTTP_PROXY=http://127.0.0.1:7897
HTTPS_PROXY=http://127.0.0.1:7897
```

这里有一个容易误判的细节：在容器内，`127.0.0.1` 指的是容器自身，不是宿主机的 Clash、Verge 或其他代理。只有确实需要容器经宿主机代理出站时，才使用：

```text
http://host.docker.internal:7897
```

同时设置：

```text
NO_PROXY=localhost,127.0.0.1,::1,host.docker.internal
no_proxy=localhost,127.0.0.1,::1,host.docker.internal
```

如果代理只服务于浏览器，不要给网关容器强行注入代理变量。国际版出现 `EOF`、403 或超时时，应分别测试宿主机直连、宿主机代理和容器出站，再修改配置。

### 7.1 构建时的代理问题

如果 `docker build` 的日志出现：

```text
Get "https://proxy.golang.org/...": proxyconnect tcp: dial tcp 127.0.0.1:7897: connect: connection refused
```

说明构建环境把宿主机的 `127.0.0.1:7897` 注入到了容器。构建阶段必须使用 Docker 自己的网络，不能把宿主机代理地址带进去。临时排查可在当前 PowerShell 会话执行：

```powershell
$env:HTTP_PROXY=''
$env:HTTPS_PROXY=''
$env:http_proxy=''
$env:https_proxy=''
docker compose build --no-cache
```

如果本机 Docker Desktop 的代理设置本身不正确，应在 Docker Desktop 的 Resources / Proxies 中修正，而不是在项目 Dockerfile 里写死宿主机地址。构建机需要能访问 Go 模块代理和 Alpine 软件源；公司网络或受限环境可配置正确的内部镜像源。

## 8. 常见状态解释

`/status` 中的 `healthy` 表示账号没有被禁用、没有处于冷却、熔断或降权期。它不是一次实时上游探测，因此可能出现“状态显示正常，但某次上游请求刚好遇到网络 EOF”的情况。

判断真实可用性请看：

- `docker compose logs` 中最近的 `chat_stream` 结果；
- `GET /v1/stats` 的成功、失败和最近请求统计；
- 用 `/v1/chat/completions` 或 `/v1/responses` 发起真实请求。

## 9. 安全检查清单

- 修改默认 API 密钥和面板口令。
- 不公开暴露 `auths/`、`data/` 或 Docker socket。
- 管理页面不挂载 `/var/run/docker.sock` 时仍可监控，只是不能一键重启网关。
- 升级前确认备份可读，尤其是 `auths/` 和 `data/metrics.json`。
- 不要把真实账号、令牌、邮箱、内网路径或本机代理地址提交到 Git。
