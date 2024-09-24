# Benchmark

A simple benchmarking tool to generate easy to reproduce benchmarks for the Sia ecosystem. Each benchmark gets a distinct testnet using [cluster](https://github.com/SiaFoundation/cluster). The benchmarks are designed to be run on a machine with a large number of cores to support the number of concurrent processes.

We will add additional benchmarks specifically for single host/renter interactions using RHP3 as a baseline for testing regressions.

The results published in this repository and the code to reproduce the benchmarks are under the MIT license.

## Benchmarks

### End-to-End

The end-to-end benchmark is designed to test the performance of a renterd node. It forms contracts with a number of hosts and tests the speed of encrypting, erasure-coding, uploading shards, downloading, and reconstruction in ideal production-like conditions. The benchmark uploads and downloads a 320MiB test file and reports speeds.

*While the test data is 320MiB, the actual data uploaded is 960MiB due to 10-of-30 erasure-coding overhead. It would therefore be technically accurate to multiply the upload data size by three. However, the desire is to show the speed based on the file size a user uploads, not the redundant shards sent to hosts.*

*320 MiB was chosen since it aligns the object size with the erasure-coded slab size using the default erasure coding parameters*

[Results](results/e2e.csv)

## Usage

```
./benchmark -hosts=50 -output=results
```
