# snowid-server

[中文](README.md) | **English**

A gRPC service that wraps [`bwmarrin/snowflake`](https://github.com/bwmarrin/snowflake)
and hands out unique, roughly time-ordered 64-bit IDs. **That is all it does.**

No external infrastructure — no ZooKeeper, no Redis, no database.

```console
$ snowid-server --worker-id 0
INFO generator ready worker_id=0 epoch=2020-01-01T00:00:00Z worker_bits=10 step_bits=12 \
     max_workers=1024 max_ids_per_second=4096000 ids_valid_until=2089-09-06T15:47:35Z
INFO serving addr=:50051

$ grpcurl -plaintext -d '{"count": 3}' localhost:50051 snowid.v1.SnowId/Next
{ "ids": ["864842698333356032", "864842698333356033", "864842698333356034"] }
```

## Why you might want this

A snowflake ID is a 64-bit integer, so it is half the size of a UUID, sorts by
creation time, and is cheap as a primary key — but generating one requires each
process to hold **an identity no other live process holds**. That constraint leaks
into every service that generates IDs in-process: they must all be StatefulSets, or
all coordinate through Redis, or all be a single replica.

Pulling ID generation into one service moves the constraint to **one place**.
Everything else calls `Next` and stays stateless.

If you only generate IDs **inside a single process**, do not run this service — use
`bwmarrin/snowflake` directly. One less network hop, one less thing to break.

## ID layout

The segments are Twitter's original snowflake, and so are their names:

```
| unused(1) | timestamp(41) | datacenter(0) | worker(10) | step(12) |
```

| Segment | Default width | Meaning |
| --- | --- | --- |
| timestamp | 41 bits | milliseconds since the epoch — ~69 years |
| **datacenter** | **0 bits** | which datacenter — off by default |
| **worker** | **10 bits** | which machine inside that datacenter — up to 1024 |
| step | 12 bits | position within the millisecond — 4096 IDs/ms/worker |

The top bit stays zero so IDs are positive in languages without unsigned integers.
The step segment is fixed at bwmarrin's 12 bits — **4,096,000 IDs/second/worker**.
(Twitter called this segment the `sequence`; bwmarrin calls it the `step`, and since
it is bwmarrin doing the counting, so do we.)

**The `(datacenter, worker)` pair is a process's identity.** With datacenters off,
all 10 bits belong to the worker.

> **The segment widths and the epoch are permanent.** Every ID you have ever issued
> is decoded relative to them. Choose them before the first ID and **never change
> them** — changing any of them makes every existing ID decode to the wrong time and
> the wrong origin.

## Configuration

| Flag | Environment | Default | Meaning |
| --- | --- | --- | --- |
| `--worker-id` | `SNOWID_WORKER_ID` | **required** | This machine's ID within its datacenter |
| `--datacenter-id` | `SNOWID_DATACENTER_ID` | `0` | This datacenter's ID |
| `--worker-bits` | | `10` | Width of the worker segment. `10` bits = up to 1024 workers per datacenter |
| `--datacenter-bits` | | `0` | Width of the datacenter segment. `0` = no datacenters |
| `--epoch` | | `1577836800000` | Zero point of the timestamp, in **unix milliseconds** (2020-01-01 UTC) |
| `--addr` | | `:50051` | gRPC listen address |

**The one rule: no two live processes may hold the same `(datacenter, worker)` pair.**
So the server **will not guess** `--worker-id` — leave it out and it refuses to start:

```console
$ snowid-server
ERROR startup failed error="--worker-id is required: two live processes sharing an identity issue the same ids"
```

### The bit budget: add datacenter bits, take them off the worker

There are 64 bits and no more. The top one stays 0, so 63 are yours to divide, and
the step segment takes a fixed 12. The datacenter, the worker and **the timestamp are
all competing for the same remainder**:

```
timestamp_bits = 63 - datacenter_bits - worker_bits - 12
```

**The two width flags ADD; the datacenter is not carved out of the worker.** So adding
`--datacenter-bits` without taking the same off `--worker-bits` steals the bits from
the *timestamp*, and the ID layout's lifespan falls off a cliff:

| `--datacenter-bits` | `--worker-bits` | Identities | Timestamp bits | Lasts |
| --- | --- | --- | --- | --- |
| `0` (default) | `10` | 1024 | 41 | **69.7 years** ✅ |
| `5` | `5` | 32 × 32 = 1024 | 41 | **69.7 years** ✅ (Twitter's) |
| `3` | `7` | 8 × 128 = 1024 | 41 | **69.7 years** ✅ |
| `5` | **`10`** ← forgot | 32 × 1024 = 32768 | 36 | **2.2 years** ❌ already overflowed |

That last row is **refused at startup**. You do not get to run a layout that expired
two years after its own epoch:

```console
$ snowid-server --datacenter-bits 5 --worker-id 2 --datacenter-id 1
ERROR startup failed error="--epoch=1577836800000 is too far in the past:
      36 bits of timestamp hold only 19088h44m36.736s, and 57277h6m24.432s have passed since it"
```

### Datacenters

Off by default. Turn them on and you get Twitter's original 5/5 split — note that
`--worker-bits` comes **down** from 10 to 5:

```console
$ snowid-server --datacenter-bits 5 --worker-bits 5 --datacenter-id 1 --worker-id 2
INFO generator ready worker_id=2 worker_bits=5 step_bits=12 max_workers=32 \
     datacenter_id=1 datacenter_bits=5 max_datacenters=32 ...
```

**32 datacenters × 32 workers each = 1024 identities** — the same total as the default,
with the timestamp still on 41 bits. You have **re-divided** those 10 identity bits, not
asked for more.

**Underneath, the two IDs are CONCATENATED, not added:**

```go
identity = (datacenter_id << worker_bits) | worker_id
```

> ⚠️ **Never add them.** `datacenter=1,worker=2` and `datacenter=2,worker=1` both
> **add to 3** — two different processes would get **the same identity**, and every ID
> they issued in the same millisecond would be a **duplicate**. Concatenation is what
> makes the *pair* unique. Measured: those two pack to **34** and **65**.

Either ID overflowing its segment would spill into the other's bits and land on
somebody else's identity, so the server refuses:

```console
$ snowid-server --datacenter-bits 5 --worker-bits 5 --datacenter-id 1 --worker-id 32
ERROR startup failed error="--worker-id=32 is out of range [0,31] for --worker-bits=5"
```

### Why startup validation exists

bwmarrin's `Generate()` **returns no error and does no bounds check** — it shifts the
timestamp into place and hands you the result. A bad layout therefore **does not
fail**. It **silently** emits IDs whose time is wrong, whose sign bit is set (negative
IDs), and which **repeat** once the segment wraps.

For example: `--worker-bits=19`, plus the fixed 12 step bits, leaves 32 bits of
timestamp = **49.7 days** — against an epoch in 2020. That layout overflowed years
ago. So it has to be caught at startup, or never:

```console
$ snowid-server --worker-id 0 --worker-bits 19
ERROR startup failed error="--epoch=1577836800000 is too far in the past:
      32 bits of timestamp hold only 1193h2m47.296s, and 57276h16m11.412s have passed since it"
```

The `ids_valid_until` line in the startup log says how long the layout you chose lasts.

## API

See [`api/proto/snowid/v1/snowid.proto`](api/proto/snowid/v1/snowid.proto).

| RPC | Description |
| --- | --- |
| `Next(count)` | Returns `count` IDs, ascending. Capped at 1000; more is `INVALID_ARGUMENT` |
| `GetLayout()` | Returns the epoch and segment widths, so clients can **decode IDs locally** |

The standard gRPC health check and server reflection are both registered.

## Go client

```go
import "github.com/itmisx/snowid-server/pkg/client"

c, err := client.New(ctx, "dns:///snowid:50051")
defer c.Close()

id, err := c.Next(ctx)                  // one RPC, one ID
ids, err := c.NextN(ctx, 500)           // one RPC, 500 IDs

// Decoding happens here, not over the network.
fmt.Println(c.Layout().Time(id))          // when it was made
fmt.Println(c.Layout().Datacenter(id))    // which datacenter made it
fmt.Println(c.Layout().Worker(id))        // and which machine inside it
```

| Method | |
| --- | --- |
| `New(ctx, target, opts...)` | Dial and fetch the layout; `Close` closes the connection |
| `NewWithConn(ctx, conn)` | Reuse a connection you own; `Close` leaves it open |
| `Next(ctx)` | One ID, one round trip |
| `NextN(ctx, n)` | n IDs. `n` may exceed the server's cap; the call is split |
| `Layout()` | For decoding locally |
| `Identity()` / `MaxBatch()` | Which `(datacenter, worker)` answered; the server's per-call cap |

`New` fetches the layout on connect, so an unreachable server **fails immediately**
rather than at the first `Next`. The defaults are plaintext and round-robin (what you
want behind the headless Service in `deploy/k8s`); any `grpc.DialOption` you pass is
applied **after** those, so supplying TLS simply overrides the default.

**The client does not buffer, deliberately.** Every `Next` is one RPC. If you want the
feel of one ID at a time, take a batch with `NextN(ctx, 500)` and hand them out
yourself — **that is twenty lines.** A buffer inside the client has to decide what to
do about a dead server, a closed client, and IDs whose timestamp has gone stale sitting
in the queue, and getting any of those wrong costs more than the round trips it saves.
(We tried. Four bugs: a process-killing panic, a permanent hang, and two silent
failures.)

## Decoding IDs

**Decode locally, never over the network.** Decoding is a few bit operations; a round
trip to read an ID's timestamp is pure waste. Fetch the layout once with `GetLayout`,
then:

```
unix_milli = (id >> (datacenter_bits + worker_bits + step_bits)) + epoch_unix_milli
datacenter = (id >> (worker_bits + step_bits)) & ((1 << datacenter_bits) - 1)
worker     = (id >> step_bits) & ((1 << worker_bits) - 1)
step       =  id & ((1 << step_bits) - 1)
```

With datacenters off, `datacenter_bits` is 0, so `datacenter` comes out 0 and the
worker gets the full width — **the same formulas work either way**, with no special
case.

**Carry IDs as strings across JSON and JavaScript.** Neither can hold a 64-bit integer
exactly.

## Deploying: keeping IDs unique across replicas

See [`deploy/k8s`](deploy/k8s). It comes down to one thing: **every pod must hold a
different worker ID.**

A StatefulSet already gives you something unique and stable across restarts — the
**pod ordinal**. And the StatefulSet controller writes it into a label
(`apps.kubernetes.io/pod-index`) that **a Deployment does not set**. Read it straight
out with the downward API — **no parsing code in the server at all**:

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

Kubernetes guarantees at most one pod holds a given ordinal at any moment — a rolling
update terminates `snowid-1` before recreating it **as `snowid-1`**, never overlapping.
So the worker ID is unique and stable across restarts, **with no external coordination
at all**.

Run the same spec as a **Deployment and it fails closed**: no pod-index label, so the
downward API yields `""`, and the server **refuses to start** (CrashLoop) rather than
invent an ID.

> **Never parse the hostname.** A Deployment's pod name (`snowid-7d4b9c5f8-84272`) ends
> in a random suffix drawn from an alphabet that **contains digits** — roughly **1 pod
> in 850** ends in an all-digit segment that reads exactly like an ordinal. The server
> would start happily with a **random ID that changes on every restart** → duplicate
> IDs. **A StatefulSet cannot be told from a Deployment by the shape of a string.**

The **datacenter ID is a property of the cluster, not the pod**, so it comes from
wherever you keep per-cluster config (a ConfigMap, say). One value per cluster; every
replica in it shares it.

Needs Kubernetes **1.28+** (the pod-index label is beta and on by default there, GA in
1.32). On older clusters, set `--worker-id` yourself — one StatefulSet per ID.

**One replica already serves 4,096,000 IDs/second**, so replicas are about resilience,
not throughput.

## Docker image

Published to the GitHub Container Registry for **both `linux/amd64` and
`linux/arm64`** — `docker pull` picks the right one for you.

```console
$ docker run --rm -p 50051:50051 ghcr.io/itmisx/snowid-server:v0.1.0 --worker-id 0
```

The image is distroless: one static binary, no shell, no package manager, running
non-root as UID 65532.

**Pin a tag or a digest in your manifests; do not use `:latest`.** Two replicas
that pull `:latest` at different times can end up on different binaries, and the ID
layout is **permanent** — every replica must agree on it forever. `:latest` exists
for `docker run` convenience and nothing else.

Not running containers? [Releases](https://github.com/itmisx/snowid-server/releases)
carry static binaries for linux and macOS on amd64 and arm64.

## Releasing

Push a tag. No secrets to configure — it uses the built-in `GITHUB_TOKEN`:

```bash
git tag v0.1.0
git push origin v0.1.0
```

[`release.yml`](.github/workflows/release.yml) then, in order:

1. **Runs the tests first** — a tag can never point at a tree that was not green.
2. Builds `linux/amd64` and `linux/arm64` with buildx. Go **cross-compiles**; QEMU
   never runs the compiler, which is an order of magnitude faster.
3. Pushes to `ghcr.io/itmisx/snowid-server` as `v0.1.0`, `0.1`, `0`, `latest` and
   `sha-<commit>`.
4. **Pulls both architectures back down and actually starts them.** Building is not
   working: the `USER nonroot` bug built perfectly and could never start a pod.
5. Attaches an SBOM and build provenance, verifiable with `gh attestation verify`.
6. **Only then** cuts the GitHub Release, with binaries for four platforms and a
   `checksums.txt`. A release pointing at an image nobody can pull is worse than no
   release at all.

To check that a Dockerfile change still builds for both architectures, without
tagging anything:

```bash
make docker-multiarch
```

> **The image is private after the first publish.** GitHub does not have it inherit
> the repository's visibility. Flip it once under
> `Packages → snowid-server → Package settings → Change visibility`; every later
> version inherits that.

## Development

```bash
make test                     # go test -race
make lint                     # gofmt + go vet
make build
make run                      # go run ./cmd/snowid-server --worker-id 0
./buf.gen.sh                  # regenerate stubs after editing the .proto
```

## License

MIT — see [LICENSE](LICENSE).
