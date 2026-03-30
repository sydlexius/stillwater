package updater

import (
	"os"
	"strings"
)

// InContainer reports whether the process is running inside a container.
// Three detection methods are tried in order:
//
//  1. Docker creates /.dockerenv in every container.
//  2. /proc/1/cgroup entries contain well-known container runtime strings.
//  3. The "container" environment variable is set by systemd-nspawn and some
//     cgroupv2 environments.
func InContainer() bool {
	// Method 1: Docker creates this file in every container.
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}

	// Method 2: Check cgroup for container runtime indicators.
	data, err := os.ReadFile("/proc/1/cgroup")
	if err == nil {
		s := string(data)
		if strings.Contains(s, "docker") ||
			strings.Contains(s, "containerd") ||
			strings.Contains(s, "kubepods") ||
			strings.Contains(s, "lxc") {
			return true
		}
	}

	// Method 3: Environment variable set by some container runtimes.
	if os.Getenv("container") != "" {
		return true
	}

	return false
}
