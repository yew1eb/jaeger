# ClickHouse storage backend

## Recommended Clickhouse version

No released version, waiting for the following PR:

* https://github.com/ClickHouse/ClickHouse/pull/12589

## Schema

It's expected that the tables are created externally, not by Jaeger itself. This
allows you to tweak the schema, as long as you preserve raad/write interface.

The schema went through a few iterations. Current version is three tables:

### Index table for searches

```sql
CREATE TABLE jaeger_index_v2 (
  timestamp DateTime CODEC(Delta, ZSTD(1)),
  traceID String CODEC(ZSTD(1)),
  service LowCardinality(String) CODEC(ZSTD(1)),
  operation LowCardinality(String) CODEC(ZSTD(1)),
  durationUs UInt64 CODEC(ZSTD(1)),
  tags Array(String) CODEC(ZSTD(1)),
  INDEX idx_tags tags TYPE bloom_filter(0.01) GRANULARITY 64,
  INDEX idx_duration durationUs TYPE minmax GRANULARITY 1
) ENGINE MergeTree()
PARTITION BY toDate(timestamp)
ORDER BY (service, -toUnixTimestamp(timestamp))
SETTINGS index_granularity=1024
```

Here we have a primary key that allows early termination of search by limit,
if you have more hits than you requested. This is very handy if you have a High
rate of spans coming into the system.

Tag lookups are done by bloom filter, so you can narrow down exact matches.

Duration lookups are done by minmax filter, which is useful
if you are looking for some timings that are outside of normal range.

Inversion of timestamp is necessary to efficiently read data in order.

### Span lookup table for models

```sql
CREATE TABLE jaeger_spans_v2 (
  timestamp DateTime CODEC(Delta, ZSTD(1)),
  traceID String CODEC(ZSTD(1)),
  model String CODEC(ZSTD(3))
) ENGINE MergeTree()
PARTITION BY toDate(timestamp)
ORDER BY traceID
SETTINGS index_granularity=1024
```

Here we have a primary key lookup by `traceID`, but having it sorted
differently from the index table mens that models are not compressed as well.

### Service and operation lookup table

```sql
CREATE MATERIALIZED VIEW jaeger_operations_v2
ENGINE SummingMergeTree
PARTITION BY toYYYYMM(date) ORDER BY (date, service, operation)
SETTINGS index_granularity=32
POPULATE
AS SELECT
  toDate(timestamp) AS date,
  service,
  operation,
  count() as count
FROM jaeger_index_v2
GROUP BY date, service, operation
```

Here we keep our services and operations in a compact form, since it's rather
expensive to pull them out of the index table, compared to their cardinality.

### Archive

Archive table for spans is the same as regular span storage, but with a monthly
partition key, since the number of archived traces is unlikely to be large:

```sql
CREATE TABLE jaeger_archive_spans_v2 (
  timestamp DateTime CODEC(Delta, ZSTD(1)),
  traceID String CODEC(ZSTD(1)),
  model String CODEC(ZSTD(3))
) ENGINE MergeTree()
PARTITION BY toYYYYMM(timestamp)
ORDER BY traceID
SETTINGS index_granularity=1024
```

### Known issues

Issues below are from running a single instance Clickhouse v20.1.4.14-stable.

* CPU: 2 x E5-2630 v4 @ 2.20GHz
* Memory: 8 x 32GB @ 2667MHz
* Storage: RAID0 across 12 x ST10000NM0016 (Seagate SATA 10TB Helium)

#### Query performance depends on time range

Ideally we'd like to terminate search queries as soon as we found enough
traceIDs to respond, but Clickhouse wants to examine every part.

Consider this query:

```sql
SELECT DISTINCT traceID
FROM jaeger_index_v2
WHERE
    service = 'some-service'
    AND
    timestamp >= now() - 3600
ORDER BY
    service DESC,
    timestamp DESC
LIMIT 1
```

Running it with 1h window takes ~150ms:

```
<Debug> jaeger.jaeger_index_v2 (SelectExecutor): Selected 28 parts by date, 28 parts by key, 67610 marks to read from 28 ranges
...
1 rows in set. Elapsed: 0.151 sec. Processed 737.34 thousand rows, 25.23 MB (4.87 million rows/s., 166.69 MB/s.)
```

Running it with 24h window takes almost 10000ms:

```
<Debug> jaeger.jaeger_index_v2 (SelectExecutor): Selected 49 parts by date, 49 parts by key, 2015820 marks to read from 49 ranges
...
1 rows in set. Elapsed: 9.817 sec. Processed 954.50 thousand rows, 32.72 MB (97.23 thousand rows/s., 3.33 MB/s.)
```

This is not great, since intuitively the information we see is known to be
in the very same most recent parts no matter what window we query. The query
itself is ordered by primary key.

The solution we arrived to is to query ranges in progressively increasing
intervals, stopping early if enough results are found. This works reasonably
well for both cases when data is found early and when it need longer lookback.

### Useful queries

#### Storage usage

```sql
SELECT
    table,
    sum(marks) AS marks,
    sum(rows) AS rows,
    formatReadableSize(sum(data_compressed_bytes)) AS compressed,
    formatReadableSize(sum(data_uncompressed_bytes)) AS uncompressed,
    toDecimal64(sum(data_uncompressed_bytes) / sum(data_compressed_bytes), 2) AS compression_ratio,
    formatReadableSize(sum(data_compressed_bytes) / rows) AS bytes_per_row,
    formatReadableSize(sum(primary_key_bytes_in_memory)) AS pk_in_memory
FROM system.parts
WHERE (table IN ('jaeger_index_v2', 'jaeger_spans_v2')) AND active
GROUP BY table
ORDER BY table ASC
```

```
┌─table───────────┬─────marks─┬─────────rows─┬─compressed─┬─uncompressed─┬─compression_ratio─┬─bytes_per_row─┬─pk_in_memory─┐
│ jaeger_index_v2 │ 104649234 │ 107160536185 │ 2.51 TiB   │ 26.81 TiB    │             10.66 │ 25.79 B       │ 898.33 MiB   │
│ jaeger_spans_v2 │ 104649563 │ 107160646185 │ 7.24 TiB   │ 40.21 TiB    │              5.55 │ 74.29 B       │ 3.01 GiB     │
└─────────────────┴───────────┴──────────────┴────────────┴──────────────┴───────────────────┴───────────────┴──────────────┘
```

```sql
SELECT
    table,
    column,
    formatReadableSize(sum(column_data_compressed_bytes)) AS compressed,
    formatReadableSize(sum(column_data_uncompressed_bytes)) AS uncompressed,
    toDecimal64(sum(column_data_uncompressed_bytes) / sum(column_data_compressed_bytes), 2) AS compression_ratio,
    formatReadableSize(sum(column_data_compressed_bytes) / sum(rows)) AS bytes_per_row
FROM system.parts_columns
WHERE (table IN ('jaeger_index_v2', 'jaeger_spans_v2')) AND active
GROUP BY
    table,
    column
ORDER BY
    table ASC,
    column ASC
```

```
┌─table───────────┬─column─────┬─compressed─┬─uncompressed─┬─compression_ratio─┬─bytes_per_row─┐
│ jaeger_index_v2 │ durationUs │ 199.61 GiB │ 798.37 GiB   │              3.99 │ 2.00 B        │
│ jaeger_index_v2 │ operation  │ 46.63 GiB  │ 180.97 GiB   │              3.88 │ 0.47 B        │
│ jaeger_index_v2 │ service    │ 140.26 MiB │ 101.67 GiB   │            742.28 │ 0.00 B        │
│ jaeger_index_v2 │ tags       │ 1.78 TiB   │ 22.74 TiB    │             12.75 │ 18.29 B       │
│ jaeger_index_v2 │ timestamp  │ 108.50 GiB │ 798.37 GiB   │              7.35 │ 1.09 B        │
│ jaeger_index_v2 │ traceID    │ 393.11 GiB │ 2.23 TiB     │              5.81 │ 3.94 B        │
│ jaeger_spans_v2 │ model      │ 6.60 TiB   │ 37.19 TiB    │              5.63 │ 67.75 B       │
│ jaeger_spans_v2 │ timestamp  │ 374.91 GiB │ 798.37 GiB   │              2.12 │ 3.76 B        │
│ jaeger_spans_v2 │ traceID    │ 277.57 GiB │ 2.23 TiB     │              8.24 │ 2.78 B        │
└─────────────────┴────────────┴────────────┴──────────────┴───────────────────┴───────────────┘
```

#### Ongoing merges

```sql
SELECT
    table,
    partition_id,
    round(elapsed) AS elapsed,
    toDecimal64(progress * 100, 2) AS progress,
    num_parts,
    formatReadableSize(total_size_bytes_compressed) AS compressed,
    formatReadableSize(bytes_read_uncompressed) AS read_uncompressed,
    formatReadableSize(bytes_written_uncompressed) AS written_uncompressed,
    formatReadableSize(memory_usage) AS memory
FROM system.merges
WHERE table IN ('jaeger_index_v2', 'jaeger_spans_v2')
```

```
┌─table───────────┬─partition_id─┬─elapsed─┬─progress─┬─num_parts─┬─compressed─┬─read_uncompressed─┬─written_uncompressed─┬─memory────┐
│ jaeger_spans_v2 │ 20200531     │     554 │    42.21 │         7 │ 23.95 GiB  │ 58.60 GiB         │ 58.60 GiB            │ 62.35 MiB │
│ jaeger_index_v2 │ 20200531     │       1 │    55.29 │         7 │ 8.58 MiB   │ 72.19 MiB         │ 68.47 MiB            │ 45.95 MiB │
│ jaeger_spans_v2 │ 20200531     │       0 │   100.00 │         5 │ 4.26 MiB   │ 21.31 MiB         │ 20.93 MiB            │ 8.00 MiB  │
│ jaeger_index_v2 │ 20200531     │       0 │    20.48 │         6 │ 2.01 MiB   │ 4.29 MiB          │ 2.15 MiB             │ 32.25 MiB │
└─────────────────┴──────────────┴─────────┴──────────┴───────────┴────────────┴───────────────────┴──────────────────────┴───────────┘
```

### Other tried schema approaches

#### Tags in a nested field

Having `tags` like this makes searches way slower:

```
tags Nested(
  key LowCardinality(String),
  valueString LowCardinality(String),
  valueBool UInt8,
  valueInt Int64,
  valueFloat Float64
)
```

However, it allows more complex searches, like prefix for strings and range
for numeric values.

#### Single table

One other approach is to have a single table with an extra index on `traceID`:

```sql
CREATE TABLE jaeger_v1 (
  timestamp DateTime CODEC(Delta, ZSTD(1)),
  traceID String CODEC(ZSTD(1)),
  service LowCardinality(String) CODEC(ZSTD(1)),
  operation LowCardinality(String) CODEC(ZSTD(1)),
  durationUs UInt64 CODEC(ZSTD(1)),
  tags Array(String) CODEC(ZSTD(1)),
  model String CODEC(ZSTD(3)),
  INDEX idx_tags tags TYPE bloom_filter(0.01) GRANULARITY 64,
  INDEX idx_traces traceID TYPE bloom_filter(0.01) GRANULARITY 512
) ENGINE MergeTree() PARTITION BY toDate(timestamp) ORDER BY (service, timestamp)
SETTINGS index_granularity=1024;
```

Unfortunately, this is a whole lot slower for lookups by `traceID`.

#### Having `operation` in primary key

Having `ORDER BY (service, operation, timestamp)` means that you fall back
to slower searches when operation is not defined. Having `operation` at the end
doesn't make anything faster. One could hope for better compression to sorting,
but it wasn't observed in test setting.

#### Different codecs for columns

Here are values that were tried (other than the defaults), before settling:

* `timestamp`: `CODEC(DoubleDelta)`, `CODEC(DoubleDelta, ZSTD(1))`
* `durationUs`: `CODEC(T64)`
* `tags`: `CODEC(ZSTD(3))`
* `model`: `CODEC(ZSTD(1))`

#### Different index granularity

Index granularity is picked to be explicit (just like compression level),
and it proved to be a bit faster and less greedy in terms of reads in testing.

Your mileage may vary.

#### Single tag per row

This produces too many rows and makes things slower and less compact.

#### Binary traceID column

Having `traceID` as a binary string in theory saves half of uncompressed space,
but with compression enabled it saved only 0.3 bytes per span.

## Possible improvements

### Materialized view for cross-span searches

It's possible to aggregate tags (and maybe durations?) across spans of a trace
in a materialized view, which enables searches for multiple tags that belong
to different spans in a trace.

Consider the following trace:

```
+ root span: request {host = "example.com", responseStatus = 200}
++++ child span: upstream {upstreamResponseStatus = 502}
```

With cross-span searches one can search for:

```
responseStatus=200 upstreamResponseStatus=502
```

There are open questions about whether this should be segregated by service
or not, and how searches should work if not. Primary key also gets interesting.

### Separate columns for commonly searched tags

Currently search by tags is working in two stages:

* Index lookup to find granules with possible matches
* Full deserialization and match for array elements of individual spans

This is pretty slow if there's a lot of matches, and if we extract
commonly searched tags, searches on those can be significantly faster.

Commonly searched tags can be extracted in either a separate array column,
or each tag in a separate column. An index is needed in either case.

### Insert through one table with Null engine

It's possible to insert to both index and model tables via intermediate
table with `Null` engine. In addition to simpler inserts it also allows
operators to build more complex transformations before storing the data.

Materialized view from cross-span searches is one example of that.
