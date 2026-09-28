//go:build !windows

package main

import (
	"fmt"
	"os/exec"
	"strconv"
)

type processSampler struct{}

func (*processSampler) sample(pid int) (int64, float64, error) {
	data, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "rss=,%cpu=").Output()
	if err != nil {
		return 0, 0, err
	}
	var rss int64
	var cpu float64
	_, err = fmt.Sscan(string(data), &rss, &cpu)
	return rss, cpu, err
}
