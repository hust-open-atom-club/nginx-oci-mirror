# nginx-oci-mirror

用 Go 将远程 OCI / Docker 镜像下载成 [Registry HTTP API V2](https://github.com/opencontainers/distribution-spec/blob/main/spec.md) 的静态文件目录，再通过 Nginx 提供只读镜像拉取服务。无需运行 Registry 后端。

运行环境为 Linux，编译需要 Go 1.27 或更新版本。

## 编译与下载

```sh
go build -o tool .
./tool alpine:latest .
```

省略 tag 时默认为 `latest`。`lates` 不会自动纠正，它会作为实际 tag 向源 Registry 请求。

支持短名称、完整 Registry 地址和 digest：

```sh
./tool alpine ./mirror
./tool ghcr.io/owner/image:v1 ./mirror
./tool alpine@sha256:<实际的完整摘要> ./mirror
```

工具保留 manifest 原始字节，递归下载所有平台、嵌套 index，以及 index 引用的附加 manifest；配置和压缩层均校验摘要与大小。再次执行会校验并复用已有 blob，发现损坏时重新下载。tag 在全部内容下载成功后才发布。

私有源 Registry 使用已有的 Docker 登录凭据及 credential helper，例如先执行 `docker login ghcr.io`。工具不会把源 Registry 的凭据写入输出目录。

## 生成配置并提供服务

```sh
./tool --nginx alpine:latest ./mirror
nginx -t -c "$(realpath ./mirror/nginx.conf)"
nginx -c "$(realpath ./mirror/nginx.conf)"
docker pull localhost:8080/library/alpine:latest
```

`--nginx` 必须放在镜像参数前面。默认只下载文件，不生成配置。生成的 `nginx.conf` 是完整配置，默认监听 HTTP 8080 端口，提供 `/v2/` 探测、正确的 manifest Content-Type、Docker-Content-Digest、GET / HEAD 和 blob Range 请求。其他路径返回 404，写入请求返回 405。

Nginx 需要能读取目标目录并遍历其父目录；使用普通用户启动时，工作进程使用该用户的权限。配置的 PID 文件位于 `/tmp/nginx-oci-mirror.pid`，临时目录为 `/tmp/nginx-oci-mirror-*`，不会使用系统 Nginx 的 PID 文件。启动前确保 8080 端口空闲。

普通 `root` 静态服务缺少 Registry 所需的响应头，因此应使用生成的配置。这里导出的是 HTTP URL 对应的文件布局，不是 Docker Distribution 后端的内部存储目录，也不是 `oci-layout`。

远程 Docker 客户端默认要求 HTTPS。对外使用时可在此服务前配置 TLS 反向代理；如果使用远程 HTTP 地址，需要在客户端 Docker daemon 中显式配置对应的 `insecure-registries`。参考 [Docker Registry 配置文档](https://docs.docker.com/reference/cli/dockerd/#insecure-registries)。生成的服务没有鉴权，能访问服务的客户端就能拉取已导入的镜像。

## 目录与更新

下载 `alpine:latest` 后，主要目录如下：

```text
mirror/
├── nginx.conf                 # 仅 --nginx 时生成
└── v2/library/alpine/
    ├── manifests/
    │   ├── latest
    │   ├── sha256:<digest>    # index / 各平台 manifest
    │   └── .metadata.json    # 生成响应头需要的摘要与 media type
    └── blobs/
        └── sha256:<digest>    # 配置、压缩层及引用的数据
```

源 Registry 的域名不会加入输出 repository 路径：`alpine` 对应 `library/alpine`，`ghcr.io/owner/image` 对应 `owner/image`。不同源 Registry 的相同 repository 和 tag 共用一个路径，后导入的 tag 会覆盖先前的 tag。

可以向同一目录逐次导入多个镜像或 tag。每次 `--nginx` 都会为目录中已经成功导入的全部 manifest 重新生成配置：

```sh
./tool --nginx busybox:latest ./mirror
nginx -t -c "$(realpath ./mirror/nginx.conf)"
nginx -c "$(realpath ./mirror/nginx.conf)" -s reload
docker pull localhost:8080/library/busybox:latest
```

生成配置中的 tag 指向对应 digest 文件，因此运行中的 Nginx 在 reload 前继续提供原先的 tag 内容，reload 后提供更新后的内容。按 digest 拉取同样可用。

同一目录的导入需要串行执行。不要删除 `.metadata.json`。移动目录后需要修改配置中的 `root`，或在新位置重新执行带 `--nginx` 的导入。Nginx 配置的目标路径不能包含 `$` 或换行符。工具会拒绝覆盖非本工具生成的 `nginx.conf`；本工具生成的配置会被重写，包括手工修改的端口或其他指令。

## 范围与验证

支持 OCI image manifest / image index 和 Docker schema 2 manifest / manifest list。仅提供镜像拉取，不提供 push、删除、catalog、tags/list 或 referrers API。没有下载被导出 index 引用之外的签名、referrer 或其他 tag；源 Registry 不提供的外部 / foreign blob 会导致导入失败。旧 Docker schema 1 manifest 不受支持。

Linux 下运行测试：

```sh
go test -race ./...
go fix ./...
modernize ./...
staticcheck ./...
deadcode ./...
```

测试使用本地 HTTP Registry，覆盖 Docker / OCI 的嵌套 index、按 digest 导入、多 tag 保留、Nginx 配置生成、缓存复用、损坏缓存修复和失败时保护已有文件。

开发时使用真实 Nginx 1.24.0 和 Docker 29.7.1 验证了 Alpine 按 tag、按 digest 和 `linux/arm64` 平台拉取，以及 blob 的 206 Range 响应、配置文件路径的 404 和写入请求的 405。私有 Registry 登录及 TLS 部署未做实机测试。
