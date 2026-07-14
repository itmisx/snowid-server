<div align="center">

# ❄️ snowid-server

**Distributed snowflake ID service** · gRPC · no external dependencies · amd64 / arm64

[![ci](https://github.com/itmisx/snowid-server/actions/workflows/ci.yml/badge.svg)](https://github.com/itmisx/snowid-server/actions/workflows/ci.yml) [![release](https://github.com/itmisx/snowid-server/actions/workflows/release.yml/badge.svg)](https://github.com/itmisx/snowid-server/actions/workflows/release.yml) [![Go Reference](https://pkg.go.dev/badge/github.com/itmisx/snowid-server.svg)](https://pkg.go.dev/github.com/itmisx/snowid-server) [![license](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[中文](README.md) · **English**

</div>

---

A gRPC wrapper around [`bwmarrin/snowflake`](https://github.com/bwmarrin/snowflake) that hands
out **unique, roughly time-ordered 64-bit IDs**. No ZooKeeper, no Redis, no database.

**4,096,000 IDs/second per node.**

---

## 🚀 Quick start

### Try it with Docker

```bash
docker run -d -p 50051:50051 \
  ghcr.io/itmisx/snowid-server:latest --worker-id 0
```

> [!IMPORTANT]
> **`--worker-id` is required. The server will not guess it.**
> Two live processes sharing one issue **duplicate IDs**. Every replica must hold a different
> value — see [🎯 Deploying](#deploy).

### Get IDs

**Go client**

```go
import "github.com/itmisx/snowid-server/pkg/client"

c, _ := client.New(ctx, "dns:///snowid:50051")
defer c.Close()

id, _ := c.Next(ctx)          // one
ids, _ := c.NextN(ctx, 500)   // a batch

// Decoded here, not over the network.
c.Layout().Time(id)           // when
c.Layout().Worker(id)         // which machine
c.Layout().Datacenter(id)     // which datacenter
```

**Any other language** — it is plain gRPC; call the generated stub. Server reflection is
registered, so `grpcurl` does not even need the `.proto`:

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
> **Ask in batches.** One `NextN(500)` costs one round trip; 500 `Next()` calls cost 500.
> The client **does not buffer** — if you want the feel of one at a time, take a batch and hand
> them out yourself. That is twenty lines.

### Deploy to Kubernetes (production)

```bash
kubectl apply -f https://raw.githubusercontent.com/itmisx/snowid-server/main/deploy/k8s/statefulset.yaml
```

Each pod takes its `worker-id` from its own StatefulSet ordinal — **no external coordination**.

---

## ⚙️ Configuration

| Flag | Environment | Default | Meaning |
| :--- | :--- | :--- | :--- |
| `--worker-id` | `SNOWID_WORKER_ID` | ⚠️ **required** | This machine's ID within its datacenter |
| `--datacenter-id` | `SNOWID_DATACENTER_ID` | `0` | This datacenter's ID |
| `--worker-bits` | | `10` | Worker segment width → up to **1024** per datacenter |
| `--datacenter-bits` | | `0` | Datacenter segment width. `0` = no datacenters |
| `--epoch` | | `1577836800000` | Timestamp zero point, **unix ms** (2020-01-01 UTC) |
| `--addr` | | `:50051` | gRPC listen address |

> [!WARNING]
> **`--worker-bits`, `--datacenter-bits` and `--epoch` are permanent.**
> Every ID you have ever issued is decoded relative to them.
> **Choose them before the first ID, and never change them.**

<details>
<summary><b>🧮 The bit budget — add datacenter bits, take them off the worker</b></summary>

<br>

There are 64 bits and no more. The top one stays 0, so 63 are yours to divide, and the step
segment takes a fixed 12. The datacenter, the worker and **the timestamp all compete for the
same remainder**:

```
timestamp_bits = 63 - datacenter_bits - worker_bits - 12
```

**The two width flags ADD** — the datacenter is not carved out of the worker. So adding
`--datacenter-bits` without taking the same off `--worker-bits` steals the bits from the
*timestamp*, and the layout's lifespan falls off a cliff:

| `--datacenter-bits` | `--worker-bits` | Identities | Timestamp bits | Lasts |
| :---: | :---: | :---: | :---: | :--- |
| `0` *(default)* | `10` | 1024 | 41 | ✅ **69.7 years** |
| `5` | `5` | 32 × 32 = 1024 | 41 | ✅ **69.7 years** *(Twitter's)* |
| `3` | `7` | 8 × 128 = 1024 | 41 | ✅ **69.7 years** |
| `5` | **`10`** ⚠️ forgot | 32 × 1024 = 32768 | 36 | ❌ **2.2 years** — expired in 2022 |

That last row is **refused at startup**. You do not get to run a layout that expired two years
after its own epoch:

```console
$ snowid-server --datacenter-bits 5 --worker-id 2 --datacenter-id 1
ERROR startup failed error="--epoch=1577836800000 is too far in the past:
      36 bits of timestamp hold only 19088h44m36.736s, and 57277h6m24.432s have passed since it"
```

</details>

<details>
<summary><b>🏢 Datacenters — concatenated, never added</b></summary>

<br>

Off by default. Turn them on and you get Twitter's original 5/5 split (note that
`--worker-bits` comes **down** from 10 to 5):

```console
$ snowid-server --datacenter-bits 5 --worker-bits 5 --datacenter-id 1 --worker-id 2
INFO generator ready worker_id=2 worker_bits=5 max_workers=32 \
     datacenter_id=1 datacenter_bits=5 max_datacenters=32 ...
```

**32 datacenters × 32 workers each = 1024 identities** — the same total as the default, with the
timestamp still on 41 bits. You have **re-divided** those 10 identity bits, not asked for more.

Underneath, the two are **concatenated**:

```go
identity = (datacenter_id << worker_bits) | worker_id
```

> [!CAUTION]
> **Never add them.**
> `datacenter=1,worker=2` and `datacenter=2,worker=1` both **add to 3** — two different processes
> would get **the same identity**, and every ID they issued in the same millisecond would be a
> **duplicate**. Concatenation is what makes the *pair* unique.
> Measured: those two pack to **34** and **65**.

Either ID overflowing its segment would spill into the other's bits and land on somebody else's
identity, so the server refuses:

```console
$ snowid-server --datacenter-bits 5 --worker-bits 5 --datacenter-id 1 --worker-id 32
ERROR startup failed error="--worker-id=32 is out of range [0,31] for --worker-bits=5"
```

</details>

<details>
<summary><b>🛡️ Why startup validation exists</b></summary>

<br>

bwmarrin's `Generate()` **returns no error and does no bounds check** — it shifts the timestamp
into place and hands you the result. A bad layout therefore **does not fail**. It **silently**
emits:

- IDs whose time is wrong
- IDs whose sign bit is set — **negative IDs**
- IDs that **repeat** once the segment wraps

For example: `--worker-bits=19`, plus the fixed 12 step bits, leaves 32 bits of timestamp =
**49.7 days** — against an epoch in 2020. **That layout overflowed years ago.** So it has to be
caught at startup, or never:

```console
$ snowid-server --worker-id 0 --worker-bits 19
ERROR startup failed error="--epoch=1577836800000 is too far in the past:
      32 bits of timestamp hold only 1193h2m47.296s, and 57276h16m11.412s have passed since it"
```

The `ids_valid_until` line in the startup log says how long the layout you chose lasts.

</details>

---

## 📐 ID layout

The segments are **Twitter's original snowflake**, and so are their names:

```
┌────────┬──────────────┬────────────┬─────────────┬───────────┐
│ unused │  timestamp   │ datacenter │   worker    │   step    │
│  1 bit │   41 bits    │   0 bits   │   10 bits   │  12 bits  │
└────────┴──────────────┴────────────┴─────────────┴───────────┘
```

| Segment | Default | Meaning |
| :--- | :---: | :--- |
| timestamp | 41 bits | milliseconds since the epoch → about **69 years** |
| **datacenter** | 0 bits | which datacenter (off by default) · Twitter: `datacenterId` |
| **worker** | 10 bits | which machine inside it → up to **1024** · Twitter: `workerId` |
| step | 12 bits | position within the millisecond → **4096**/ms/worker |

> [!NOTE]
> The top bit stays zero, so IDs are **positive** in languages without unsigned integers.
> **The `(datacenter, worker)` pair is a process's identity.**

<details>
<summary><b>🔓 Decoding locally (any language)</b></summary>

<br>

**Never decode over the network.** It is a few bit operations; a round trip to read an ID's
timestamp is pure waste. Call `GetLayout` once for the segment widths, then decode forever:

```python
unix_milli = (id >> (datacenter_bits + worker_bits + step_bits)) + epoch_unix_milli
datacenter = (id >> (worker_bits + step_bits)) & ((1 << datacenter_bits) - 1)
worker     = (id >> step_bits) & ((1 << worker_bits) - 1)
step       =  id & ((1 << step_bits) - 1)
```

With datacenters off, `datacenter_bits` is 0, so `datacenter` comes out 0 and the worker gets the
full width — **the same formulas work either way**, with no special case.

> [!WARNING]
> **Carry IDs as strings across JSON and JavaScript.** Neither can hold a 64-bit integer exactly.

</details>

---

## 🔌 API

See [`snowid.proto`](api/proto/snowid/v1/snowid.proto). The standard gRPC **health check** and
**server reflection** are both registered, so `grpcurl` works without the `.proto` on hand.

| RPC | Description |
| :--- | :--- |
| `Next(count)` | Returns `count` IDs, ascending. Capped at **1000**; more is `INVALID_ARGUMENT` |
| `GetLayout()` | Returns the epoch and segment widths, so clients can **decode locally** |

<details>
<summary><b>📘 The full Go client API</b></summary>

<br>

| Method | |
| :--- | :--- |
| `New(ctx, target, opts...)` | Dial and fetch the layout; `Close` closes the connection |
| `NewWithConn(ctx, conn)` | Reuse a connection you own; `Close` leaves it open |
| `Next(ctx)` | One ID, one round trip |
| `NextN(ctx, n)` | n IDs. `n` may exceed the server's cap; the call is split |
| `Layout()` | The layout, for decoding locally |
| `Identity()` | Which `(datacenter, worker)` answered |
| `MaxBatch()` | The server's per-call cap |
| `Close()` | Close the connection — unless you lent it with `NewWithConn` |

`New` fetches the layout on connect, so an unreachable server **fails immediately** rather than
at the first `Next`.

The default dial options are **plaintext and round-robin** (what you want behind the headless
Service in `deploy/k8s`). Any `grpc.DialOption` you pass is applied **after** those, so supplying
TLS simply overrides the default.

**The client deliberately does not buffer.** Every `Next` is one RPC. A buffer would have to
decide: what happens when the server dies? Does a closed client keep handing out what it queued?
What about IDs whose timestamp went stale sitting there? Getting any of those wrong costs more
than the round trips it saves — we tried, and it cost four bugs: a process-killing panic, a
permanent hang, and two silent failures.

</details>

---

<a id="deploy"></a>

## 🎯 Deploying — keeping IDs unique across replicas

It comes down to one thing: **every pod must hold a different `worker-id`.**

A StatefulSet already gives you something unique and stable across restarts — the **pod ordinal**.
And the StatefulSet controller writes it into the label `apps.kubernetes.io/pod-index`, which
**a Deployment does not set**. Read it with the downward API — **no parsing code in the server at
all**:

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

Kubernetes guarantees **at most one pod holds a given ordinal at any moment** — a rolling update
terminates `snowid-1` before recreating it **as `snowid-1`**, never overlapping.

> [!TIP]
> **Run the same spec as a Deployment and it fails closed.**
> No pod-index label → the downward API yields `""` → the server **refuses to start**
> (CrashLoop) rather than invent an ID. That is exactly the outcome you want.

<details>
<summary><b>☠️ Why you must never parse the hostname</b></summary>

<br>

A Deployment's pod name (`snowid-7d4b9c5f8-84272`) ends in a random suffix, so surely no ordinal
can be read from it — **wrong.**

Kubernetes draws that suffix from the alphabet `bcdfghjklmnpqrstvwxz2456789`, which **contains
seven digits**. So roughly **1 pod in 850** ends in an all-digit segment that parses as a
perfectly valid ordinal:

```
PodOrdinal("snowid-7d4b9c5f8-84272") = 84272   ← read as an ordinal!
```

The server then starts happily with a **random ID that changes on every restart** → **duplicate
IDs**.

**A StatefulSet cannot be told from a Deployment by the shape of a string.** We shipped that bug,
and a test caught it. The `pod-index` label is exact, unambiguous, and fails closed.

</details>

<details>
<summary><b>🌍 Multiple datacenters</b></summary>

<br>

The **datacenter ID is a property of the cluster, not the pod** — every replica in a cluster
shares it. So it comes from wherever you keep per-cluster config, a ConfigMap say:

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
> Needs **Kubernetes 1.28+** (the `pod-index` label is beta and on by default there, GA in 1.32).
> On older clusters, set `--worker-id` yourself — one StatefulSet per ID.
>
> **One replica already serves 4,096,000 IDs/second**, so replicas are about **resilience**, not
> throughput.

---

## 📦 Image

Published to the GitHub Container Registry for **both `linux/amd64` and `linux/arm64`** —
`docker pull` picks the right one.

```bash
docker pull ghcr.io/itmisx/snowid-server:v0.1.0
```

The image is **distroless**: one static binary, no shell, no package manager, running non-root as
UID 65532.

> [!CAUTION]
> **Pin a tag or a digest in your manifests. Do not use `:latest`.**
> Two replicas that pull `:latest` at different times can end up on **different binaries**, and
> the ID layout is **permanent** — every replica must agree on it forever. `:latest` exists for
> `docker run` convenience and nothing else.

Not running containers? [Releases](https://github.com/itmisx/snowid-server/releases) carry static
binaries for linux and macOS on amd64 and arm64.

<details>
<summary><b>🏷️ Releasing</b></summary>

<br>

Push a tag. **No secrets to configure** — it uses the built-in `GITHUB_TOKEN`:

```bash
git tag v0.1.0
git push origin v0.1.0
```

[`release.yml`](.github/workflows/release.yml) then:

1. Builds `linux/amd64` and `linux/arm64` with buildx. Go **cross-compiles**; QEMU never runs the
   compiler, which is an order of magnitude faster.
2. Pushes to `ghcr.io` as **`v0.1.0`** — spelled exactly as the git tag — and `latest`.
3. **Pulls the image back down and actually starts it** — building is not working.
4. Cuts a GitHub Release with binaries for four platforms and a `checksums.txt`.

An ordinary `git push` does **not** build the image; it only runs the tests (~20s).

> [!NOTE]
> **The image is private after the first publish.** GitHub does not have it inherit the
> repository's visibility. Flip it once under
> `Packages → snowid-server → Package settings → Change visibility`; every later version
> inherits that.

</details>

---

## 🛠️ Development

```bash
make test     # go test -race
make lint     # gofmt + go vet
make build
make run      # go run ./cmd/snowid-server --worker-id 0
./buf.gen.sh  # regenerate stubs after editing the .proto
```

<details>
<summary><b>🤔 When NOT to use this service</b></summary>

<br>

**If you only generate IDs inside a single process, do not run this** — just
`import "github.com/bwmarrin/snowflake"` directly.

One less network hop, one less thing to break. The only reason this service exists is to move the
"every process must hold a unique identity" constraint into **one place**, so that everything else
can stay stateless. A single process does not have that problem.

</details>

---

<div align="center">

**MIT** · [LICENSE](LICENSE)

</div>
