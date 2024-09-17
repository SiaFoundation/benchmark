# Benchmark

A simple benchmarking tool for Sia. Starts up a small local testnet with 50 storage providers and uploads and downloads 1GiB of data. The upload benchmark measures the time it takes to encrypt, erasure-code, and upload the data using the default 10-of-30 params. The download benchmark measures the time it takes to download, erasure-code, and decrypt the data.

We will add additional benchmarks specifically for single host/renter interactions using RHP3 as a baseline for testing regressions.

The goal of this repository is to publish easy to reproduce end-to-end benchmarks for Sia in ideal conditions. As such, it should be run on a machine with a recent CPU with plenty of cores to support the number of concurrent processes.

Both the data in this repository and the code to run the benchmarks are licensed under the MIT license.

## Usage

```
./benchmark -hosts=50 -output=results
```

## Example Output

```csv
Timestamp,CPU,hostd version,renterd version,upload speed,download speed
2024-09-17T11:30:58-07:00,Apple M1 Max,v1.1.3-0.20240913141601-573428f89b5f,v1.0.8-0.20240917130351-e7ad132b9077,679.3815 Mbps,1269.0282 Mbps
```