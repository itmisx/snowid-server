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
| `--worker-id` | `SNOWID_WORKER_ID` | ⚠️ **必填** | 本机器在其机房内的 ID |
| `--datacenter-id` | `SNOWID_DATACENTER_ID` | `0` | 本机房的 ID |
| `--worker-bits` | | `10` | 机器段宽度 → 每机房最多 **1024** 台 |
| `--datacenter-bits` | | `0` | 机房段宽度，`0` = 不分机房 |
| `--epoch` | | `1577836800000` | 时间戳零点，**Unix 毫秒**（2020-01-01 UTC） |
| `--addr` | | `:50051` | gRPC 监听地址 |

> [!WARNING]
> **`--worker-bits`、`--datacenter-bits`、`--epoch` 是永久性的。**
> 每一个已发出的 ID 都是相对它们解码的。**发出第一个 ID 之前定好，之后绝不要改。**

<details>
<summary><b>🧮 位数预算 —— 加机房位，就得减机器位</b></summary>

<br>

64 位是固定的，最高位恒为 0，所以只有 63 位可分。序号段固定 12 位，剩下的**机房段、机器段和
时间戳段在抢同一块地**：

```
时间戳位数 = 63 - 机房位数 - 机器位数 - 12
```

**两个位宽参数是相加的**，不是「机房从机器里切一块」。所以只加 `--datacenter-bits` 而不减
`--worker-bits`，是在从**时间戳**里偷位——可用年限会断崖式下跌：

| `--datacenter-bits` | `--worker-bits` | 身份总数 | 时间戳位 | 能用多久 |
| :---: | :---: | :---: | :---: | :--- |
| `0` *(默认)* | `10` | 1024 | 41 | ✅ **69.7 年** |
| `5` | `5` | 32 × 32 = 1024 | 41 | ✅ **69.7 年** *(Twitter 原版)* |
| `3` | `7` | 8 × 128 = 1024 | 41 | ✅ **69.7 年** |
| `5` | **`10`** ⚠️ 忘了减 | 32 × 1024 = 32768 | 36 | ❌ **2.2 年**，从 2020 年算早就溢出 |

最后那行**启动就会被拒绝**，不会让你带着一个两年就崩的布局跑起来：

```console
$ snowid-server --datacenter-bits 5 --worker-id 2 --datacenter-id 1
ERROR startup failed error="--epoch=1577836800000 is too far in the past:
      36 bits of timestamp hold only 19088h44m36.736s, and 57277h6m24.432s have passed since it"
```

</details>

<details>
<summary><b>🏢 机房（datacenter）—— 位拼接，不是相加</b></summary>

<br>

默认不分机房。要分的话，就是 Twitter 原版的 5/5 分法（**注意 `--worker-bits` 要从 10 减到 5**）：

```console
$ snowid-server --datacenter-bits 5 --worker-bits 5 --datacenter-id 1 --worker-id 2
INFO generator ready worker_id=2 worker_bits=5 max_workers=32 \
     datacenter_id=1 datacenter_bits=5 max_datacenters=32 ...
```

**32 个机房 × 每机房 32 台 = 1024 个身份**——总数和默认一样，时间戳也还是 41 位。你是把那 10
位身份**重新划分**了，不是凭空多要位。

底层是**位拼接**：

```go
identity = (datacenter_id << 机器位数) | worker_id
```

> [!CAUTION]
> **绝对不能用相加。**
> `机房=1,机器=2` 和 `机房=2,机器=1` 相加**都等于 3** —— 两个不同的进程会拿到**同一个身份**，
> 同一毫秒内必然发出**重复 ID**。位拼接才能保证 `(机房, 机器)` 这个**组合**唯一。
> 实测这两组的底层身份值分别是 **34** 和 **65**。

任何一个 ID 溢出自己那几位，就会**侵占对方的位**、落到别人的身份上。所以服务会拒绝：

```console
$ snowid-server --datacenter-bits 5 --worker-bits 5 --datacenter-id 1 --worker-id 32
ERROR startup failed error="--worker-id=32 is out of range [0,31] for --worker-bits=5"
```

</details>

<details>
<summary><b>🛡️ 为什么要做启动校验</b></summary>

<br>

bwmarrin 的 `Generate()` **没有 error 返回，也不做边界检查**——它直接把时间戳移位塞进去。所以
位宽配错了它**不会报错**，而是**静默地**发出：

- 时间戳错误的 ID
- 符号位被污染的 ID（**负数**）
- 段位回绕后**重复**的 ID

举例：`--worker-bits=19` 加上固定的 12 位序号，只剩 32 位时间戳 = **49.7 天**，而默认 epoch 是
2020 年——**这个配置早就溢出了**。只能在启动时拦下：

```console
$ snowid-server --worker-id 0 --worker-bits 19
ERROR startup failed error="--epoch=1577836800000 is too far in the past:
      32 bits of timestamp hold only 1193h2m47.296s, and 57276h16m11.412s have passed since it"
```

启动日志里的 `ids_valid_until` 会告诉你这套布局能用到哪一天。

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

**机房 ID 是集群的属性，不是 pod 的属性**——同一集群内所有副本共用一个值。所以它从集群自己的
配置里来，例如 ConfigMap：

```yaml
args:
  - --datacenter-bits=5
  - --worker-bits=5
env:
  - name: SNOWID_WORKER_ID
    valueFrom:
      fieldRef:
        fieldPath: metadata.labels['apps.kubernetes.io/pod-index']
  - name: SNOWID_DATACENTER_ID
    valueFrom:
      configMapKeyRef:
        name: cluster-info
        key: datacenter-id
```

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
