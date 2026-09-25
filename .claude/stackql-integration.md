# Routing stackql queries through omnisdk

What stackql has to do to send a query to omnisdk and stream back its rows. omnisdk resolves the
query against the provider documents and runs it; stackql parses SQL, holds sessions, and does
whatever needs every row at once.

## Division of work

| stackql | omnisdk |
|---------|---------|
| Parse SQL | Choose each table's method from the documents |
| Split CTEs, subqueries and UNION into single queries, then combine results | Decide which conditions become request parameters, edges between tables, or row filters |
| ORDER BY, GROUP BY, aggregates, DISTINCT | Run requests, joins, `IN` fan-out, polls and mutations |
| Hold per-client sessions and turn them into per-query config | Return an eager, unordered, unaggregated row stream |

## Per query

### 1. Build a `query.Unresolved` (package `pkg/query`, standard library only)

Handles are registry addresses: `stackql_unstable_aws.iam.users`. omnisdk does not map names.

| SQL | Constructor |
|-----|-------------|
| `FROM t a` | `query.NewJoin(query.NewResource("a", "<address>"), query.Base)` |
| `[INNER] JOIN t b ON …` | `query.NewJoin(query.NewResource("b", …), query.Inner, on...)` |
| `LEFT JOIN t b ON …` | `query.NewJoin(…, query.Left, on...)` |
| `a.col` / unqualified `col` | `query.NewColumn("a", "col")` / `query.NewColumn("", "col")` |
| `'x'`, `1` | `query.NewLiteral(v)` |
| `f(x, y)` | `query.NewCall("f", x, y)` |
| `=`, `<>`, `<`, `<=`, `>`, `>=` | `query.NewCompare(query.Eq, l, r)` (and `Ne`, `Lt`, …) |
| `x IN (…)` | `query.NewIn(x, query.NewCollection(items...))` |
| `OR`, `NOT` | `query.NewOr(...)`, `query.NewNot(p)` |
| boolean function as condition | `query.NewTest(call)` |
| `SELECT expr AS name` | `query.NewOutput("name", expr)` |
| `SELECT *` / `a.*` | `query.NewOutput("", query.NewStar(""))` / `NewStar("a")` |
| `INSERT INTO t (c…) VALUES (…)` | `query.NewInsert(res, query.NewAssignment("c", v)...)` |
| `INSERT … SELECT` | same, with values reading the SELECT's tables, which go in `from` |
| `UPDATE t SET c = v` | `query.NewUpdate(res, assignments...)` |
| `DELETE FROM t` | `query.NewDelete(res)` |
| `RETURNING …` | the mutation's outputs |

Split WHERE and each ON into conjuncts (top-level `AND`). Every output needs a name: give
unnamed expressions one. Then:

```go
q, err := query.New(from, where, outputs)                      // a read
q, err := query.NewMutation(target, from, where, returning)    // a mutation
```

Leave out ORDER BY, GROUP BY, aggregates, DISTINCT and HAVING. If ORDER BY or GROUP BY reads a
column that isn't selected, add it to the outputs so it comes back.

Not supported: RIGHT, FULL and CROSS joins.

### 2. Describe each table

```go
tables := map[string]omnisdk.Table{}
for _, j := range q.From() {
    t, err := omnisdk.DescribeTable(registry, j.Resource().Handle())
    tables[j.Resource().Alias()] = t
}
if tg := q.Target(); tg != nil {
    t, err := omnisdk.DescribeMutation(registry, tg.Resource().Handle(), tg.Verb().String())
    tables[tg.Resource().Alias()] = t
}
```

A `Table` lists each method's parameters (name, location, required) and row columns, which is also
what `DESCRIBE` / `SHOW` can report.

### 3. Resolve

```go
res, err := omnisdk.Resolve(q, tables)
```

An error names the clause it could not place, for example a condition a mutation's method can't take
(it would only be checked after the effect), a left join whose preserved side needs the other side's
value, or an ambiguous `*`. Show it to the user as is.

### 4. Run

```go
args := omnisdk.Args{
    Params:    merge(session.Params, res.Params()), // scope such as region, then the query's own
    Auth:      session.Auth,                        // nil falls back to the environment
    Tuning:    omnisdk.Tuning{Limit: limit},        // LIMIT without ORDER BY; otherwise apply it after sorting
    Journal:   session.Journal,                     // opt-in write-ahead intent for mutations
    Redaction: session.Redaction,                   // nil drops credentials from rows
}
pl, err := omnisdk.NewGraphSelectQuery(registry, res.Graph(), args)
rows, err := pl.Open(ctx)
defer rows.Close()
for rows.Next() {
    row := rows.Row() // map[string]any, keyed by output name
}
err = rows.Err()
```

Rows arrive as they are produced. A left join's unmatched row lacks the joined table's columns;
treat a missing column as NULL.

## Per session

- **Config:** keep it in the session and build a fresh `Args` for each query. omnisdk holds no
  per-client state, so concurrent clients with different settings don't interfere.
- **Document changes for one client:** `EffectiveRegistry(registry, patches, omnisdk.DocCache{Dir: dir})`
  returns a registry directory with that client's patches applied (RFC 7386 merge patches on service
  documents). Use it as `registry` for that client's queries. `NewDocCache(parent)` gives a new cache
  location; passing an existing `Dir` reuses its entries; `Fresh: true` rebuilds.
- **Mutations:** set `Args.Journal{State, RunID}` to record each effect before it is sent. A failure is
  reported as "rejected" (the provider refused it) or "may or may not have taken effect" (no answer, or
  a 5xx). Mutations are never retried.
- **Credentials:** dropped from result rows by default. `omnisdk.RedactNone()` keeps them, for a user
  who needs them.

## Once per process

- `omnisdk.ConfigureDocumentCache(cache.Config{...})` bounds the parsed-document cache. The default
  takes a quarter of the process's memory limit (`GOMEMLIMIT`, else the container's cgroup limit,
  else 512 MiB). Parsed documents are keyed by the directory they are read from, so clients with
  different patches never share entries.
