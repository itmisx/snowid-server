<div align="center">

# ❄️ snowid-server

**分布式雪花 ID 生成服务** · gRPC · 零外部依赖 · amd64 / arm64

[![ci](https://github.com/itmisx/snowid-server/actions/workflows/ci.yml/badge.svg)](https://github.com/itmisx/snowid-server/actions/workflows/ci.yml) [![release](https://github.com/itmisx/snowid-server/actions/workflows/release.yml/badge.svg)](https://github.com/itmisx/snowid-server/actions/workflows/release.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/itmisx/snowid-server.svg)](https://pkg.go.dev/github.com/itmisx/snowid-server) [![license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

**中文** · [English](README.en.md)

</div>

---

把 [`bwmarrin/snowflake`](https://github.com/bwmarrin/snowflake) 包了一层 gRPC，对外发放
**全局唯一、大致按时间有序的 64 位 ID**。不需要 ZooKeeper，不需要 Redis，也不需要数据库。

**单节点 400 万 ID/秒。**

---

## 🚀 快速开始

### 用 Docker 跑起来

```bash
docker run -d -p 50051:50051 \
  ghcr.io/itmisx/snowid-server:latest --worker-id 0
```

> [!IMPORTANT]
> **`--worker-id` 是必填的，服务不会去猜。**
> 两个存活的进程共用一个 ID，就会发出**重复的雪花 ID**。多副本部署时每个进程必须拿到不同的值
> ——见 [🎯 部署](#deploy)。

### 获取 ID

**Go 客户端**

```go
import "github.com/itmisx/snowid-server/pkg/client"

c, _ := client.New(ctx, "dns:///snowid:50051")
defer c.Close()

id, _ := c.Next(ctx)          // 一个
ids, _ := c.NextN(ctx, 500)   // 一批

// 解码在本地，不走网络
c.Layout().Time(id)           // 生成时刻
c.Layout().Worker(id)         // 哪台机器
c.Layout().Datacenter(id)     // 哪个机房
```

**其它语言** —— 就是普通的 gRPC，用生成的 stub 调即可。服务端注册了 reflection，所以
`grpcurl` 连 `.proto` 都不用带：

```console
$ grpcurl -plaintext -d '{"count": 3}' localhost:50051 snowid.v1.SnowId/Next
{
  "ids": [
    "864842698333356032",
    "864842698333356033",
    "864842698333356034"
  ]
}
```

> [!TIP]
> **批量取。** 一次 `NextN(500)` 只花一次往返，500 次 `Next()` 要花 500 次。客户端**不做缓冲**
> ——要「一次一个」的手感，自己拿一批在本地发就行，二十行的事。

### 部署到 Kubernetes（生产）

```bash
kubectl apply -f https://raw.githubusercontent.com/itmisx/snowid-server/main/deploy/k8s/statefulset.yaml
```

每个 pod 自动从 StatefulSet 序号拿到自己的 `worker-id`，**不需要任何外部协调**。

---

## ⚙️ 配置

| 参数 | 环境变量 | 默认 | 说明 |
| :--- | :--- | :--- | :--- |
| `--worker-id` | `SNOWID_WORKER_ID` | ⚠️ **必填** | 本机器在其机房内的 ID。**唯一一个逐 pod 不同的值**，所以支持环境变量 |
| `--datacenter-id` | | ⚠️ 设了 `datacenter-bits` 就**必填** | 本机房的 ID。整个集群一个值，所有副本共用 |
| `--node-bits` | | `10` | **整个** node 段宽度（机房 + 机器） |
| `--datacenter-bits` | | `0` | node 段里有几位是机房。机器段 = `node-bits − datacenter-bits` |
| `--step-bits` | | `12` | 序号段宽度 → 每机器每毫秒 **4096** 个 ID |
| `--epoch` | | `1727712000000` | 时间戳零点，**Unix 毫秒**（2024-10-01 UTC+8） |
| `--addr` | | `:50051` | gRPC 监听地址 |

> [!WARNING]
> **`--node-bits`、`--datacenter-bits`、`--step-bits`、`--epoch` 是永久性的。**
> 每一个已发出的 ID 都是相对它们解码的。**发出第一个 ID 之前定好，之后绝不要改。**

<details>
<summary><b>🧮 位数预算 —— 唯一的规则：node + step ≤ 22</b></summary>

<br>

64 位是固定的。最高位恒为 0，时间戳、node、序号分剩下的 63 位：

```
node 段 = 机房段 + 机器段          （机器段是【减】出来的，不用配）
时间戳位 = 63 − node-bits − step-bits
```

**规则只有一条：**

```
--node-bits + --step-bits ≤ 22
```

这是 bwmarrin 自己文档里写的约束。守住它，时间戳就**至少留 41 位 ≈ 69 年**——布局不可能悄悄
到期，所以也不需要别的检查。

| `--node-bits` | `--step-bits` | 合计 | 身份数 | ID/毫秒/机器 | |
| :---: | :---: | :---: | ---: | ---: | :--- |
| `10` *(默认)* | `12` | 22 | 1,024 | 4,096 | ✅ Twitter 原版 |
| `12` | `10` | 22 | 4,096 | 1,024 | ✅ 更多机器，更低单机吞吐 |
| `5` | `17` | 22 | 32 | 131,072 | ✅ 更少机器，更高单机吞吐 |
| `11` | `12` | **23** | — | — | ❌ 超了 |

超了就**拒绝启动**：

```console
$ snowid-server --worker-id 0 --node-bits 11
ERROR startup failed error="--node-bits(11) + --step-bits(12) = 23, and snowflake has only
      22 bits for the two of them; everything else is the timestamp"
```

> [!NOTE]
> **bwmarrin 不会拦你。** 它的注释白纸黑字写着「你总共只有 22 位给 Node/Step 分」，但**代码里
> 没有任何检查**——`NewNode()` 只校验 node ID 的范围。位宽设过头，`Generate()` 会把时间戳直接
> **移出段外**，一声不吭地发出时间戳错误、符号位被污染（负数）、并且回绕后重复的 ID。
> 这道守卫必须由我们来做。

</details>

<details>
<summary><b>🏢 机房（datacenter）—— 位拼接，不是相加</b></summary>

<br>

默认不分机房（`--datacenter-bits=0`，node 段整整 10 位全归机器）。要分的话，就从 node 段里
**切一块**给机房——**机器段是减出来的，不用配**：

```console
$ snowid-server --datacenter-bits 5 --datacenter-id 1 --worker-id 2
INFO generator ready worker_id=2 node_bits=10 step_bits=12 max_workers=32 \
     datacenter_id=1 datacenter_bits=5 max_datacenters=32 node_id=34
```

`node-bits=10`，切 5 位给机房 → 机器剩 5 位 → **32 个机房 × 每机房 32 台 = 1024 个身份**。
总数和默认一样，时间戳也还是 41 位——你只是把那 10 位**重新划分**了，没有多要一位。

底层是**位拼接**：

```go
node_id = (datacenter_id << 机器位数) | worker_id      // 机器位数 = node-bits - datacenter-bits
```

> [!CAUTION]
> **绝对不能用相加。**
> `机房=1,机器=2` 和 `机房=2,机器=1` 相加**都等于 3** —— 两个不同的进程会拿到**同一个身份**，
> 同一毫秒内必然发出**重复 ID**。位拼接才能保证 `(机房, 机器)` 这个**组合**唯一。
> 实测这两组的底层身份值分别是 **34** 和 **65**。

任何一个 ID 溢出自己那几位，就会**侵占对方的位**、落到别人的身份上。所以服务会拒绝：

```console
$ snowid-server --datacenter-bits 5 --datacenter-id 1 --worker-id 32
ERROR startup failed error="--worker-id=32 is out of range [0,31]: --node-bits(10) less
      --datacenter-bits(5) leaves 5 bits for the worker"
```

</details>

---

## 📐 ID 布局

沿用 **Twitter 原版雪花**的分段方式和命名：

```
┌────────┬──────────────┬────────────┬─────────────┬───────────┐
│ 未使用  │   时间戳      │    机房     │    机器      │   序号     │
│  1 位   │   41 位      │   0 位      │   10 位      │   12 位    │
└────────┴──────────────┴────────────┴─────────────┴───────────┘
                          datacenter      worker         step
```

| 段 | 默认 | 含义 |
| :--- | :---: | :--- |
| 时间戳 | 41 位 | 距 epoch 的毫秒数 → 约 **69 年** |
| **机房** | 0 位 | 哪个机房（默认不分） · Twitter: `datacenterId` |
| **机器** | 10 位 | 该机房内的哪台机器 → 最多 **1024** 台 · Twitter: `workerId` |
| 序号 | 12 位 | 该毫秒内的第几个 → **4096** 个/毫秒/机器 |

> [!NOTE]
> 最高位恒为 0，这样在没有无符号整数的语言里 ID 也是**正数**。
> **`(机房, 机器)` 这一对，就是一个进程的身份。**

<details>
<summary><b>🔓 本地解码（任意语言）</b></summary>

<br>

**解码不要走网络。** 只是几次位运算；为了读一个 ID 的时间戳而发起一次往返纯属浪费。
调一次 `GetLayout` 拿到各段宽度，之后永远本地算：

```python
unix_milli = (id >> (datacenter_bits + worker_bits + step_bits)) + epoch_unix_milli
datacenter = (id >> (worker_bits + step_bits)) & ((1 << datacenter_bits) - 1)
worker     = (id >> step_bits) & ((1 << worker_bits) - 1)
step       =  id & ((1 << step_bits) - 1)
```

不分机房时 `datacenter_bits = 0`，上式里 `datacenter` 恒为 0，机器段拿到全部宽度
——**同一套公式两种情况都对**，不用特判。

> [!WARNING]
> **ID 过 JSON / JavaScript 边界时用字符串。** 它们存不下精确的 64 位整数。

</details>

---

## 🔌 API

参见 [`snowid.proto`](api/proto/snowid/v1/snowid.proto)。同时注册了标准 gRPC **健康检查**和
**server reflection**（所以 `grpcurl` 不用带 `.proto` 就能用）。

| RPC | 说明 |
| :--- | :--- |
| `Next(count)` | 返回 `count` 个 ID，升序。上限 **1000**，超过返回 `INVALID_ARGUMENT` |
| `GetLayout()` | 返回 epoch 和各段宽度，供客户端**本地解码** |

<details>
<summary><b>📘 Go 客户端完整 API</b></summary>

<br>

| 方法 | 说明 |
| :--- | :--- |
| `New(ctx, target, opts...)` | 拨号并取布局；`Close` 会关闭这个连接 |
| `NewWithConn(ctx, conn)` | 复用你已有的连接；`Close` **不会**关闭它 |
| `Next(ctx)` | 一个 ID，一次往返 |
| `NextN(ctx, n)` | n 个 ID。`n` 可超过服务端上限，会自动拆成多次调用 |
| `Layout()` | 本地解码用的布局 |
| `Identity()` | 对端是哪个 `(机房, 机器)` |
| `MaxBatch()` | 服务端单次接受的上限 |
| `Close()` | 关闭连接（`NewWithConn` 借来的除外） |

`New` 在连接时取一次布局，所以**服务端不可达会立刻失败**，而不是等到第一次 `Next`。

默认拨号选项是**明文 + round-robin**（配合 `deploy/k8s` 的 headless Service）。你传的
`grpc.DialOption` 追加在默认值**之后**，所以要加 TLS 直接传就行，会自然覆盖掉默认的明文。

**客户端刻意不做缓冲。** 每个 `Next` 就是一次 RPC。加了缓冲就得决定：服务端死了怎么办？客户端
关了缓冲里的 ID 还发不发？ID 躺久了时间戳过期怎么办？任何一个判断错了，代价都比省下的那点往返
大得多——我们试过，四个 bug：进程 panic、永久卡死、两处静默失效。

</details>

---

<a id="deploy"></a>

## 🎯 部署：多副本如何保证唯一

核心只有一句话：**每个 pod 必须拿到一个不同的 `worker-id`。**

StatefulSet 已经给了你一个天然唯一、重启后稳定的东西——**pod 序号**。而且 StatefulSet 控制器会
把它写成标签 `apps.kubernetes.io/pod-index`，**Deployment 不会写**。直接用 downward API 注入，
服务端**一行解析代码都不用写**：

```yaml
env:
  - name: SNOWID_WORKER_ID
    valueFrom:
      fieldRef:
        fieldPath: metadata.labels['apps.kubernetes.io/pod-index']
```

```
snowid-0 → pod-index "0" → SNOWID_WORKER_ID=0
snowid-1 → pod-index "1" → SNOWID_WORKER_ID=1
snowid-2 → pod-index "2" → SNOWID_WORKER_ID=2
```

Kubernetes 保证**任一时刻最多一个 pod 持有某个序号**——滚动更新会先终止 `snowid-1`，再把它
**重建为 `snowid-1`**，绝不重叠。

> [!TIP]
> **把同一份 spec 当 Deployment 跑，它会 fail-closed。**
> Deployment 不写 pod-index 标签 → downward API 返回空串 → 服务**拒绝启动**（CrashLoop），
> 而不是编一个 ID 出来。这正是我们要的行为。

<details>
<summary><b>☠️ 为什么绝不能解析主机名</b></summary>

<br>

看起来 Deployment 的 pod 名（`snowid-7d4b9c5f8-84272`）末尾是随机后缀，解析不出序号——**错。**

K8s 的随机后缀字母表是 `bcdfghjklmnpqrstvwxz2456789`，**里面有 7 个数字**。所以约 **1/853** 的
pod 后缀是**纯数字**，会被当成合法序号解析出来：

```
PodOrdinal("snowid-7d4b9c5f8-84272") = 84272   ← 被当成序号了！
```

于是服务带着一个**每次重启都变的随机 ID** 欢快启动 → **重复 ID**。

**靠字符串形状区分不了 StatefulSet 和 Deployment。** 这个 bug 我们真写过，也真被测试抓到了。
用 `pod-index` 标签是精确、无歧义、且 fail-closed 的。

</details>

<details>
<summary><b>🌍 多机房部署</b></summary>

<br>

**机房 ID 是集群的属性，不是 pod 的属性**——同一集群内所有副本共用一个值。所以它和位宽一样，
就写在启动参数里；只有逐 pod 不同的 `worker-id` 才需要走环境变量：

```yaml
# 机房 A 的集群
args:
  - --datacenter-bits=5      # node-bits(10) 里切 5 位给机房，机器段自动剩 5 位
  - --datacenter-id=0        # 必填。机房 B 的集群改成 =1，其余一模一样
env:
  - name: SNOWID_WORKER_ID   # 逐 pod 不同 → 只能从 downward API 来
    valueFrom:
      fieldRef:
        fieldPath: metadata.labels['apps.kubernetes.io/pod-index']
```

> [!IMPORTANT]
> **只要设了 `--datacenter-bits`，`--datacenter-id` 就是必填的——没有默认值。**
>
> 因为**默认值 `0` 就是一个默认身份**。而重复 ID 最真实的来法，就是把一份跑得好好的 yaml
> 复制到第二个集群、什么都不改。所以第二个集群必须把 `--datacenter-id=1` **说出口**，否则
> 服务直接拒绝启动。这和 `--worker-id` 必填是同一个道理。

</details>

> [!NOTE]
> 需要 **Kubernetes 1.28+**（`pod-index` 标签在 1.28 是默认开启的 beta，1.32 转 GA）。
> 更老的集群就自己设 `--worker-id`——一个 ID 一个 StatefulSet。
>
> **单副本已经能发 400 万 ID/秒**，所以多副本是为了**容灾**，不是为了吞吐。

---

## 📦 镜像

托管在 GitHub Container Registry，**同时支持 `linux/amd64` 和 `linux/arm64`**，`docker pull`
会自动挑对架构。

```bash
docker pull ghcr.io/itmisx/snowid-server:v0.1.0
```

镜像基于 **distroless**：一个静态二进制，没有 shell、没有包管理器，以 UID 65532 **非 root** 运行。

> [!CAUTION]
> **部署清单里请固定 tag 或 digest，不要用 `:latest`。**
> 两个副本在不同时刻拉 `:latest` 可能跑上**不同的二进制**，而 ID 布局是**永久性**的——所有副本
> 必须永远对它保持一致。`:latest` 只是为了让你 `docker run` 时方便。

不跑容器的话，[Releases](https://github.com/itmisx/snowid-server/releases) 里有各平台的静态
二进制（linux / macOS × amd64 / arm64）。

<details>
<summary><b>🏷️ 发布流程</b></summary>

<br>

打个 tag 就行，**不需要配任何 secret**（用的是内置 `GITHUB_TOKEN`）：

```bash
git tag v0.1.0
git push origin v0.1.0
```

[`release.yml`](.github/workflows/release.yml) 会：

1. 用 buildx 构建 `linux/amd64` + `linux/arm64`（Go **交叉编译**，不走 QEMU，快一个数量级）
2. 推到 `ghcr.io`，打上 **`v0.1.0`**（和 git tag 完全一致）和 `latest`
3. **把镜像拉回来真的跑一遍**——能构建 ≠ 能启动
4. 创建 GitHub Release，附上四个平台的二进制和 `checksums.txt`

普通 `git push` **不会**触发镜像构建，只跑测试（~20 秒）。

> [!NOTE]
> **首次发布后镜像默认是 private**，GitHub 不会让它继承仓库的可见性。去
> `Packages → snowid-server → Package settings → Change visibility` 改一次即可，之后所有版本
> 都继承。

</details>

---

## 🛠️ 开发

```bash
make test     # go test -race
make lint     # gofmt + go vet
make build
make run      # go run ./cmd/snowid-server --worker-id 0
./buf.gen.sh  # 修改 .proto 后重新生成桩代码
```

<details>
<summary><b>🤔 什么时候【不该】用这个服务</b></summary>

<br>

**如果你只在单个进程里发号，别用它** —— 直接 `import "github.com/bwmarrin/snowflake"`。

少一层网络、少一层出错面。这个服务存在的唯一意义，是把「每个进程必须持有唯一身份」这个约束
**收拢到一个地方**解决，好让你其余的服务保持无状态。单进程根本没有这个问题。

</details>

---

<div align="center">

**MIT** · [LICENSE](LICENSE)

</div>
