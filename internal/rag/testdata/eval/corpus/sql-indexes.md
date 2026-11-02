# Choosing a SQL index type

A relational database offers several index structures, and the right choice depends on how a column is queried. The default B-tree index orders entries, so it serves equality checks, range scans and sorted results on scalar columns. A GIN index maps each indexed value, such as a word in a text column or a key in a JSON document, to every row containing it; that makes it the usual choice for full-text search, arrays and document containment.

BRIN indexes take a different approach: they store one summary per range of adjacent table blocks. They stay tiny even on tables too large for a B-tree, but only help when a column's physical order correlates with its values, as with append-only timestamps. A partial index covers only rows matching a WHERE clause, which keeps it small when most queries filter to a minority of rows such as open orders.

Every index slows writes, because each insert and update must maintain its entries. Before adding one, confirm the column appears in selective filters and check the planner's estimate of index size against the table itself. Unused indexes can be found by comparing index scans against table writes, and a redundant index covering the same leftmost columns as a wider one usually can be dropped. Measure read latency and write amplification together; a faster lookup that doubles insertion cost may be a poor trade.
