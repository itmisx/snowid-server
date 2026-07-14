# snowid-server

**中文** | [English](README.en.md)

一个 gRPC 服务，把 [`bwmarrin/snowflake`](https://github.com/bwmarrin/snowflake) 包了一层，
对外发放全局唯一、大致按时间有序的 64 位 ID。**它只做这一件事。**

不依赖任何外部组件——不需要 ZooKeeper，不需要 Redis，也不需要数据库。

```console
$ snowid-server --worker-id 0
INFO generator ready worker_id=0 epoch=2020-01-01T00:00:00Z worker_bits=10 step_bits=12 \
     max_workers=1024 max_ids_per_second=4096000 ids_valid_until=2089-09-06T15:47:35Z
INFO serving addr=:50051

$ grpcurl -plaintext -d '{"count": 3}' localhost:50051 snowid.v1.SnowId/Next
{ "ids": ["864842698333356032", "864842698333356033", "864842698333356034"] }
```

## 为什么需要它

雪花 ID 是一个 64 位整数：体积只有 UUID 的一半，天然按创建时间排序，做主键很便宜。但生成它
有个前提——**每个进程都必须持有一个其它存活进程都不持有的身份**。这个约束会渗透进每一个在本
进程内生成 ID 的服务：要么全都改成 StatefulSet，要么全都通过 Redis 协调，要么全都只能跑单副本。

把 ID 生成收拢成一个服务，这个约束就只需要在**一个地方**解决。其余服务调用 `Next` 即可，自身
保持无状态。

如果你只在**单个进程**里发号，那就别用这个服务——**直接用 `bwmarrin/snowflake`**，少一层网络、
少一层出错面。

## ID 布局

沿用 Twitter 原版雪花的分段方式：

```
| 未使用(1) | 时间戳(41) | 机房(0) | 机器(10) | 序号(12) |
```

| 段 | 默认宽度 | 含义 | 术语出处 |
| --- | --- | --- | --- |
| 时间戳 | 41 位 | 距 epoch 的毫秒数——约 69 年 | timestamp |
| **机房** | **0 位** | 哪个机房——默认不分机房 | Twitter: `datacenterId` |
| **机器** | **10 位** | 该机房内的哪台机器——最多 1024 台 | Twitter: `workerId` |
| 序号 | 12 位 | 该毫秒内的第几个——每台机器每毫秒 4096 个 | Twitter: `sequence`（bwmarrin 叫 `step`） |

最高位恒为 0，这样在没有无符号整数的语言里 ID 也是正数。序号段固定 12 位（bwmarrin 的默认
值），即 **400 万 ID/秒/机器**。

**`(机房, 机器)` 这一对，就是一个进程的身份。** 默认不分机房（机房段 0 位），10 位全归机器。

> **各段宽度和 epoch 是永久性的。** 你发出去的每一个 ID 都是相对它们解码的。发出第一个 ID
> 之前定好，之后**绝不要改**——改了会让所有已有 ID 解出错误的时间和错误的来源。

## 配置

| 参数 | 环境变量 | 默认值 | 含义 |
| --- | --- | --- | --- |
| `--worker-id` | `SNOWID_WORKER_ID` | **必填** | 本机器在其机房内的 ID |
| `--datacenter-id` | `SNOWID_DATACENTER_ID` | `0` | 本机房的 ID |
| `--worker-bits` | | `10` | 机器段宽度。`10` 位 = 每个机房最多 1024 台机器 |
| `--datacenter-bits` | | `0` | 机房段宽度。`0` = 不分机房 |
| `--epoch` | | `1577836800000` | 时间戳零点，**Unix 毫秒**（默认 2020-01-01 UTC） |
| `--addr` | | `:50051` | gRPC 监听地址 |

**红线只有一条：任意时刻，不能有两个存活的进程持有相同的 `(机房, 机器)` 组合。** 所以服务
**不会去猜** `--worker-id`——不给就拒绝启动：

```console
$ snowid-server
ERROR startup failed error="--worker-id is required: two live processes sharing an identity issue the same ids"
```

### 位数预算：加机房位，就得减机器位

64 位是**固定的**，最高位恒为 0，所以只有 63 位可分。序号段固定 12 位，剩下的**机房段、机器段
和时间戳段在抢同一块地**：

```
时间戳位数 = 63 - 机房位数 - 机器位数 - 12
```

**两个位宽参数是相加的，不是「机房从机器里切一块」。** 所以只加 `--datacenter-bits` 而不减
`--worker-bits`，是在从**时间戳**里偷位——ID 的可用年限会断崖式下跌：

| `--datacenter-bits` | `--worker-bits` | 身份总数 | 时间戳位数 | 能用多久 |
| --- | --- | --- | --- | --- |
| `0`（默认） | `10` | 1024 | 41 | **69.7 年** ✅ |
| `5` | `5` | 32 × 32 = 1024 | 41 | **69.7 年** ✅（Twitter 原版） |
| `3` | `7` | 8 × 128 = 1024 | 41 | **69.7 年** ✅ |
| `5` | **`10`** ← 忘了减 | 32 × 1024 = 32768 | 36 | **2.2 年** ❌ 从 2020 年算早就溢出 |

最后那行**启动就会被拒绝**，不会让你带着一个两年就崩的布局跑起来：

```console
$ snowid-server --datacenter-bits 5 --worker-id 2 --datacenter-id 1
ERROR startup failed error="--epoch=1577836800000 is too far in the past:
      36 bits of timestamp hold only 19088h44m36.736s, and 57277h6m24.432s have passed since it"
```

### 机房（datacenter）

默认不分机房。要分的话，就是 Twitter 原版的 5/5 分法——**注意 `--worker-bits` 要从 10 减到 5**：

```console
$ snowid-server --datacenter-bits 5 --worker-bits 5 --datacenter-id 1 --worker-id 2
INFO generator ready worker_id=2 worker_bits=5 step_bits=12 max_workers=32 \
     datacenter_id=1 datacenter_bits=5 max_datacenters=32 ...
```

**32 个机房 × 每机房 32 台机器 = 1024 个身份**——总数和默认一样，时间戳也还是 41 位。你是把
那 10 位身份**重新划分**了，不是凭空多要位。

**两个 ID 在底层是「位拼接」，不是「相加」：**

```go
identity = (datacenter_id << 机器位数) | worker_id
```

> ⚠️ **绝对不能用相加。** `机房=1,机器=2` 和 `机房=2,机器=1` 相加**都等于 3**——两个不同的
> 进程会拿到**同一个身份**，同一毫秒内必然发出**重复 ID**。位拼接才能保证 `(机房, 机器)` 这个
> **组合**是唯一的。实测这两组的底层身份值分别是 **34** 和 **65**。

任何一个 ID 溢出自己那几位，就会**侵占对方的位**，落到别人的身份上。所以服务会拒绝：

```console
$ snowid-server --datacenter-bits 5 --worker-bits 5 --datacenter-id 1 --worker-id 32
ERROR startup failed error="--worker-id=32 is out of range [0,31] for --worker-bits=5"
```

### 为什么要做启动校验

bwmarrin 的 `Generate()` **没有 error 返回，也不做边界检查**——它直接把时间戳移位塞进去。所以
位宽配错了它**不会报错**，而是**静默地**发出时间戳错误、符号位被污染（负数）、并且在段位回绕后
**重复**的 ID。

举例：`--worker-bits=19` 加上固定的 12 位序号，只剩 32 位时间戳 = **49.7 天**，而默认 epoch 是
2020 年。这个配置**早就溢出了**。所以只能在启动时拦下：

```console
$ snowid-server --worker-id 0 --worker-bits 19
ERROR startup failed error="--epoch=1577836800000 is too far in the past:
      32 bits of timestamp hold only 1193h2m47.296s, and 57276h16m11.412s have passed since it"
```

启动日志里的 `ids_valid_until` 会告诉你这套布局能用到哪一天。

## API

参见 [`api/proto/snowid/v1/snowid.proto`](api/proto/snowid/v1/snowid.proto)。

| RPC | 说明 |
| --- | --- |
| `Next(count)` | 返回 `count` 个 ID，升序。上限 1000，超过返回 `INVALID_ARGUMENT` |
| `GetLayout()` | 返回 epoch 和各段宽度，供客户端**本地解码** ID |

服务同时注册了标准 gRPC 健康检查和 server reflection。

## Go 客户端

```go
import "github.com/itmisx/snowid-server/pkg/client"

c, err := client.New(ctx, "dns:///snowid:50051")
defer c.Close()

id, err := c.Next(ctx)                  // 一次 RPC，一个 ID
ids, err := c.NextN(ctx, 500)           // 一次 RPC，500 个 ID

// 解码在本地做，不走网络
fmt.Println(c.Layout().Time(id))          // 生成时刻
fmt.Println(c.Layout().Datacenter(id))    // 出自哪个机房
fmt.Println(c.Layout().Worker(id))        // 该机房内的哪台机器
```

| 方法 | 说明 |
| --- | --- |
| `New(ctx, target, opts...)` | 拨号并取布局；`Close` 会关闭这个连接 |
| `NewWithConn(ctx, conn)` | 复用已有连接；`Close` **不会**关闭它 |
| `Next(ctx)` | 一个 ID，一次往返 |
| `NextN(ctx, n)` | n 个 ID。`n` 可以超过服务端上限，会自动拆成多次调用 |
| `Layout()` | 本地解码用 |
| `Identity()` / `MaxBatch()` | 对端是哪个 `(机房, 机器)`；服务端单次接受的上限 |

`New` 在连接时取一次布局，所以**服务端不可达会立刻失败**，而不是等到第一次 `Next`。默认是明文
+ round-robin（配合 `deploy/k8s` 的 headless Service）；你传的 `grpc.DialOption` 追加在默认值
**之后**，所以要加 TLS 直接传就行，会自然覆盖掉默认的明文。

**客户端不做缓冲，这是刻意的。** 每个 `Next` 就是一次 RPC。要「一次一个 ID」的手感，就
`NextN(ctx, 500)` 取一批自己发——**二十行的事**。客户端内部一旦加缓冲，就得决定「服务端死了怎么
办」「客户端关了缓冲里的 ID 还发不发」「ID 在队列里躺久了时间戳过期怎么办」，任何一个判断错了，
代价都比省下的那点往返大得多。（我们试过，四个 bug：进程 panic、永久卡死、两处静默失效。）

## 解码 ID

**在本地解码，不要走网络。** 解码只是几次位运算；为了读一个 ID 的时间戳而发起一次往返纯属浪费。
`GetLayout` 取一次，然后：

```
unix_milli = (id >> (datacenter_bits + worker_bits + step_bits)) + epoch_unix_milli
datacenter = (id >> (worker_bits + step_bits)) & ((1 << datacenter_bits) - 1)
worker     = (id >> step_bits) & ((1 << worker_bits) - 1)
step       =  id & ((1 << step_bits) - 1)
```

不分机房时 `datacenter_bits = 0`，上式里 `datacenter` 恒为 0，机器段拿到全部宽度——**同一套公式
两种情况都对**，不用特判。

**ID 过 JSON / JavaScript 边界时用字符串。** 它们存不下精确的 64 位整数。

## 部署：多副本下如何保证 ID 唯一

见 [`deploy/k8s`](deploy/k8s)。核心只有一句话：**每个 pod 必须拿到一个不同的 worker ID。**

StatefulSet 已经给了你一个天然唯一、重启后稳定的东西——**pod 序号**。而且 StatefulSet 控制器会
把它写成一个标签（`apps.kubernetes.io/pod-index`），**Deployment 不会写**。直接用 downward API
注入即可，服务端**一行解析代码都不用写**：

```yaml
env:
  - name: SNOWID_WORKER_ID
    valueFrom:
      fieldRef:
        fieldPath: metadata.labels['apps.kubernetes.io/pod-index']
```

```
snowid-0  ->  pod-index "0"  ->  SNOWID_WORKER_ID=0
snowid-1  ->  pod-index "1"  ->  SNOWID_WORKER_ID=1
snowid-2  ->  pod-index "2"  ->  SNOWID_WORKER_ID=2
```

Kubernetes 保证任一时刻最多只有一个 pod 持有某个序号——滚动更新会先终止 `snowid-1`，再把它
**重建为 `snowid-1`**，绝不重叠。所以 worker ID 唯一、且跨重启稳定，**不需要任何外部协调**。

**把同一份 spec 当 Deployment 跑，它会 fail-closed**：Deployment 不写 pod-index 标签 →
downward API 返回空串 → 服务**拒绝启动**（CrashLoop），而不是编一个 ID 出来。

> **绝不要去解析主机名。** Deployment 的 pod 名（`snowid-7d4b9c5f8-84272`）末尾是随机后缀，而
> K8s 的后缀字母表里**含数字**——约 **1/853** 的 pod 后缀是纯数字，会被当成合法序号解析出来，
> 于是服务带着一个**每次重启都变的随机 ID** 欢快启动 → 重复 ID。**靠字符串形状区分不了
> StatefulSet 和 Deployment。**

**机房 ID 是集群的属性，不是 pod 的属性**，所以从集群自己的配置里来（例如 ConfigMap），同一
集群内所有副本共用一个值。

需要 Kubernetes **1.28+**（pod-index 标签在 1.28 是默认开启的 beta，1.32 转 GA）。更老的集群就
自己设 `--worker-id`——一个 ID 一个 StatefulSet。

**单副本已经能发 400 万 ID/秒**，所以多副本是为了容灾，不是为了吞吐。

## Docker 镜像

镜像托管在 GitHub Container Registry，**同时支持 `linux/amd64` 和 `linux/arm64`**——
`docker pull` 会自动挑对架构。

```console
$ docker run --rm -p 50051:50051 ghcr.io/itmisx/snowid-server:v0.1.0 --worker-id 0
```

镜像基于 distroless，只装了一个静态二进制：没有 shell、没有包管理器、以 UID 65532 非 root
运行。

**在部署清单里请固定 tag 或 digest，不要用 `:latest`。** 两个副本在不同时刻拉 `:latest`
可能跑上不同的二进制，而 ID 布局是**永久性**的——所有副本必须永远对它保持一致。`:latest`
只是为了让你 `docker run` 时方便。

不跑容器的话，[Releases](https://github.com/itmisx/snowid-server/releases) 里有各平台的二进制
（linux / macOS × amd64 / arm64），静态链接，解开即用。

## 发布

打个 tag 就行，不需要配任何 secret——用的是内置的 `GITHUB_TOKEN`：

```bash
git tag v0.1.0
git push origin v0.1.0
```

[`release.yml`](.github/workflows/release.yml) 会依次：

1. **先跑测试**——tag 绝不会指向一棵没绿过的树
2. 用 buildx 构建 `linux/amd64` + `linux/arm64`（Go **交叉编译**，不走 QEMU 模拟，快一个数量级）
3. 推到 `ghcr.io/itmisx/snowid-server`，打上 `v0.1.0` / `0.1` / `0` / `latest` / `sha-<commit>`
4. **把两个架构的镜像都拉回来真的跑一遍**——能构建 ≠ 能启动（那个 `USER nonroot` 的 bug 就是
   构建得好好的、pod 永远起不来）
5. 生成 SBOM 和 build provenance 证明，可用 `gh attestation verify` 校验来源
6. **最后**才创建 GitHub Release，附上四个平台的二进制和 `checksums.txt`——一个指向拉不下来的
   镜像的 Release，比没有 Release 更糟

本地想先验证多架构能不能构建：

```bash
make docker-multiarch
```

> **首次发布后，镜像默认是 private。** GitHub 不会让它继承仓库的可见性。要公开的话去
> `Packages → snowid-server → Package settings → Change visibility` 改一次，之后所有版本都继承。

## 开发

```bash
make test                     # go test -race
make lint                     # gofmt + go vet
make build
make run                      # 等价于 go run ./cmd/snowid-server --worker-id 0
./buf.gen.sh                  # 修改 .proto 后重新生成桩代码
```

## 许可证

MIT——见 [LICENSE](LICENSE)。
