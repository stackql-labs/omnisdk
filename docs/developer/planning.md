# Join planning

OmniSDK uses a greedy join order for read queries with inner, listed, and cross joins. Request bindings establish fixed dependencies. The planner completes ready dependent lookups first, then prefers equality joins with a source already in the stream and leaves isolated Cartesian inputs until later. Separate connected groups still need a Cartesian step to combine them. The executor uses ordered pipeline stages for these plans so a parallel group cannot pull a Cartesian input ahead of a lookup.

A table can implement `JoinCostTable` to estimate rows and bytes per read. The planner multiplies those estimates by the number of requests implied by table-local fan-out. It prefers fewer estimated rows among eligible request lookups. When starting a group of unbound equality joins, it prefers to stream the larger input and use smaller inputs for hash lookups. Missing estimates use a unit cost, and ties follow `FROM` order. The planner does not probe sources or estimate predicate selectivity.

Queries with left joins, table functions, or mutations use the existing ordering rules.
