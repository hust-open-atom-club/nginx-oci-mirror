# 本地扫描性能与校验边界

## 为什么下载完成后还需要等待

调用链为 `run` → `nginxConfig` → `localManifests`。`Saved ...` 在下载结束后输出；启用 Nginx 配置生成时，随后从整个目标目录寻找有效镜像，并校验它们的完整依赖图。原版 `3b3c983` 已在同一 repository 内复用成功的 blob 校验结果，但所有文件仍串行哈希，也没有进度输出。下载时的完整性校验和这里的磁盘校验是两个步骤。

新版先解析 manifest 图，再用所有仓库共用的有界 worker 池校验独立 blob，最后根据校验结果筛选有效的 manifest / tag，并原子写入配置。默认 2 个 worker，可用 `--parallel-verify 1` 降低磁盘压力，或在 SSD / NVMe 上尝试 4 个。允许范围为 1–32；下载并行度保持独立。

## 同字节基准

环境：Linux amd64，Go 1.27.2，AMD EPYC 9V74，Go 可用并行度 9。临时文件写入 overlay 文件系统，基准循环复用文件，数据大部分位于页缓存。以下是每项 3 组、每组 5 次扫描的组均值中位数；不包含构造数据的时间，不开启 race instrumentation，没有同时运行其他基准或编译。

| 数据集 | 原版串行 | 新版 1 worker | 新版 2 workers（默认） | 新版 4 workers |
| --- | ---: | ---: | ---: | ---: |
| 64 个不同 manifest、256 个 tag，共享 32 个 16 MiB 层 | 395.1 ms | 403.1 ms | 212.9 ms | 124.1 ms |
| 32 个 manifest、32 个 tag，128 个不同 4 MiB 层 | 382.2 ms | 405.0 ms | 195.5 ms | 100.3 ms |

默认并行度分别约为原版的 **1.86× / 1.95×**；4 worker 约 **3.18× / 3.81×**。单 worker 有约 2–6% 的图调度、取消检查和计数开销。这是暖缓存下读取 / 哈希吞吐的示例，不是实际机器或冷盘读取的速度保证。

每轮实际 blob 读入字节在原版和所有新版配置中均为 **536,870,914 字节**（512 MiB 层数据 + 2 字节配置）。共享 tag 数据集均为 **33 次** blob 校验，多 blob 数据集均为 **129 次**。计数来自实际 reader；不含较小的 manifest / metadata 读取。相同 tag / 层不会重复读取，完整性检查没有被省略。

基准 fixture 也断言每次扫描的有效引用数、实际字节数和 blob 校验次数。原版适配仅修改测试入口并包装 `validContent` 的 reader 计数，保持原来的扫描与验证逻辑。

## 复现

在本分支 checkout 中执行（需要 Go、Git、Python 3；约需 1 GiB 空闲空间）：

```sh
mkdir -p /path/on/disk/scan-bench-tmp
TMPDIR=/path/on/disk/scan-bench-tmp sh scripts/benchmark-local-scan.sh
```

脚本从 `3b3c9838bb5ebd80f5086f0e4d97bfb07a77867c` 建立临时 detached worktree，使用相同 fixture 顺序运行原版及新版，然后清理临时 worktree。输出每组扫描时间、MB/s、实际 `blob_bytes/op` / `blob_checks/op`。不需要源 Registry、Docker 或 Nginx 服务。

只运行新版：

```sh
go test -run '^$' -bench BenchmarkLocalInventory -benchtime=5x -count=3 ./...
```

不要把 `/tmp` 一定视为物理磁盘，它可能是 tmpfs；明确设置 `TMPDIR`。测试不会清空系统页缓存，不修改系统 / 存储设置。衡量实际生产盘时，应使用其代表性数据量与负载，分别比较 1、2、4 个 worker。

## 正确性与剩余成本

- 每次扫描都从磁盘重新哈希有效文件，包括本轮刚下载的文件；没有持久缓存，也不依赖 mtime 信任文件。相同大小、相同 mtime 的损坏也会被检测。
- 去重键包含物理路径（含 repository）、摘要和预期大小；错误的描述符或另一仓库的损坏不会污染有效兄弟。成功和失败结果都只在当前扫描中复用。
- 文件大小已不匹配时直接判为无效，不再读取整份文件；相同大小的损坏仍必须完整哈希后识别。
- manifest 的摘要 / 大小 / media type、递归子节点、sha512 旧元数据提示、符号链接限制与完整图发布规则继续生效。worker 全部退出后才决定可发布的引用，输出顺序仍稳定。
- Ctrl+C / SIGTERM 停止调度，并在读取块之间检查取消；原子替换配置前再次检查取消，已取消的扫描不会用部分结果覆盖旧配置。已经阻塞的内核读取不能被 Go context 强行中断。
- 校验与 Nginx 服务仍是一次文件快照，不支持其他进程同时修改同一目录，也不防止校验完成后外部再次改写。无需、也没有引入新的并发写入假设。
- 有效数据的总读取字节数不变。单个超大 blob 的 SHA 校验不能通过文件级并行提速；机械硬盘、共享存储或冷缓存的瓶颈可能是 I/O。默认并行度克制，必要时使用 1 个 worker。
