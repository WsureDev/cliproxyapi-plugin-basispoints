# cliproxyapi-plugin-basispoints

把 `gpt-6-astra` 和 `gpt-5.6-sol` 转到 Basis Points。客户端仍按原来的 Codex 方式请求，凭据用 CLIProxyAPI 里已有的 Codex 账号。

## 第一次用 Docker Compose

插件文件在容器里的 `/CLIProxyAPI/plugins`。这个目录、配置、账号和日志都要挂到宿主机，否则容器重建后插件和配置会丢。

目录：

```text
.
├── docker-compose.yml
└── cpa/
    ├── config.yaml
    ├── auths/
    ├── logs/
    └── plugins/
        └── linux/amd64/basispoints.so
```

`docker-compose.yml` 里给 `cli-proxy-api` 加上这四条挂载：

```yaml
services:
  cli-proxy-api:
    image: eceasy/cli-proxy-api:latest
    container_name: cli-proxy-api
    restart: unless-stopped
    ports:
      - "8317:8317"
      - "1455:1455"
    volumes:
      - ./cpa/config.yaml:/CLIProxyAPI/config.yaml
      - ./cpa/auths:/root/.cli-proxy-api
      - ./cpa/logs:/CLIProxyAPI/logs
      - ./cpa/plugins:/CLIProxyAPI/plugins
```

`./cpa/auths` 对应容器内的 `/root/.cli-proxy-api`，Codex 登录文件放这里。`config.yaml` 里的插件目录保持相对路径：

```yaml
auth-dir: /root/.cli-proxy-api
plugins:
  enabled: true
  dir: plugins
```

## 编译

用 Debian 12 的 Go 镜像编译，和 `eceasy/cli-proxy-api` 的 glibc 一致。在本机直接编出来的 `.so` 放进容器会加载失败。

在本仓库根目录执行：

```bash
docker run --rm \
  -v "$PWD":/src \
  -w /src \
  -e CGO_ENABLED=1 \
  -e PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  golang:1.26-bookworm \
  go build -buildvcs=false -buildmode=c-shared -o basispoints.so .
```

产物是当前目录的 `basispoints.so`。

## 安装

把 `.so` 放到挂载目录里的平台子目录，然后重启。Linux amd64 是：

```bash
install -D -m 644 basispoints.so /path/to/compose/cpa/plugins/linux/amd64/basispoints.so
docker restart cli-proxy-api
```

容器内的实际路径是 `/CLIProxyAPI/plugins/linux/amd64/basispoints.so`。启动日志里应有：

```text
plugin registered plugin_id=basispoints plugin_name=basispoints version=0.3.0
```

## YAML 配置

写在 `config.yaml` 的 `plugins.configs.basispoints` 下。改完保存，重启容器，或在管理界面里重新加载。管理界面的 **插件 → 插件管理 → basispoints → 编辑配置** 改的是同一份字段。

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    basispoints:
      enabled: true
      priority: 100
      base_url: https://bps.openai.com/basispoints/api
      auth_mode: chatgpt
      auth_provider: codex
      models:
        - gpt-6-astra
        - gpt-5.6-sol
      max_effort: xhigh
      max_in_flight_per_account: 0
      min_interval_ms: 0
      cooldown_ms: 0
      image_upload: false
      image_ttl_seconds: 1800
      image_s3_endpoint: ""
      image_s3_region: auto
      image_s3_bucket: ""
      image_s3_access_key_id: ""
      image_s3_secret_access_key: ""
      image_s3_prefix: bps
```

| 字段 | 作用 |
|---|---|
| `enabled` | 这个插件是否接管请求。`plugins.enabled` 也必须是 `true` |
| `priority` | 多个插件同时匹配时的优先级，默认 `100` |
| `base_url` | Basis Points 地址，末尾不要加 `/responses` |
| `auth_mode` | 固定 `chatgpt`，使用 Codex OAuth |
| `auth_provider` | 从哪种账号里取令牌，固定 `codex` |
| `models` | 要转发的模型。省略时默认就是上面两个。写成 `[]` 则一个都不转发 |
| `max_effort` | 上游推理强度上限。`max` / `ultra` 会先变成 `xhigh`，再被这个值封顶 |
| `max_in_flight_per_account` | 每个账号同时打上游的请求数。保持 `0`，表示不在本地限流 |
| `min_interval_ms` | 同一账号两次请求的最小间隔。保持 `0` |
| `cooldown_ms` | 上游 429 之后本地冷却多久。保持 `0`，否则重试会一直收到本地 429 |
| `image_upload` | 是否把 base64 图片上传到 S3。关闭时这类请求直接拒绝 |
| `image_ttl_seconds` | 图片保留时间，默认 1800 秒 |
| `image_s3_endpoint` | S3 地址，例如 `https://<ACCOUNT_ID>.r2.cloudflarestorage.com` |
| `image_s3_region` | R2 填 `auto` |
| `image_s3_bucket` | 桶名。只写名字，不要写整段网址 |
| `image_s3_access_key_id` | R2 的 32 位 Access Key ID |
| `image_s3_secret_access_key` | 对应的 64 位 Secret |
| `image_s3_prefix` | 对象前缀，默认 `bps` |

模型名后面的 `(max)` 之类后缀会先去掉再和 `models` 比较。

## 打开图片转发

把上面的图片相关字段改成实际的 S3 信息，例如 Cloudflare R2：

```yaml
image_upload: true
image_ttl_seconds: 1800
image_s3_endpoint: https://<ACCOUNT_ID>.r2.cloudflarestorage.com
image_s3_region: auto
image_s3_bucket: image
image_s3_access_key_id: <32 位 Access Key ID>
image_s3_secret_access_key: <64 位 Secret Access Key>
image_s3_prefix: bps
```

Access Key ID 是 R2 令牌页上的 32 位 ID。以 `cfut_` 开头的 Cloudflare 用户 API Token 不能填在这里。桶保持私有。图片到期后由插件删除。给 `bps/` 再加一条一天后删除的生命周期规则，避免进程重启留下残留文件。

S3 上传走 CLIProxyAPI 的 `proxy-url`。容器里经常解析不了 `r2.cloudflarestorage.com`，不走这个代理时上传会失败。

开关打开但地址、桶或密钥没填全时，带 base64 图片的请求会失败，不会把图片原文送到上游。
