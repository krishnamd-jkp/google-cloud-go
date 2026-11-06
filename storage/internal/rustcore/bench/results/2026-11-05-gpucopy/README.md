# gpucopy runs, 2026-11-05

Raw data of the GPU direct-copy test in [GPU_DIRECT_COPY.md](../../GPU_DIRECT_COPY.md). Each
`.jsonl` line is one run. `cpu_s_per_gib` is the benchmark process's CPU seconds per GiB read.
The modes map to the report's names as follows: `host` = heap, `pinned` = direct,
`pageable` = heap + copy.

| File | What |
|---|---|
| `read.jsonl`, `run.log` | The measured runs: 2 engines × 2 transports × 3 modes × 3 rounds, 48 workers, 14:09–14:48 UTC. |
| `verify.jsonl` | The verification runs of that pass: every range read back from the device and compared. |
| `copycost.jsonl` | Hand-off costs on one thread, with no network: a `memcpy`, and a Unix-socket hop. |
| `gc-check.jsonl`, `gc-check.log` | The GC check: heap vs direct vs direct with `GOGC=off GOMEMLIMIT=2GiB`, 14:53–15:01 UTC. |
| `verify-pass1.jsonl` | The verification runs of an earlier pass with one connection each, 12:54 UTC. |

All runs used `--device=sim`. No GPU was used.

Binaries, all built with `-tags gcsrust` from [`cmd/gpucopy`](../../../cmd/gpucopy/main.go):

- **Main pass:** sha256 `d972c1545a336f74ea1dd0fcfcd073b1970f84f22b425c29cbff2f1f9185a0c5`. This is the source before the GC counters were added.
- **GC check:** sha256 `4b5ba77255492e56ebd9a8a55779f150f3ff9cd4ebbc2c028027eb277597c68c`.
- **Earlier pass:** sha256 `bff79dba34743dd297bd9e08b809ccceab9261965705f1b1e1a9f7222eadaff6`. This build predates `--grpc-conns` and `--json-http1`.

One run of the main pass, for example:

```sh
gpucopy --engine=rust --transport=bidi --grpc-conns=48 --mode=pinned --device=sim \
  --bucket=<bucket> --workers=48 --warmup=10s --duration=45s
```

For JSON, replace `--grpc-conns=48` with `--json-http1`.
