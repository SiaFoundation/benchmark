# Benchmark

A simple benchmarking tool to generate easy to reproduce benchmarks for the Sia ecosystem. Each benchmark gets a distinct testnet using [cluster](https://github.com/SiaFoundation/cluster). The benchmarks are designed to be run on a machine with a large number of cores to support the number of concurrent processes.

We will add additional benchmarks specifically for single host/renter interactions using RHP3 as a baseline for testing regressions.

The results published in this repository and the code to reproduce the benchmarks are under the MIT license.

## Benchmarks

### End-to-End

The end-to-end benchmark is designed to test the performance of a renterd node. It forms contracts with a number of hosts and tests the speed of encrypting, erasure-coding, uploading shards, downloading, and reconstruction in ideal production-like conditions. The benchmark performs four iterations using 160MiB test files and reports the average of the four iterations. 

*While the test data is 160MiB, the actual data uploaded to hosts is 480MiB due to 10-of-30 erasure-coding overhead. It would therefore be technically accurate to multiply the upload data size by three. However, the desire is to show the speed based on the file size a user uploads, not the redundant shards sent to hosts.*

[Results](results/e2e.csv)

## Usage

```
./benchmark -hosts=50 -output=results
```

## Example Output

```csv
Timestamp,CPU,hostd version,renterd version,upload speed,download speed
2024-09-17T11:30:58-07:00,Apple M1 Max,v1.1.3-0.20240913141601-573428f89b5f,v1.0.8-0.20240917130351-e7ad132b9077,679.3815 Mbps,1269.0282 Mbps
```