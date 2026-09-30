package main

import (
	"fmt"
	"os/exec"
	"time"
)

type processSampler struct {
	at      time.Time
	seconds float64
	handles int64
}

func (s *processSampler) sample(pid int) (int64, float64, error) {
	command := fmt.Sprintf(`$p=Get-Process -Id %d; [Console]::WriteLine('{0} {1} {2}',[long]($p.WorkingSet64/1024),$p.TotalProcessorTime.TotalSeconds,$p.HandleCount)`, pid)
	data, err := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", command).Output()
	if err != nil {
		return 0, 0, err
	}
	var rss int64
	var seconds, cpu float64
	if _, err = fmt.Sscan(string(data), &rss, &seconds, &s.handles); err != nil {
		return 0, 0, err
	}
	now := time.Now()
	if !s.at.IsZero() {
		cpu = (seconds - s.seconds) / now.Sub(s.at).Seconds() * 100
	}
	s.at, s.seconds = now, seconds
	return rss, cpu, nil
}
