# Benchmark

A simple benchmarking tool to generate easy to reproduce benchmarks for the Sia ecosystem. Each benchmark gets a distinct testnet using [cluster](https://github.com/SiaFoundation/cluster). The benchmarks are designed to be run on a machine with a large number of cores to support the number of concurrent processes. These benchmarks are intended to track performance over time and identify regressions.

The results published in this repository and the code to reproduce the benchmarks are released under the MIT license.

## Benchmarks

### End-to-End

The end-to-end benchmark is designed to test the performance of a renterd node. It forms contracts with a number of hosts and tests the speed of encrypting, erasure-coding, uploading shards, downloading, and reconstruction in ideal production-like conditions. The benchmark uploads and downloads a 320MiB test file and reports speeds.

*While the test data is 320MiB, the actual data uploaded is 960MiB due to 10-of-30 erasure-coding overhead. It would therefore be technically accurate to multiply the upload data size by three. However, the desire is to show the speed based on the file size a user uploads, not the redundant shards sent to hosts.*

*320 MiB was chosen since it aligns the object size with the erasure-coded slab size using the default erasure coding parameters*

[Results](results/e2e.csv)

### RHP2

The RHP2 benchmark is designed to test the performance of the RHP2 implementation in `hostd`. It forms a contract with a single host and tests the speed of uploading and downloading randomly generated sectors. The benchmark uploads and downloads 256 sectors, or 1GiB of data. To simulate production scenarios, the benchmark appends a single sector at a time instead of batching the appends into a single RPC. This will result in slightly slower speeds, but is more representative of how RHP2 is used in production.

[Results](results/rhp2.csv)

### RHP3

The RHP3 benchmark is designed to test the performance of the RHP3 implementation in `hostd`. It forms a contract with a single host and tests the speed of uploading and downloading randomly generated sectors. The benchmark uploads and downloads 256 sectors, or 1GiB of data. To simulate production scenarios, the benchmark appends a single sector at a time instead of batching the appends into a single MDM program. This will result in slightly slower speeds, but is more representative of how RHP3 is used in production.

[Results](results/rhp3.csv)

### RHP4

The RHP4 benchmark is designed to test the performance of the RHP4 implementation in `hostd`. It forms a contract with a single host and tests the speed of uploading and downloading randomly generated sectors. The benchmark uploads and downloads 256 sectors, or 1GiB of data.

*RHP4 is still in development and not available on Mainnet yet*

[Results](results/rhp4.csv)

## Usage

```
./benchmark -hosts=50 -output=results
```
