package schemadiff

// Classify decides whether a Diff is safe or destructive by comparing
// table and column presence between the two structured schemas, never by
// scanning generated SQL text for statements like DROP.
