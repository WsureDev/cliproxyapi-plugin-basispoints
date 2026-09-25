# cliproxyapi-plugin-basispoints

把 `gpt-6-astra` 和 `gpt-5.6-sol` 转到 Basis Points。客户端仍按原来的 Codex 方式请求，凭据用 CLIProxyAPI 里已有的 Codex 账号。

## 编译

用 Debian 12 的 Go 镜像编译，和 `eceasy/cli-proxy-api` 的 glibc 一致。在本机直接编出来的 `.so` 放进容器会加载失败。

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

复制到 CLIProxyAPI 的插件目录后重启。

```bash
install -D -m 644 basispoints.so plugins/linux/amd64/basispoints.so
docker restart cli-proxy-api
```

容器内路径是 `/CLIProxyAPI/plugins/linux/amd64/basispoints.so`。启动日志里应有：

```text
plugin registered plugin_id=basispoints plugin_name=basispoints version=0.3.0
```

## 使用

在 `config.yaml` 里打开插件。下面这些值不写也会生效，写出来是为了在管理界面里看得到。

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
```

重启后再请求这两个模型，就会走 Basis Points。模型名后面的 `(max)` 之类后缀会先去掉再匹配。`models` 写成空列表时，这个插件不接管任何请求。

`max_in_flight_per_account` 和 `cooldown_ms` 保持 `0`。不要设成 `1`，否则一个长请求没结束时，后续重试会一直收到本地 429。

## 图片

默认不转发 base64 图片，这种请求会被拒绝。HTTPS 图片地址原样通过。

要转发时，用 R2 的 S3 密钥，并保持桶是私有的：

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

`image_s3_bucket` 只填桶名。Access Key ID 是 R2 令牌页上的 32 位 ID，不是以 `cfut_` 开头的用户 API Token。图片保留 30 分钟，到期后删除。给 `bps/` 加一条一天后删除的生命周期规则，避免进程重启留下残留文件。
