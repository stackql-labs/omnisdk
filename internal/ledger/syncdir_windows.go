package ledger

// Windows does not support flushing a directory opened by os.Open: FlushFileBuffers
// requires a writable handle and directories reject that access. The ledger file
// itself is synced before linking on every platform; directory syncing is Unix only.
func syncDir(_ string) error { return nil }
