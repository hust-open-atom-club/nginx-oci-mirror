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

工具保留 manifest 原始字节，递归下载所有平台、嵌套 index，以及 index 引用的附加 manifest；配置和压缩层均校验摘要与大小。再次执行会校验并复用同一 repository 下的已有 blob，发现损坏时重新下载。tag 在其引用的全部内容下载成功后才发布。

私有源 Registry 使用已有的 Docker 登录凭据及 credential helper，例如先执行 `docker login ghcr.io`。工具不会把源 Registry 的凭据写入输出目录。

## 从配置文件批量下载

创建 `images.txt`，每行一个镜像，顺序即下载顺序：

```text
alpine:latest
busybox:latest
ghcr.io/owner/image:v1
```

```sh
./tool --image-file images.txt ./mirror
./tool --image-file images.txt --nginx-config ./nginx/oci-registry.conf ./mirror
```

`--image-file` 模式只接收一个目标目录参数，不能再指定单个镜像。支持 LF / CRLF，忽略行首尾空白、空行及整行 `#` 注释；镜像名称、tag 和 digest 的规则与单镜像模式相同。

工具先校验整个文件，无效镜像会报告文件名和行号，空文件也会报错。校验通过后按列表顺序处理镜像。每个 manifest、配置或层文件下载失败后自动重试 **3 次**，加上首次尝试共 **4 次**，包括响应体中断、摘要或大小不符、无效 manifest 等情况；重试间隔依次为 100、200、400 毫秒。日志标明文件、尝试次数及失败原因，取消操作会立即停止重试。

达到重试上限后跳过失败文件，继续其他独立文件、index 的其他分支以及列表中的后续镜像。只有依赖完整的 manifest 才会发布，失败的父 index / tag 不会发布或替换已有有效版本；已完成的分支仍可按 digest 使用。存在任何下载失败时，处理完后返回非零退出状态，方便脚本发现不完整的同步。

若同时启用 `--nginx` 或 `--nginx-config`，即使本次同步部分或全部失败，也会从目标目录中的全部有效镜像文件重新生成配置。因此此前下载成功、但本次源站不可用的镜像仍会包含在配置中。配置生成失败时保留原配置。

## 并行下载文件

```sh
./tool --parallel-downloads 4 alpine:latest ./mirror
./tool --parallel-downloads 4 --image-file images.txt --nginx ./mirror
```

`--parallel-downloads` 控制同时下载的 blob 文件数（配置和压缩层），默认 `1`，允许 `1` 到 `128` 的整数；无效参数在访问源站前报错。manifest 树先顺序获取，再对所有平台及嵌套 index 的 blob 使用同一个有界工作池；镜像列表本身仍按顺序处理。相同 digest 和预期大小在一个镜像树中只安排一次下载，避免多个 worker 重复写入同一文件。所有下载先写临时文件，校验通过后才原子替换，失败不会留下半文件或覆盖原有有效内容。

## 生成 Nginx 配置模块

```sh
./tool --nginx alpine:latest ./mirror
./tool --nginx-config ./nginx/oci-registry.conf alpine:latest ./mirror
```

默认只下载文件，不生成配置。`--nginx` 保存到镜像目录下的 `nginx.conf`；`--nginx-config PATH` 单独指定保存位置，并自动启用配置生成。两个 flag 都要放在镜像参数前面，同时指定时使用 `--nginx-config` 的路径；缺失的父目录会自动创建。

生成的是 `server` 内的 include 模块，内容为 `location ^~ /v2/ { ... }`。将它加入已有 Nginx 配置中的 `server {}`，例如：

```nginx
server {
    listen 8080;
    server_name registry.example.org;

    include /absolute/path/to/nginx/oci-registry.conf;
}
```

include 路径应替换为实际配置文件的绝对路径。同一 `server` 中包含一次即可；不要将模块直接 include 到 `http {}`，也不要用 `nginx -c` 把它当作主配置启动。原先工具生成的完整配置也需要按上述方式迁移。

模块仅接管 `/v2/`，提供 Registry 探测、正确的 manifest Content-Type、Docker-Content-Digest、GET / HEAD 和 blob Range 请求。已导入资源的写入请求返回 405，未知 Registry 路径返回 404；其他业务路径继续由宿主配置处理。模块没有监听端口、日志、进程、临时目录或 `http` / `server` 外层配置；端口、TLS、鉴权等均由宿主配置控制。Nginx 工作进程需要能读取镜像目录并遍历其父目录。

在主配置中加入 include 后，检查并 reload 现有 Nginx：

```sh
nginx -t
nginx -s reload
docker pull localhost:8080/library/alpine:latest
```

普通 `root` 静态服务缺少 Registry 所需的响应头，因此应使用生成的配置。这里导出的是 HTTP URL 对应的文件布局，不是 Docker Distribution 后端的内部存储目录，也不是 `oci-layout`。

远程 Docker 客户端默认要求 HTTPS。对外使用时应在宿主配置中启用 TLS，或配置 TLS 反向代理；如果使用远程 HTTP 地址，需要在客户端 Docker daemon 中显式配置对应的 `insecure-registries`。参考 [Docker Registry 配置文档](https://docs.docker.com/reference/cli/dockerd/#insecure-registries)。模块本身不配置鉴权，访问权限由宿主配置决定。

## 目录与更新

下载 `alpine:latest` 后，主要目录如下：

```text
mirror/
├── nginx.conf                 # --nginx 的默认模块路径，可单独指定
└── v2/library/alpine/
    ├── manifests/
    │   ├── latest
    │   ├── sha256:<digest>    # index / 各平台 manifest
    │   └── .metadata.json    # 生成响应头需要的摘要与 media type
    └── blobs/
        └── sha256:<digest>    # 配置、压缩层及引用的数据
```

源 Registry 的域名不会加入输出 repository 路径：`alpine` 对应 `library/alpine`，`ghcr.io/owner/image` 对应 `owner/image`。不同源 Registry 的相同 repository 和 tag 共用一个路径，后导入的 tag 会覆盖先前的 tag。

可以向同一目录逐次导入多个镜像或 tag。每次启用配置生成时，都会扫描镜像目录中的全部 manifest 和 tag 文件，并校验它们的 digest、大小及递归引用的全部子 manifest / blob；只为完整有效的镜像重新生成模块；更新时继续使用相同的配置输出路径：

```sh
./tool --nginx-config ./nginx/oci-registry.conf busybox:latest ./mirror
nginx -t
nginx -s reload
docker pull localhost:8080/library/busybox:latest
```

生成配置中的 tag 指向对应 digest 文件，因此运行中的 Nginx 在 reload 前继续提供原先的 tag 内容，reload 后提供更新后的内容。按 digest 拉取同样可用。

不同 repository 仍各自保存 blob：相同 digest 在同一 repository 的多个 tag / manifest 间复用，但不同 repository 之间不会自动共享物理文件，也不会避免再次下载。测试通过实际 HTTP 请求计数和 inode 比较验证此行为；本工具目前不提供跨 repository 去重。

不同进程向同一目录导入仍需要串行执行。建议保留 `.metadata.json` 以兼容省略 media type 的旧 manifest；配置生成以磁盘文件为准，元数据缺失或损坏时仍会检查有效文件。损坏、不完整、临时或符号链接支持的 manifest 图会被记录并跳过，不影响其他有效镜像。此校验是生成配置时的快照，运行期间不要从外部修改文件；它不代替 Nginx 的文件访问权限控制。移动镜像目录后需要修改模块中的 `root`，或在新位置重新执行并启用配置生成。镜像目录的路径不能包含 `$` 或换行符。工具会拒绝覆盖非本工具生成的配置文件；本工具生成的模块会被重写，手工添加的指令应放在宿主配置中。

## 范围与验证

支持 OCI image manifest / image index 和 Docker schema 2 manifest / manifest list。仅提供镜像拉取，不提供 push、删除、catalog、tags/list 或 referrers API。没有下载被导出 index 引用之外的签名、referrer 或其他 tag；源 Registry 不提供的外部 / foreign blob 会导致相关 manifest / tag 跳过并报告同步失败。旧 Docker schema 1 manifest 不受支持。

Linux 下运行测试：

```sh
go test -race ./...
go fix ./...
modernize ./...
staticcheck ./...
deadcode ./...
```

测试使用本地 HTTP Registry，覆盖 Docker / OCI 的嵌套 index、按 digest 导入、多 tag 保留、批量顺序与失败后继续、四次 HTTP 尝试、文件预校验、并发上限与真实重叠、共享 digest 的并发安全、取消操作、默认和指定路径的模块生成、完整磁盘图校验、旧镜像保留、缓存复用及跨 repository 的实际请求 / inode 行为、损坏缓存修复和失败时保护已有文件。

指定实际 Nginx 二进制可以额外验证模块在已有 `server` 中的行为：

```sh
NGINX_TEST_BINARY="$(command -v nginx)" go test -race ./...
```

此测试使用临时宿主配置和动态本地端口，覆盖 Content-Type、摘要、完整 blob、HEAD、Range、写入拒绝，以及模块对既有业务、MIME 类型、gzip 和正则 location 的兼容性。

已使用真实 Nginx 1.24.0 验证上述 include 场景。私有 Registry 登录及 TLS 部署未做实机测试。
