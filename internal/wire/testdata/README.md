Wire fixtures come from the unmodified `homa_wire.h` at
PlatformLab/HomaModule revision `d8914b8a57aa48c19a2c8130484961585bcbe53c`:
https://github.com/PlatformLab/HomaModule/blob/d8914b8a57aa48c19a2c8130484961585bcbe53c/homa_wire.h

SHA-256: `84ee73542f3825d53427a712908df92ff8eee84e515a56d792e29b44a4fe21fa`.
The upstream file retains its BSD-2-Clause OR GPL-2.0+ SPDX notice.
The dependency shims provide only kernel scalar types and inline helper stubs;
all packet structs, opcodes, array dimensions, and packed layouts are upstream.

`fixtures.c` constructs representative packets using the C struct fields and
network-byte-order conversion functions, then emits their bytes, `sizeof`, and
`offsetof` results. Regenerate from `internal/wire` with:

```
cc -std=c11 -Wall -Wextra -Werror -Itestdata/shim testdata/fixtures.c -o /tmp/homa-wire-fixtures
/tmp/homa-wire-fixtures > testdata/upstream-fixtures.json
```

Go tests always compare against the committed C-generated golden. When a C
compiler is installed they also compile the pinned header and compare its fresh
output against that golden. These prove wire layout compatibility, not end-to-end
interoperability with a live Linux Homa endpoint.
