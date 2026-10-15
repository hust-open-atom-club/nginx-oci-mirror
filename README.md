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

运行结束时，标准输出的最后一行为 `Total size is 1.23G`。小于 1024 字节时只输出整数，例如 `Total size is 0`；其余使用以 1024 为进位的 `K` / `M` / `G` / `T` / `P`，四舍五入到最多两位小数，不附加 `B` 或 `iB`。

这里统计目标目录 `v2/` 下保留的普通文件的逻辑字节总数，包含 blob、manifest、tag 文件和 `.metadata.json`，不等同于文件系统分配的磁盘块大小或本次下载量。以前保留的文件以及未引用或损坏的文件也计入；每条文件路径只计一次，共享 blob 不因多个 tag 引用而重复计数，不同 repository 的独立副本分别计数，tag 文件自身的副本也计入。不包含 `v2/` 之外的配置等文件、符号链接及其目标、`.download-*` 临时文件或目录。

无论是否生成 Nginx 配置，最后都只通过目录遍历和文件属性统计大小，不重新读取或哈希文件内容。部分或全部下载失败时，只要统计成功，仍输出当前保留的总量，同时保持非零退出状态；新目录没有数据时输出 `0`。参数 / 镜像列表校验失败、用户取消或大小统计出错时不输出总量，避免把未完成的统计当成完整结果。与同步和校验一样，统计期间不要让其他进程改动镜像目录。

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

## 下载后本地校验的进度与并行度

启用 `--nginx` / `--nginx-config` 时，`Saved ...` 只表示当前镜像下载完成；接下来还会清点本地全部 manifest / tag，并从磁盘完整读取其引用的 blob，校验摘要和大小。镜像目录越大，这一步越久。没有启用配置生成时，不会执行这次全目录校验。

```sh
./tool --nginx --parallel-verify 4 --image-file images.txt ./mirror
```

`--parallel-verify` 独立控制本地 blob 校验并行度，默认 `2`，允许 `1` 到 `32`；不改变 `--parallel-downloads`（默认仍为 `1`）。所有仓库、平台和 manifest 共用一个有界工作池，不会为每个镜像单独开一组 worker。机械硬盘或繁忙的共享存储建议设为 `1`，SSD / NVMe 可以试 `2` 或 `4`；继续增大不一定更快，单个超大 blob 的摘要仍然顺序计算。

开始扫描时会输出提示，每 2 秒显示已检查 / 已发现的 blob 数和实际读入的 MiB，结束时报告耗时；发现阶段的总数可能继续增加。Ctrl+C / SIGTERM 会停止调度，在正在执行的读取之间检查取消，并等待 worker 退出，保留旧的 Nginx 配置。操作系统中已经阻塞的磁盘读取仍需要先返回。

每次执行仍会完整校验本地文件，包括刚下载的内容；不会持久保存或依据 mtime 跳过摘要校验。同一仓库内的相同 digest 和预期大小每轮只检查一次（失败结果也复用）；不同仓库的物理文件分别检查。大小已经不符的文件会直接判为无效，无需读完整个文件。生成配置仍只包含完整有效的依赖图，不能与其他写入进程同时使用同一镜像目录。

基准测试和可复现命令见 [本地扫描性能测试](docs/local-scan-performance.md)。加速来自有界并行读取 / 哈希，不会减少有效 blob 必须读取的总字节数；冷缓存、机械硬盘或存储带宽瓶颈下的收益需在实际机器上测量。

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

测试使用本地 HTTP Registry，覆盖 Docker / OCI 的嵌套 index、按 digest 导入、多 tag 保留、批量顺序与失败后继续、四次 HTTP 尝试、文件预校验、并发上限与真实重叠、共享 digest 的并发安全、取消操作、默认和指定路径的模块生成、完整磁盘图校验、本地校验并发上限与跨 tag 去重、同大小 / 同 mtime 损坏检测、扫描取消与配置保护、旧镜像保留、缓存复用及跨 repository 的实际请求 / inode 行为、损坏缓存修复和失败时保护已有文件。

指定实际 Nginx 二进制可以额外验证模块在已有 `server` 中的行为：

```sh
NGINX_TEST_BINARY="$(command -v nginx)" go test -race ./...
```

此测试使用临时宿主配置和动态本地端口，覆盖 Content-Type、摘要、完整 blob、HEAD、Range、写入拒绝，以及模块对既有业务、MIME 类型、gzip 和正则 location 的兼容性。

已使用真实 Nginx 1.24.0 验证上述 include 场景。私有 Registry 登录及 TLS 部署未做实机测试。
