package sqlitemigrate

// Checksum computes and verifies a per-migration checksum over a
// migration's SQL body, so tampering with an already-applied migration
// file is detected on every generate and apply, not just recorded once.
