//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
)

type processSampler struct{ handles int64 }

func (s *processSampler) sample(pid int) (int64, float64, error) {
	s.handles = -1 // Not collected on macOS; never present unknown as zero.
	if runtime.GOOS == "linux" {
		entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		if err != nil {
			return 0, 0, err
		}
		s.handles = int64(len(entries))
	}
	data, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "rss=,%cpu=").Output()
	if err != nil {
		return 0, 0, err
	}
	var rss int64
	var cpu float64
	_, err = fmt.Sscan(string(data), &rss, &cpu)
	return rss, cpu, err
}
