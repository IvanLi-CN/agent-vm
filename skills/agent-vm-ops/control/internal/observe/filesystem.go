package observe

import (
	"math"

	"golang.org/x/sys/unix"
)

// Filesystems samples host filesystems separately from guest disk reservations.
// Failed reads retain an explicit health signal without invented capacity.
func Filesystems(paths ...string) []map[string]any {
	result := []map[string]any{}
	seen := map[string]bool{}
	for _, path := range paths {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		row := map[string]any{"mountpoint": path, "up": false}
		var value unix.Statfs_t
		if err := unix.Statfs(path, &value); err == nil && value.Bsize > 0 && value.Blocks > 0 && value.Bfree <= value.Blocks && value.Bavail >= 0 && uint64(value.Bavail) <= value.Blocks {
			block := uint64(value.Bsize)
			if value.Blocks <= math.MaxInt64/block {
				row["up"] = true
				row["size_bytes"] = int64(value.Blocks * block)
				row["used_bytes"] = int64((value.Blocks - value.Bfree) * block)
				row["available_bytes"] = int64(uint64(value.Bavail) * block)
			}
		}
		result = append(result, row)
	}
	return result
}
