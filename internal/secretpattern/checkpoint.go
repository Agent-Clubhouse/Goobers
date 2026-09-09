package secretpattern

import (
	"regexp/syntax"
	"strings"
	"unicode/utf8"
)

// SafePrefix returns a conservative boundary at which a checkpoint can split
// input before Scrub is applied. No default-pattern match can cross this
// boundary, including a match completed by bytes that have not arrived yet.
// An unfinished token or PEM block is deliberately retained, not written raw.
// This covers only the pattern net; exact-value registries need their own
// boundary, and callers composing scrubbers must honor every member.
func (s *Scrubber) SafePrefix(input []byte) int {
	var patterns []string
	for _, p := range s.patterns {
		patterns = append(patterns, "(?:"+p.re.String()+")")
	}
	if len(patterns) == 0 {
		return len(input)
	}
	re, err := syntax.Parse(strings.Join(patterns, "|"), syntax.Perl)
	if err != nil {
		return 0
	}
	program, err := syntax.Compile(re.Simplify())
	if err != nil {
		return 0
	}
	return checkpointBoundary(program, input)
}

func checkpointBoundary(program *syntax.Prog, input []byte) int {
	active := make([]bool, len(program.Inst))
	next := make([]bool, len(program.Inst))
	safe := 0
	for offset := 0; offset < len(input); {
		if !utf8.FullRune(input[offset:]) {
			break
		}
		r, size := utf8.DecodeRune(input[offset:])
		if !checkpointClosure(program, active, uint32(program.Start)) {
			return safe
		}
		clear(next)
		for pc, present := range active {
			if !present {
				continue
			}
			inst := &program.Inst[pc]
			if checkpointRune(inst, r) && !checkpointClosure(program, next, inst.Out) {
				return safe
			}
		}
		offset += size
		active, next = next, active
		if !checkpointLive(program, active) {
			safe = offset
		}
	}
	return safe
}

// Closure retains visited epsilon instructions too, so cycles terminate.
// Context-sensitive assertions are unsupported and fail closed if a future
// pattern introduces one; treating them as unconditional would risk leakage.
func checkpointClosure(program *syntax.Prog, states []bool, start uint32) bool {
	queue := []uint32{start}
	for len(queue) > 0 {
		pc := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if states[pc] {
			continue
		}
		states[pc] = true
		inst := &program.Inst[pc]
		switch inst.Op {
		case syntax.InstAlt, syntax.InstAltMatch:
			queue = append(queue, inst.Out, inst.Arg)
		case syntax.InstCapture, syntax.InstNop:
			queue = append(queue, inst.Out)
		case syntax.InstEmptyWidth:
			return false
		}
	}
	return true
}

func checkpointRune(inst *syntax.Inst, r rune) bool {
	switch inst.Op {
	case syntax.InstRune, syntax.InstRune1:
		return inst.MatchRune(r)
	case syntax.InstRuneAny:
		return true
	case syntax.InstRuneAnyNotNL:
		return r != '\n'
	default:
		return false
	}
}

func checkpointLive(program *syntax.Prog, states []bool) bool {
	for pc, present := range states {
		if !present {
			continue
		}
		switch program.Inst[pc].Op {
		case syntax.InstRune, syntax.InstRune1, syntax.InstRuneAny, syntax.InstRuneAnyNotNL:
			return true
		}
	}
	return false
}
