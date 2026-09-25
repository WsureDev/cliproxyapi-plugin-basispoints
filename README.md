# cliproxyapi-plugin-basispoints

把 `gpt-6-astra` 和 `gpt-5.6-sol` 转到 Basis Points。客户端仍按原来的 Codex 方式请求，凭据用 CLIProxyAPI 里已有的 Codex 账号。配置在管理界面里完成，不要手改 YAML。

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

## 在管理界面里启用

浏览器打开 CLIProxyAPI 管理面板，用管理密钥登录。

1. 左侧进入 **插件** → **插件管理**。
2. 确认页面上的全局状态是 **已启用**。如果是 **已停用**，插件实例即使打开也不会生效。
3. 在列表里找到 `basispoints`，点 **编辑配置**。
4. 在 **基础设置** 里打开 **启用**。优先级保持 `100` 即可。
5. 在 **配置字段** 里确认这些值，然后点 **保存**：

| 字段 | 填写 |
|---|---|
| `base_url` | `https://bps.openai.com/basispoints/api` |
| `auth_mode` | `chatgpt` |
| `auth_provider` | `codex` |
| `models` | `gpt-6-astra`、`gpt-5.6-sol`，用添加数组项逐个写入 |
| `max_effort` | `xhigh` |
| `max_in_flight_per_account` | `0` |
| `cooldown_ms` | `0` |

保存成功时页面提示 **插件配置已保存**。之后请求这两个模型就会走 Basis Points。

`models` 留空表示不接管任何请求。`max_in_flight_per_account` 不要填 `1`，否则一个长请求没结束时，后续重试会一直收到本地 429。

## 图片转发

默认关闭。需要转发 base64 图片时，仍在同一页的 **配置字段** 里填写，不要改配置文件：

| 字段 | 填写 |
|---|---|
| `image_upload` | 打开 |
| `image_ttl_seconds` | `1800` |
| `image_s3_endpoint` | `https://<ACCOUNT_ID>.r2.cloudflarestorage.com` |
| `image_s3_region` | `auto` |
| `image_s3_bucket` | 桶名，例如 `image`。不要填整段网址 |
| `image_s3_access_key_id` | R2 令牌页上的 32 位 Access Key ID |
| `image_s3_secret_access_key` | 对应的 64 位 Secret |
| `image_s3_prefix` | `bps` |

填完点 **保存**。以 `cfut_` 开头的 Cloudflare 用户 API Token 不能当作 Access Key ID。图片保留 30 分钟后删除。给桶里的 `bps/` 前缀加一条一天后删除的生命周期规则，避免进程重启留下残留文件。
