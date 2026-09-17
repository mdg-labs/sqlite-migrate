package sqlitemigrate

// CheckDrift replays a migration sequence into a temporary database and
// compares the resulting schema against the target, so both generate-time
// verification and the `verify` CLI command against a real database share
// the same replay-based comparison rather than trusting generated SQL
// text.
