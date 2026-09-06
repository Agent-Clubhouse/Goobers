//go:build linux

package proc

import (
	"os/exec"

	"golang.org/x/sys/unix"
)

func startBounded(cmd *exec.Cmd, maxBytes uint64, allowRlimitFallback bool) (*Tree, *MemoryBound, error) {
	// The cgroup is prepared BEFORE the child exists, because placement
	// happens at clone time. There is deliberately no window in which the
	// child runs outside its bound.
	cg, cgErr := prepareStageCgroup(maxBytes)
	if cgErr == nil {
		// Configure allocates SysProcAttr and sets Setsid; layering the
		// cgroup fd on afterwards is the documented order for callers that
		// need both (proc.go's Configure doc).
		Configure(cmd)
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = int(cg.dir.Fd())
		prepareStart(cmd)
		if err := cmd.Start(); err != nil {
			_ = cg.release()
			return nil, nil, err
		}
		trackChild(cmd.Process.Pid)
		tree, err := newTree(cmd)
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			_ = cg.release()
			return nil, nil, err
		}
		return tree, &MemoryBound{mechanism: MechanismCgroup, maxBytes: maxBytes, impl: cg}, nil
	}

	if !allowRlimitFallback {
		// Deliberate: an RLIMIT_AS bound is only ever applied to a number an
		// operator chose, never to one derived on their behalf. See the
		// mechanism note in memlimit.go.
		return startUnbounded(cmd, "no delegated cgroup ("+cgErr.Error()+") and the address-space fallback is not enabled")
	}

	tree, err := Start(cmd)
	if err != nil {
		return nil, nil, err
	}
	limit := unix.Rlimit{Cur: maxBytes, Max: maxBytes}
	if err := unix.Prlimit(cmd.Process.Pid, unix.RLIMIT_AS, &limit, nil); err != nil {
		// The child is already running; failing the stage over an unapplied
		// bound would be worse than running it as every previous release did.
		return tree, unenforcedBound("no delegated cgroup and RLIMIT_AS could not be applied: " + err.Error()), nil
	}
	return tree, &MemoryBound{mechanism: MechanismRlimitAS, maxBytes: maxBytes, impl: &rlimitBound{}}, nil
}
