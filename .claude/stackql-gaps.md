# stackql → omnisdk: gaps for real queries

Target: stackql sends the read side of every query to omnisdk as a `query.Unresolved`; omnisdk resolves
it against the documents and returns an eager, unordered, unaggregated row stream. stackql applies
ORDER BY, GROUP BY and aggregation.

Joins: stackql supports only inner and left outer joins, and the engine has both merges. `Resolve`
still rejects LEFT until it selects the left-outer merge for the joined node.

| # | Gap | Status | Implementation |
|---|-----|--------|----------------|
| 1 | Filters that can't bind (`<>`, `<`, `OR`, `NOT`, `Test`), and bindings the API may apply inexactly | Fixed | `Resolve` turns every unplaced condition into a graph filter, and re-checks a pushed-down binding wherever the row has that column. Filters run on each finished row with SQL three-valued logic, reading each column from its node's private key (`relational.go`: `filterTransform`). |
| 2 | Joins neither side needs | Fixed | The equality becomes a filter over the engine's nested loop. A node with no wiring into it runs once per distinct input and is replayed from a per-run cache, streaming as its rows arrive (`replayed`). Each side is listed once. |
| 3 | `IN` lists | Fixed | On a table's parameter: `NewFanoutNode` gets a no-network values exchange emitting one row per value, bound into the node. Query-wide (e.g. `region`): one values exchange runs first, so every node in a row sees the same value (`valuesSpec`). |
| 4 | Computed values: functions on join keys; expressions reading several tables | Fixed | `column = f(other table)`: where the column's table needs the value, the producer computes `f` in its projection under a hidden name and the edge carries it; otherwise it is a filter. A function on the needing side cannot be inverted and never binds (`bindComputed`). An output reading several tables is computed on each finished row, from the columns each node keeps (`outputTransform`). |
| 5a | Output is exactly the selected columns | Fixed | Where every node has a projection, egress keeps only the projected columns (`onlyColumns`). |
| 5b | `SELECT *` expanded from the schema | Fixed | `query.NewStar(qualifier)` states `*` or `u.*`. `Resolve` expands it to the columns the table's methods declare, each named by its column. A table with no declared schema, or two expanded columns sharing a name (e.g. `*` over a self-join), is an error (`expand`). |
| 5c | ORDER BY / GROUP BY columns emitted | Open | stackql's front end adds them to the select list; nothing needed in omnisdk. |
| 6 | Naming: stackql handles → registry addresses | Open | |
| 7 | Queries that aren't one SELECT (CTEs, subqueries, UNION) | Open | stackql splits them into single SELECTs, sends each, and combines the results. |
| 8 | Mutations (INSERT/UPDATE/DELETE, with RETURNING) | Fixed, one exception | Same abstraction: `query.NewMutation` with a `Target` (verb, resource, assignments); RETURNING is its Select. `Resolve` places the target as a node running its verb's methods: a literal assignment is a parameter, a source column or a function of one is an edge (INSERT … SELECT runs once per source row), and WHERE on the target binds its parameters, `IN` fanning out. A condition the methods can't take is refused, since it could only be checked after the effect; so are sources wired into nothing. The node is never replayed and never retried, and a failure is reported as possibly applied (`effect`). An assignment's column resolves through a `pkg/namespace` built from the method's declared parameters and their locations; a name no parameter declares is a request-body field where the method takes a body (`body.x` qualifies it), placed per row by docx `WithBodyFields` (e.g. `INSERT INTO google.storage.buckets (project, name, location)`: `project` in the query, `name`/`location` in the JSON body). **Exception:** intent is not journaled before the effect (invariant 7), which needs an explicit state directory. |

## Deferred

- Choosing a hash join vs a per-row lookup needs cardinality stats, which aren't collected.
