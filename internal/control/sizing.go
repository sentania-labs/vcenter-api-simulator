package control

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const mib int64 = 1 << 20

// MemoryRequired is a conservative planning budget, not a heap prediction.
// It allows two collectors, transient SOAP buffers, and overlapping old/new
// inventories during rebuild. See docs/sizing.md for the measured envelope.
func MemoryRequired(p *Projection) int64 {
	n := 128*mib + int64(p.Hosts)*2*mib + int64(p.VMs)*256*1024 + int64(p.NVMe+p.LocalDisks)*16*1024
	return ((n + 255*mib) / (256 * mib)) * 256 * mib
}

// memoryLimit walks this process's cgroup v2 ancestry because a parent slice
// may enforce a tighter limit than its leaf. Missing/unlimited is unknown.
func memoryLimit() int64 {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return limitUnder("/sys/fs/cgroup", path)
		}
	}
	return 0
}
func limitUnder(root, path string) int64 {
	dir := filepath.Join(root, filepath.Clean("/"+path))
	var limit int64
	for {
		b, err := os.ReadFile(filepath.Join(dir, "memory.max"))
		if err == nil {
			n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
			if err == nil && n > 0 && (limit == 0 || n < limit) {
				limit = n
			}
		}
		if dir == root {
			return limit
		}
		parent := filepath.Dir(dir)
		if parent == dir || !strings.HasPrefix(parent, root) {
			return limit
		}
		dir = parent
	}
}
func memoryLine(p *Projection, limit int64) Item {
	need := MemoryRequired(p)
	text := fmt.Sprintf("Allow about %d MiB of memory for %s and %s, including two collectors and rebuilding.", need/mib, plural(p.Hosts, "host"), plural(p.VMs, "VM"))
	if limit > 0 && need > limit {
		text += fmt.Sprintf(" This size will not fit the process memory limit of %d MiB. Raise the container memory limit to at least %d MiB before applying it.", limit/mib, need/mib)
	} else if limit > 0 {
		text += fmt.Sprintf(" The process memory limit is %d MiB.", limit/mib)
	}
	return note("%s", text)
}
