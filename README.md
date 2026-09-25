# cliproxyapi-plugin-basispoints

CLIProxyAPI 插件。把名单里的 Codex 模型转到 Excel / Basis Points（`https://bps.openai.com/basispoints/api/responses`），使用宿主机里已有的 Codex OAuth 凭据。

协议转换来自 sub2api 的 Basis Points 适配。出处见 [basispoints/NOTICE.md](basispoints/NOTICE.md)。

## 行为

- 只转发配置名单中的模型。名单按别名、`codex/` 前缀和 `(max)` 这类 effort 后缀解开后再做精确匹配。省略 `models` 时默认是 `gpt-6-astra` 和 `gpt-5.6-sol`。显式空列表不接管任何模型。
- 请求体会重写成 BPS 白名单：保留原模型名，附带 `model_selection: explicit`、`reasoning_effort`、工具目录和改写后的 `prompt_cache_key`。Codex 的 `additional_tools`、`client_metadata`、`include`、`reasoning.context` 不会原样上传。
- `max` / `ultra` 先归一成 `xhigh`，再按 `max_effort` 封顶。`none` / `minimal` 变成 `low`。
- 客户端工具通过一次 `run_officejs` 传递。响应里的原生工具调用会还原成原来的 function / custom 工具。回放缓存在当前进程内，按账号和线程隔离。
- `image_generation`，以及带外部访问的 `web_search`，留在原来的 Codex 通道。
- `previous_response_id` 会拒绝。BPS 需要客户端带上完整历史。

## 编译和安装

在与 CLIProxyAPI 容器相同的 glibc 上编译。官方镜像是 Debian 12：

```bash
go build -buildmode=c-shared -o basispoints.so .
```

把产物放到插件目录：

```text
plugins/linux/amd64/basispoints.so
```

容器内路径是 `/CLIProxyAPI/plugins/linux/amd64/basispoints.so`。替换文件后重启 CLIProxyAPI，日志里应出现 `plugin registered ... version=0.3.0`。

## 配置

写在 `plugins.configs.basispoints` 下。缺省时插件会使用下表中的默认值；管理界面只显示写进配置文件的字段。

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

`max_in_flight_per_account` 和 `cooldown_ms` 默认是 0。本地不再因为单账号并发或冷却直接回 429。上游返回的 429 会原样交给客户端。

## 图片

`image_upload: false` 时，`data:image/...;base64` 会被拒绝。已经是 HTTPS 的图片地址原样转发。

打开后，PNG、JPEG、GIF、WebP 会上传到私有 S3 桶。Basis Points 收到的是一张在 `image_ttl_seconds` 内有效的签名链接，默认 30 分钟。到期后插件删除对象。同一个进程里、同一张图在到期前会复用链接。

R2 示例：

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

`image_s3_bucket` 只填桶名。`image_s3_access_key_id` 是 R2 的 S3 Access Key ID，固定 32 位。以 `cfut_` 开头的 Cloudflare 用户 API Token 不能填在这里，R2 会返回 `Credential access key has length 53, should be 32`。

插件进程重启后，内存里尚未执行的删除任务会丢失。给 `bps/` 前缀加一条一天后删除的生命周期规则，可以清掉这些残留对象。

开关打开但 S3 字段没填全时，带图片的请求返回 `basispoints_image_unconfigured`，不会把 base64 原样送往上游。
