# Compact country database (`country.mncc`)

A minimal, read-only **IP → ISO 3166-1 alpha-2 country code** database published with
every release as `country.mncc` (checksum in `SHA256SUMS.txt`). It holds nothing else, and
is meant to be bundled in an app and looked up in microseconds without parsing or loading
it into memory beyond the file itself.

## How it is derived

- Sources: RIR allocations and geofeeds from the same pipeline as the main dataset.
- Precedence matches `/v1/ip`: a geofeed country beats an RIR allocation country; within a
  layer the narrower range wins.
- Only strict two-letter uppercase codes are used (`EU # …` pseudo-countries are dropped).
- The address space is quantized to **IPv4 /24** and **IPv6 /48**. Source ranges narrower than
  that are ignored, so a broader covering range answers for them. Anything not covered
  is "unknown".
- Neighbouring slots with the same country are merged, then range starts are delta/varint coded.

Rebuild with different granularity: `build-country-db -v4-prefix 22 -v6-prefix 40`
(smaller file, less precise); `-geofeeds=false` for RIR-only data.

## Format (version 1)

All fixed-width integers are little endian; varints are unsigned LEB128.

```
"MNCC"                       magic
u8 format = 1
u8 v4Bits, u8 v6Bits         granularity (24, 48 by default)
u8 blockSize                 entries per index block (64)
u16 n                        number of country codes
n * 2 bytes                  ASCII codes; country index 1..n (index 0 = unknown, frequency-sorted)
then the IPv4 family, then the IPv6 family:
  u32 entryCount
  u32 dataLen
  blocks * (key, u32 offset) key = ceil(bits/8) bytes: slot of the block's first entry
  data                       per entry: uvarint(slot - previous slot), uvarint(country index)
```

A *slot* is the address shifted right by `32 - v4Bits` (IPv4) or the top 64 bits shifted right by
`64 - v6Bits` (IPv6; the low 64 bits are ignored). An entry covers slots from its start to
the next entry's start (the last entry runs to the end of the space). The first entry of a
block has delta 0 relative to the block key; offsets are relative to the start of `data`.
IPv4-mapped IPv6 addresses (`::ffff:a.b.c.d`) are looked up as IPv4.

## Lookup

1. Compute the slot. 2. Binary-search the block index for the last block key ≤ slot.
3. Scan at most `blockSize` entries, accumulating deltas, until the next start exceeds the slot;
the previous entry's country index is the answer (0 → unknown).

The reference reader is [`internal/countrydb`](../internal/countrydb/countrydb.go) (`Open`, `Lookup`).
It needs only the raw bytes, so porting it to Kotlin/Java is ~60 lines; a memory-mapped
`ByteBuffer` works without copying.
