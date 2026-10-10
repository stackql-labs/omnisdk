# libmtables

Generates the C libraries' data tables the SQL catalogues' float functions read:

- `pkg/sqlfn/libm_musl_tables.go`, from musl 7ada6dde (the libm modernc.org/libc, and so stackql's
  embedded SQLite, runs on Linux);
- `pkg/sqlfn/glibc_sincostab_gen.go`, from glibc 2.31 (the libc of stackql's Postgres image,
  `postgres:14.5-bullseye`).

```bash
d=$(mktemp -d)
curl -sfL https://git.musl-libc.org/cgit/musl/snapshot/musl-7ada6dde6f9dc6a2836c3d92c2f762d35fd229e0.tar.gz | tar -xz -C $d
python3 internal/tools/libmtables/musl.py $d/musl-7ada6dde6f9dc6a2836c3d92c2f762d35fd229e0/src/math | gofmt > pkg/sqlfn/libm_musl_tables.go
curl -sfL -o $d/sincostab.c https://raw.githubusercontent.com/bminor/glibc/glibc-2.31/sysdeps/ieee754/dbl-64/sincostab.c
python3 internal/tools/libmtables/sincostab.py $d/sincostab.c | gofmt > pkg/sqlfn/glibc_sincostab_gen.go
```
