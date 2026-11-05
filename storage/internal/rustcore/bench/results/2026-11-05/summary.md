## LIST_BUCKETS

threads=1, measured=180 s per run, machine=c4-standard-192 (192 vCPU), zone=us-central1-f, bucket=-

Median over rounds (min..max in brackets for the first column).

| config | rounds | errors | items/s | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| go-default | 1 | 0 | 110 | 1.10 | 911.0 | 944.6 | 967.5 | 0.0 | 51 | 0.1 | 0.7 | 1 | 0.00 | 0.00 |
| rust-default | 1 | 0 | 110 | 1.10 | 910.8 | 947.6 | 999.7 | 0.0 | 62 | 0.1 | 0.7 | 1 | 0.00 | 0.00 |

<details><summary>Every run</summary>

| config | run | ops | errors | items/s | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps | first error |
|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|
| go-default | list-buckets-bidi-x-go-default-2026-11-05T08-27-41.947340841Z | 198 | 0 | 110 | 1.10 | 911.0 | 944.6 | 967.5 | 0.0 | 51 | 0.1 | 0.7 | 1 | 0.00 | 0.00 |  |
| rust-default | list-buckets-bidi-x-rust-default-2026-11-05T08-23-31.11255918Z | 198 | 0 | 110 | 1.10 | 910.8 | 947.6 | 999.7 | 0.0 | 62 | 0.1 | 0.7 | 1 | 0.00 | 0.00 |  |

</details>

## LIST_OBJECTS

threads=1, measured=180 s per run, machine=c4-standard-192 (192 vCPU), zone=us-central1-f, bucket=regional

Median over rounds (min..max in brackets for the first column).

| config | rounds | errors | items/s | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| go-default | 1 | 0 | 1,706 | 17.05 | 57.8 | 64.0 | 76.2 | 0.0 | 53 | 0.1 | 0.7 | 1 | 0.00 | 0.00 |
| rust-default | 1 | 0 | 1,687 | 16.87 | 58.4 | 64.9 | 77.0 | 0.0 | 63 | 0.1 | 0.7 | 1 | 0.00 | 0.00 |

<details><summary>Every run</summary>

| config | run | ops | errors | items/s | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps | first error |
|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|
| go-default | list-objects-bidi-x-go-default-2026-11-05T08-15-10.432050584Z | 3,070 | 0 | 1,706 | 17.05 | 57.8 | 64.0 | 76.2 | 0.0 | 53 | 0.1 | 0.7 | 1 | 0.00 | 0.00 |  |
| rust-default | list-objects-bidi-x-rust-default-2026-11-05T08-19-20.750827379Z | 3,036 | 0 | 1,687 | 16.87 | 58.4 | 64.9 | 77.0 | 0.0 | 63 | 0.1 | 0.7 | 1 | 0.00 | 0.00 |  |

</details>

## OPEN_100KIB

threads=1, measured=180 s per run, machine=c4-standard-192 (192 vCPU), zone=us-central1-f, bucket=regional

Median over rounds (min..max in brackets for the first column).

| config | rounds | errors | items/s | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| go-default | 1 | 0 | 14 | 13.97 | 69.6 | 79.6 | 117.7 | 0.0 | 52 | 0.1 | 0.7 | 1 | 0.01 | 0.00 |
| rust-default | 1 | 0 | 14 | 13.87 | 70.2 | 79.5 | 121.8 | 0.0 | 58 | 0.1 | 0.7 | 2 | 0.01 | 0.00 |

<details><summary>Every run</summary>

| config | run | ops | errors | items/s | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps | first error |
|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|
| go-default | open-bidi-102400-go-default-2026-11-05T06-27-22.678071466Z | 2,515 | 0 | 14 | 13.97 | 69.6 | 79.6 | 117.7 | 0.0 | 52 | 0.1 | 0.7 | 1 | 0.01 | 0.00 |  |
| rust-default | open-bidi-102400-rust-default-2026-11-05T06-23-12.294556769Z | 2,496 | 0 | 14 | 13.87 | 70.2 | 79.5 | 121.8 | 0.0 | 58 | 0.1 | 0.7 | 2 | 0.01 | 0.00 |  |

</details>

## OPEN

threads=1, measured=180 s per run, machine=c4-standard-192 (192 vCPU), zone=us-central1-f, bucket=regional

Median over rounds (min..max in brackets for the first column).

| config | rounds | errors | items/s | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| go-default | 1 | 0 | 3 | 2.77 | 333.6 | 466.5 | 502.2 | 0.0 | 67 | 0.1 | 0.7 | 1 | 0.18 | 0.00 |
| rust-default | 1 | 0 | 7 | 6.75 | 129.2 | 254.4 | 295.5 | 0.0 | 67 | 0.1 | 0.7 | 2 | 0.12 | 0.00 |

<details><summary>Every run</summary>

| config | run | ops | errors | items/s | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps | first error |
|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|
| go-default | open-bidi-104857600-go-default-2026-11-05T06-14-51.500787888Z | 498 | 0 | 3 | 2.77 | 333.6 | 466.5 | 502.2 | 0.0 | 67 | 0.1 | 0.7 | 1 | 0.18 | 0.00 |  |
| rust-default | open-bidi-104857600-rust-default-2026-11-05T06-19-01.798213464Z | 1,216 | 0 | 7 | 6.75 | 129.2 | 254.4 | 295.5 | 0.0 | 67 | 0.1 | 0.7 | 2 | 0.12 | 0.00 |  |

</details>

## RANDOM8K_BIDI

threads=1, measured=180 s per run, machine=c4-standard-192 (192 vCPU), zone=us-central1-f, bucket=regional

Median over rounds (min..max in brackets for the first column).

| config | rounds | errors | MB/s | Gbps | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| go-default | 1 | 0 | 0.1 | 0.00 | 15.96 | 60.1 | 70.9 | 111.6 | 0.0 | 51 | 0.1 | 0.7 | 1 | 0.00 | 0.00 |
| rust-default | 1 | 0 | 0.1 | 0.00 | 16.12 | 60.3 | 69.0 | 101.5 | 0.0 | 59 | 0.1 | 0.7 | 2 | 0.00 | 0.00 |

<details><summary>Every run</summary>

| config | run | ops | errors | MB/s | Gbps | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps | first error |
|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|
| go-default | random8k-bidi-x-go-default-2026-11-05T07-05-06.525536133Z | 2,872 | 0 | 0.1 | 0.00 | 15.96 | 60.1 | 70.9 | 111.6 | 0.0 | 51 | 0.1 | 0.7 | 1 | 0.00 | 0.00 |  |
| rust-default | random8k-bidi-x-rust-default-2026-11-05T07-09-16.834919369Z | 2,903 | 0 | 0.1 | 0.00 | 16.12 | 60.3 | 69.0 | 101.5 | 0.0 | 59 | 0.1 | 0.7 | 2 | 0.00 | 0.00 |  |

</details>

## RANDOM8K_JSON

threads=1, measured=180 s per run, machine=c4-standard-192 (192 vCPU), zone=us-central1-f, bucket=regional

Median over rounds (min..max in brackets for the first column).

| config | rounds | errors | MB/s | Gbps | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| go-default | 1 | 0 | 0.1 | 0.00 | 16.84 | 57.6 | 65.5 | 92.8 | 0.0 | 50 | 0.1 | 0.7 | 1 | 0.00 | 0.00 |
| rust-default | 1 | 0 | 0.1 | 0.00 | 16.44 | 58.9 | 67.2 | 97.6 | 0.0 | 58 | 0.1 | 0.7 | 2 | 0.00 | 0.00 |

<details><summary>Every run</summary>

| config | run | ops | errors | MB/s | Gbps | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps | first error |
|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|
| go-default | random8k-json-x-go-default-2026-11-05T07-17-37.72986533Z | 3,031 | 0 | 0.1 | 0.00 | 16.84 | 57.6 | 65.5 | 92.8 | 0.0 | 50 | 0.1 | 0.7 | 1 | 0.00 | 0.00 |  |
| rust-default | random8k-json-x-rust-default-2026-11-05T07-13-27.341453341Z | 2,960 | 0 | 0.1 | 0.00 | 16.44 | 58.9 | 67.2 | 97.6 | 0.0 | 58 | 0.1 | 0.7 | 2 | 0.00 | 0.00 |  |

</details>

## BIDI_READ

threads=48, measured=300 s per run, machine=c4-standard-192 (192 vCPU), zone=us-central1-f, bucket=regional

Median over rounds (min..max in brackets for the first column).

| config | rounds | errors | MB/s | Gbps | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| go-default | 1 | 0 | 201.3 | 1.61 | 12.00 | 3,952.1 | 4,668.6 | 5,029.3 | 0.4 | 1,215 | 0.7 | 0.9 | 1 | 1.71 | 0.01 |
| rust-default | 1 | 0 | 296.6 | 2.37 | 17.68 | 2,692.9 | 3,007.5 | 3,917.7 | 0.4 | 1,670 | 0.7 | 1.0 | 2 | 2.51 | 0.01 |

<details><summary>Every run</summary>

| config | run | ops | errors | MB/s | Gbps | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps | first error |
|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|
| go-default | read-bidi-x-go-default-2026-11-05T07-21-47.884581985Z | 3,600 | 0 | 201.3 | 1.61 | 12.00 | 3,952.1 | 4,668.6 | 5,029.3 | 0.4 | 1,215 | 0.7 | 0.9 | 1 | 1.71 | 0.01 |  |
| rust-default | read-bidi-x-rust-default-2026-11-05T07-28-20.658550737Z | 5,304 | 0 | 296.6 | 2.37 | 17.68 | 2,692.9 | 3,007.5 | 3,917.7 | 0.4 | 1,670 | 0.7 | 1.0 | 2 | 2.51 | 0.01 |  |

</details>

## READ_JSON

threads=48, measured=300 s per run, machine=c4-standard-192 (192 vCPU), zone=us-central1-f, bucket=regional

Median over rounds (min..max in brackets for the first column).

| config | rounds | errors | MB/s | Gbps | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| go-default | 1 | 0 | 301.5 | 2.41 | 17.97 | 2,637.8 | 3,033.6 | 3,389.0 | 0.8 | 1,165 | 1.1 | 0.9 | 1 | 2.57 | 0.03 |
| rust-default | 1 | 0 | 2,912.2 | 23.30 | 173.58 | 265.9 | 312.9 | 478.1 | 3.0 | 1,130 | 4.9 | 0.9 | 49 | 24.68 | 0.14 |

<details><summary>Every run</summary>

| config | run | ops | errors | MB/s | Gbps | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps | first error |
|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|
| go-default | read-json-x-go-default-2026-11-05T07-41-23.383342953Z | 5,391 | 0 | 301.5 | 2.41 | 17.97 | 2,637.8 | 3,033.6 | 3,389.0 | 0.8 | 1,165 | 1.1 | 0.9 | 1 | 2.57 | 0.03 |  |
| rust-default | read-json-x-rust-default-2026-11-05T07-34-52.659268848Z | 52,074 | 0 | 2,912.2 | 23.30 | 173.58 | 265.9 | 312.9 | 478.1 | 3.0 | 1,130 | 4.9 | 0.9 | 49 | 24.68 | 0.14 |  |

</details>

## READ50X2X4_BIDI_CONCURRENT

threads=1, measured=180 s per run, machine=c4-standard-192 (192 vCPU), zone=us-central1-f, bucket=regional

Median over rounds (min..max in brackets for the first column).

| config | rounds | errors | MB/s | Gbps | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| go-default | 1 | 0 | 142.1 | 1.14 | 1.36 | 725.5 | 845.4 | 926.2 | 0.3 | 649 | 0.5 | 0.8 | 1 | 1.20 | 0.01 |
| rust-default | 1 | 0 | 181.7 | 1.45 | 1.73 | 571.8 | 638.1 | 716.5 | 0.3 | 1,109 | 0.4 | 0.9 | 2 | 1.54 | 0.01 |

<details><summary>Every run</summary>

| config | run | ops | errors | MB/s | Gbps | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps | first error |
|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|
| go-default | read50x2x4-bidi-concurrent-go-default-2026-11-05T06-31-32.971223996Z | 244 | 0 | 142.1 | 1.14 | 1.36 | 725.5 | 845.4 | 926.2 | 0.3 | 649 | 0.5 | 0.8 | 1 | 1.20 | 0.01 |  |
| rust-default | read50x2x4-bidi-concurrent-rust-default-2026-11-05T06-35-43.909023509Z | 312 | 0 | 181.7 | 1.45 | 1.73 | 571.8 | 638.1 | 716.5 | 0.3 | 1,109 | 0.4 | 0.9 | 2 | 1.54 | 0.01 |  |

</details>

## READ50X2X4_BIDI_SERIAL

threads=1, measured=180 s per run, machine=c4-standard-192 (192 vCPU), zone=us-central1-f, bucket=regional

Median over rounds (min..max in brackets for the first column).

| config | rounds | errors | MB/s | Gbps | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| go-default | 1 | 0 | 16.9 | 0.14 | 0.16 | 5,731.6 | 6,848.1 | 13,463.5 | 0.0 | 190 | 0.1 | 0.7 | 1 | 0.14 | 0.00 |
| rust-default | 1 | 0 | 17.5 | 0.14 | 0.17 | 5,755.1 | 6,001.1 | 13,728.5 | 0.0 | 167 | 0.1 | 0.7 | 2 | 0.15 | 0.00 |

<details><summary>Every run</summary>

| config | run | ops | errors | MB/s | Gbps | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps | first error |
|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|
| go-default | read50x2x4-bidi-serial-go-default-2026-11-05T06-44-08.610348389Z | 29 | 0 | 16.9 | 0.14 | 0.16 | 5,731.6 | 6,848.1 | 13,463.5 | 0.0 | 190 | 0.1 | 0.7 | 1 | 0.14 | 0.00 |  |
| rust-default | read50x2x4-bidi-serial-rust-default-2026-11-05T06-39-54.772212363Z | 30 | 0 | 17.5 | 0.14 | 0.17 | 5,755.1 | 6,001.1 | 13,728.5 | 0.0 | 167 | 0.1 | 0.7 | 2 | 0.15 | 0.00 |  |

</details>

## READ50X2X4_JSON_CONCURRENT

threads=1, measured=180 s per run, machine=c4-standard-192 (192 vCPU), zone=us-central1-f, bucket=regional

Median over rounds (min..max in brackets for the first column).

| config | rounds | errors | MB/s | Gbps | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| go-default | 1 | 0 | 221.9 | 1.78 | 2.12 | 444.9 | 570.0 | 766.8 | 0.5 | 273 | 0.8 | 0.8 | 1 | 1.88 | 0.02 |
| rust-default | 1 | 0 | 448.5 | 3.59 | 4.28 | 183.4 | 353.0 | 658.1 | 0.4 | 456 | 0.7 | 0.8 | 51 | 3.80 | 0.02 |

<details><summary>Every run</summary>

| config | run | ops | errors | MB/s | Gbps | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps | first error |
|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|
| go-default | read50x2x4-json-concurrent-go-default-2026-11-05T06-48-19.093319985Z | 381 | 0 | 221.9 | 1.78 | 2.12 | 444.9 | 570.0 | 766.8 | 0.5 | 273 | 0.8 | 0.8 | 1 | 1.88 | 0.02 |  |
| rust-default | read50x2x4-json-concurrent-rust-default-2026-11-05T06-52-29.464426622Z | 770 | 0 | 448.5 | 3.59 | 4.28 | 183.4 | 353.0 | 658.1 | 0.4 | 456 | 0.7 | 0.8 | 51 | 3.80 | 0.02 |  |

</details>

## READ50X2X4_JSON_SERIAL

threads=1, measured=180 s per run, machine=c4-standard-192 (192 vCPU), zone=us-central1-f, bucket=regional

Median over rounds (min..max in brackets for the first column).

| config | rounds | errors | MB/s | Gbps | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| go-default | 1 | 0 | 13.4 | 0.11 | 0.13 | 7,160.2 | 11,332.0 | 16,178.1 | 0.0 | 170 | 0.1 | 0.7 | 1 | 0.12 | 0.00 |
| rust-default | 1 | 0 | 14.0 | 0.11 | 0.13 | 7,149.9 | 11,201.8 | 15,660.9 | 0.0 | 160 | 0.1 | 0.7 | 2 | 0.12 | 0.00 |

<details><summary>Every run</summary>

| config | run | ops | errors | MB/s | Gbps | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps | first error |
|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|
| go-default | read50x2x4-json-serial-go-default-2026-11-05T07-00-55.256088322Z | 23 | 0 | 13.4 | 0.11 | 0.13 | 7,160.2 | 11,332.0 | 16,178.1 | 0.0 | 170 | 0.1 | 0.7 | 1 | 0.12 | 0.00 |  |
| rust-default | read50x2x4-json-serial-rust-default-2026-11-05T06-56-39.898838122Z | 24 | 0 | 14.0 | 0.11 | 0.13 | 7,149.9 | 11,201.8 | 15,660.9 | 0.0 | 160 | 0.1 | 0.7 | 2 | 0.12 | 0.00 |  |

</details>

## BIDI_APPENDABLE_WRITE

threads=48, measured=300 s per run, machine=c4-standard-192 (192 vCPU), zone=us-central1-f, bucket=zonal

Median over rounds (min..max in brackets for the first column).

| config | rounds | errors | MB/s | Gbps | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| go-default | 1 | 0 | 360.1 | 2.88 | 1.76 | 26,482.8 | 28,510.9 | 28,942.7 | 0.4 | 2,819 | 0.6 | 1.2 | 1 | 0.05 | 2.92 |
| rust-default | 1 | 0 | 379.0 | 3.03 | 1.88 | 27,681.4 | 28,900.2 | 31,422.0 | 0.3 | 21,984 | 0.4 | 3.6 | 1 | 0.01 | 2.99 |

<details><summary>Every run</summary>

| config | run | ops | errors | MB/s | Gbps | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps | first error |
|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|
| go-default | write-bidi-x-go-default-2026-11-05T07-47-54.315621936Z | 528 | 0 | 360.1 | 2.88 | 1.76 | 26,482.8 | 28,510.9 | 28,942.7 | 0.4 | 2,819 | 0.6 | 1.2 | 1 | 0.05 | 2.92 |  |
| rust-default | write-bidi-x-rust-default-2026-11-05T07-54-46.67787535Z | 563 | 0 | 379.0 | 3.03 | 1.88 | 27,681.4 | 28,900.2 | 31,422.0 | 0.3 | 21,984 | 0.4 | 3.6 | 1 | 0.01 | 2.99 |  |

</details>

## WRITE_JSON

threads=48, measured=300 s per run, machine=c4-standard-192 (192 vCPU), zone=us-central1-f, bucket=regional

Median over rounds (min..max in brackets for the first column).

| config | rounds | errors | MB/s | Gbps | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps |
|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|
| go-default | 1 | 0 | 268.6 | 2.15 | 1.44 | 34,869.5 | 42,072.6 | 43,329.6 | 0.7 | 2,052 | 1.0 | 1.0 | 1 | 0.01 | 2.17 |
| rust-default | 1 | 0 | 1,571.2 | 12.57 | 7.82 | 6,019.3 | 6,546.2 | 7,194.8 | 0.5 | 10,343 | 0.8 | 2.1 | 48 | 0.07 | 12.61 |

<details><summary>Every run</summary>

| config | run | ops | errors | MB/s | Gbps | ops/s | p50 ms | p90 ms | p99 ms | proc CPU % | peak RSS MiB | VM CPU % mean | VM mem % peak | TCP :443 | NIC rx Gbps | NIC tx Gbps | first error |
|---|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|
| go-default | write-json-x-go-default-2026-11-05T08-08-11.501525115Z | 432 | 0 | 268.6 | 2.15 | 1.44 | 34,869.5 | 42,072.6 | 43,329.6 | 0.7 | 2,052 | 1.0 | 1.0 | 1 | 0.01 | 2.17 |  |
| rust-default | write-json-x-rust-default-2026-11-05T08-01-29.896878334Z | 2,345 | 0 | 1,571.2 | 12.57 | 7.82 | 6,019.3 | 6,546.2 | 7,194.8 | 0.5 | 10,343 | 0.8 | 2.1 | 48 | 0.07 | 12.61 |  |

</details>

