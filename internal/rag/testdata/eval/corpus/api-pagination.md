# Paginating list endpoints

Offset pagination answers SELECT ... OFFSET n LIMIT k, which is simple to implement but degrades on deep pages: the database must still scan and discard the skipped rows, and rows inserted or deleted while a client pages forward shift the window, producing duplicates or gaps. Cursor, or keyset, pagination avoids both problems. Each response returns an opaque token encoding the sort position of the last item, and the next request asks for rows strictly past that position along a stable, unique sort order.

The sort order must be deterministic, so append the primary key as a tie-breaker after a non-unique column like creation time. Because the condition is a comparison rather than a skip, the query cost is the same on page one and page ten thousand, provided an index supports the ordering.

Cursors also make the result contract explicit: pages are consistent snapshots in a moving direction rather than absolute positions. They do not support jumping to an arbitrary page, so interfaces that need numbered pages still use offsets and accept the cost. Whatever the scheme, publish the page size limits, document that a cursor expires alongside its underlying listing filters, and return the token only in the response envelope rather than constructing it from client-visible ids.
