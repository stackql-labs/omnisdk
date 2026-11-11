//go:build !windows

package journal

import (
	"fmt"
	"os"
)

// syncDir makes the new link itself durable; without it the file survives a crash but the
// directory entry naming it may not.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("journal opendir %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("journal syncdir %s: %w", dir, err)
	}
	return nil
}
