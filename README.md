# 丛云图床

丛云图床是一套使用 Go 编写的轻量化、多用户图片托管系统。服务端以单个二进制运行，前端资源直接内嵌；图片上传后自动转换为 WebP，并支持本机、S3 兼容对象存储与 WebDAV。

## 功能

- 多用户、管理员角色及按用户配置的单图大小、文件类型、总空间配额
- 拖拽、批量选择、剪贴板粘贴和 URL 远程上传
- `cwebp` 多线程压缩，可在后台调整质量（1–100）
- 公开图片广场和带随机访问令牌的隐私图片
- 上传完成自动复制 Markdown，支持随时复制外链或 Markdown
- 站点名称、域名、背景图、背景透明度/对齐、主题色、注册、游客上传与过期时间配置
- 本机、S3 兼容存储、WebDAV；后台展示已用、总量和可用容量
- 响应式毛玻璃界面、浅色/深色模式、手机和平板适配
- SQLite WAL、密码 bcrypt、HttpOnly 会话、同源写请求校验和远程上传 SSRF 防护
- GitHub Actions 自动构建 `linux/amd64` 与 `linux/arm64` 镜像

## Docker Compose 部署

```bash
git clone https://github.com/kmh1145/congyu-image.git
cd congyu-image
```

所有部署配置均已合并在 `docker-compose.yml` 中，不需要创建 `.env` 文件。首次部署前请编辑 `environment` 部分：

- 将 `ADMIN_PASSWORD` 改为强密码。
- 将 `BASE_URL` 改为实际访问地址，例如 `https://img.example.com`。
- 使用 HTTPS 域名时将 `COOKIE_SECURE` 改为 `"true"`；仅在 HTTP 环境中保留 `"false"`。

```bash
docker compose up -d
```

访问 `http://服务器地址:8080`。数据默认保存在 Docker 命名卷 `congyu-image-data` 中，命名卷会使用镜像内预设的正确权限，避免非 root 进程因宿主机目录权限而无法启动。首次启动会按照 Compose 中的配置创建管理员。

### Compose 配置

| 变量 | Compose 初始值 | 说明 |
| --- | --- | --- |
| `LISTEN_ADDR` | `:8080` | 服务监听地址 |
| `DATA_DIR` | `/data` | SQLite 与本机图片目录 |
| `BASE_URL` | `http://localhost:8080` | 生成外链的基础地址；后台“网站域名”优先级更高 |
| `ADMIN_USERNAME` | `admin` | 首次启动创建的管理员用户名 |
| `ADMIN_PASSWORD` | `change-this-password` | 首次启动管理员密码，部署前必须修改 |
| `COOKIE_SECURE` | `false` | HTTPS 部署时应设为 `true` |
| `TZ` | `Asia/Shanghai` | 容器时区 |

推荐在 Caddy、Nginx 或 Traefik 后运行并启用 HTTPS。反向代理需要保留 `Host`，并传递 `X-Forwarded-Proto`。

### 使用宿主机目录（可选）

默认命名卷最省心。如果需要将数据直接保存在当前目录，请先创建目录并赋予容器用户（UID/GID `10001`）访问权限：

```bash
sudo install -d -m 0750 -o 10001 -g 10001 ./data
```

然后将 `docker-compose.yml` 中的卷改为：

```yaml
volumes:
  - ./data:/data
```

如果旧版部署出现 `mkdir /data/images: permission denied`，可更新仓库并重新创建容器，默认配置会自动改用命名卷：

```bash
git pull
docker compose pull
docker compose up -d --force-recreate
```

## 存储配置

首次启动自动添加“本机存储”。管理员可以在“管理后台 → 存储管理”中添加：

- **S3 兼容存储**：Endpoint、Access Key、Secret Key、Bucket、Region、路径前缀、是否使用 HTTPS。
- **WebDAV**：服务地址、用户名、密码、路径前缀。

S3 与 WebDAV 通常不提供账户总容量接口，因此可填写“展示容量”；后台会将系统内图片用量与该容量进行对比。填 `0` 时仅显示已用容量。本机存储直接读取文件系统容量。

存储密钥写入 SQLite，仅管理员可配置；接口返回时会遮蔽 Secret Key 与密码。生产环境请同时做好 `/data` 卷的访问控制与备份。

## 本地开发

需要 Go 1.24+、`cwebp` 和支持 SQLite 文件写入的目录。

```bash
go mod download
ADMIN_PASSWORD=dev-password go run .
```

运行测试：

```bash
go test ./...
go vet ./...
```

## 镜像发布

推送到默认分支或 `v*` 标签会触发 GitHub Actions，构建并推送多架构镜像：

```text
ghcr.io/kmh1145/congyu-image:latest
```

仓库需要在 GitHub 的 Packages 设置中允许 Actions 写入包。工作流使用仓库内置的 `GITHUB_TOKEN`，无需额外密钥。

## 备份与升级

默认命名卷可用以下命令备份数据库和本机图片：

```bash
docker run --rm -v congyu-image-data:/data -v "$PWD:/backup" alpine \
  tar czf /backup/congyu-image-backup.tar.gz -C /data .
```

使用宿主机目录挂载时，停止容器后备份整个 `data/` 目录即可。升级前建议先备份，然后执行：

```bash
docker compose pull
docker compose up -d
```

数据库表会在启动时自动迁移。删除图片会同步删除存储对象，且不可恢复。

## License

[MIT](LICENSE)
