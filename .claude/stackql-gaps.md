# stackql → omnisdk: gaps for real queries

Target: stackql sends the read side of every query to omnisdk as a `query.Unresolved`; omnisdk resolves
it against the documents and returns an eager, unordered, unaggregated row stream. stackql applies
ORDER BY, GROUP BY and aggregation.

Works today (`TestRealWorldQueryEndToEnd`): inner joins on equality, with the edge direction derived
from which side's methods require the value; constant equality bindings; single-table projections,
functions included.

## Gaps

1. **Filters that can't bind:** `<>`, `<`, `LIKE`, `OR`, `NOT`, and bindings the API applies
   inexactly (e.g. `PathPrefix`), which must still be re-filtered locally.
2. **Joins neither side needs:** two listable tables joined on columns. Common. Needs a local join;
   today an unwired node is re-listed once per upstream row.
3. **LEFT JOIN.**
4. **`IN` lists:** `region IN ('a','b')` must fan out one request per value. Common.
5. **Computed values:** functions on join keys; expressions reading several tables.
6. **Output contract:**
   - emit exactly the selected columns (`region` currently leaks into every row);
   - expand `SELECT *` from the schema;
   - emit the ORDER BY / GROUP BY columns, which stackql's front end adds to the select list.
7. **Naming:** mapping stackql handles (`aws.iam.users`) to registry addresses
   (`stackql_unstable_aws.iam.users`).
8. **Queries that aren't one SELECT** (CTEs, subqueries, UNION): stackql splits them into single
   SELECTs, sends each, and combines the results.
9. **Mutations** (INSERT/UPDATE/DELETE/EXEC): a separate path, not covered by the read-query
   abstraction.

## Deferred

- Choosing a hash join vs a per-row lookup needs cardinality stats, which aren't collected.
- A node with no wiring into it could run once and be replayed from a cache instead of being
  re-listed per row. Not built.
