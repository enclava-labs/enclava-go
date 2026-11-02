# API versioning and breaking changes

A public API is a promise about behavior: field names, types, error shapes and defaults that clients bake into their own code. A breaking change is one that makes previously valid client code fail — removing or renaming a field, changing a value's type or meaning, tightening a required parameter, or altering an error contract clients match on. Adding an optional field, a new endpoint, or a new enum value is usually safe when clients ignore what they do not know.

The common conventions for announcing a break are URL path versions such as /v2/orders, a version header or media type, and query parameters. Path versions are the least ambiguous: caches, logs and routing all see the version directly. Whatever the mechanism, run the old and new versions side by side during migration instead of rewriting behavior in place.

Every new version needs a lifecycle: announce the deprecation date, keep the old version answering requests with warning headers, publish a migration guide naming exactly what changed, and set a real sunset date after which old routes return an error rather than silently degrading. Track usage of the old version and contact its remaining callers before the cutoff. Without a retirement plan, versions accumulate and every fix must be ported across the whole supported set.
