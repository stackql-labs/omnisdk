# pgcatalog

Generates `pkg/sqlfn/pg_catalog_gen.go` — every overload of the functions the Postgres catalogue
implements, the implicit casts, each type's category, the argument defaults, the time zone abbreviations, and the keywords `quote_ident` quotes — from Postgres 14.5's catalog data, so the
catalogue resolves overloads from the data Postgres itself resolves them with.

```bash
d=$(mktemp -d)
for f in pg_proc pg_cast pg_type; do
  curl -sfL -o $d/$f.dat https://raw.githubusercontent.com/postgres/postgres/REL_14_5/src/include/catalog/$f.dat
done
curl -sfL -o $d/kwlist.h https://raw.githubusercontent.com/postgres/postgres/REL_14_5/src/include/parser/kwlist.h
curl -sfL -o $d/system_functions.sql https://raw.githubusercontent.com/postgres/postgres/REL_14_5/src/backend/catalog/system_functions.sql
curl -sfL -o $d/Default https://raw.githubusercontent.com/postgres/postgres/REL_14_5/src/timezone/tznames/Default
python3 internal/tools/pgcatalog/gen.py $d < internal/tools/pgcatalog/names.txt > pkg/sqlfn/pg_catalog_gen.go
gofmt -w pkg/sqlfn/pg_catalog_gen.go
```

A function added to the catalogue is added to `names.txt` and the file regenerated.
