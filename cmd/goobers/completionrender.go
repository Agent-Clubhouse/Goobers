package main

import (
	"fmt"
	"strings"
)

// This file renders the bash, zsh, and fish completion scripts from the
// completionModel (which derives its command tree from the cliCommand
// registry). The three renderers share the same model, so the CLI surface is
// described once and cannot diverge between shells — replacing the three
// hand-maintained script constants that previously duplicated it.

// dashFlags turns flag specs into "--name" tokens.
func dashFlags(flags []completionFlagSpec) []string {
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		out = append(out, "--"+f.name)
	}
	return out
}

// subFlagsBeyondParent returns a sub's flags that its parent does not already
// offer, so a subcommand context never re-lists a flag the parent case already
// appended (e.g. `escalations` and `escalations show` both carry --json).
func subFlagsBeyondParent(parent, sub completionCommand) []completionFlagSpec {
	have := make(map[string]bool, len(parent.flags))
	for _, f := range parent.flags {
		have[f.name] = true
	}
	var out []completionFlagSpec
	for _, f := range sub.flags {
		if !have[f.name] {
			out = append(out, f)
		}
	}
	return out
}

// subNames lists a command's subcommand names in registry order.
func subNames(c completionCommand) []string {
	out := make([]string, 0, len(c.subs))
	for _, s := range c.subs {
		out = append(out, s.name)
	}
	return out
}

func completeCall(kind string) string {
	return fmt.Sprintf("$(command goobers __complete %s 2>/dev/null)", kind)
}

// commandNames lists the top-level command word forms in registry order.
func commandNames(m completionModel) []string {
	out := make([]string, 0, len(m.commands))
	for _, c := range m.commands {
		out = append(out, c.name)
	}
	return out
}

// --- bash ------------------------------------------------------------------

func renderBashCompletion(m completionModel) string {
	var b strings.Builder
	b.WriteString("# bash completion for goobers\n")
	b.WriteString("_goobers_completion()\n{\n")
	b.WriteString("    local cur command candidates flags dynamic\n")
	b.WriteString(`    cur="${COMP_WORDS[COMP_CWORD]}"` + "\n")
	b.WriteString("    dynamic=0\n\n")

	topLevel := append(append([]string{}, commandNames(m)...), m.globalFlags...)
	b.WriteString("    if (( COMP_CWORD == 1 )); then\n")
	fmt.Fprintf(&b, "        candidates=%q\n", strings.Join(topLevel, " "))
	b.WriteString("        COMPREPLY=( $(compgen -W \"${candidates}\" -- \"${cur}\") )\n")
	b.WriteString("        return\n    fi\n\n")

	b.WriteString(`    command="${COMP_WORDS[1]}"` + "\n")
	b.WriteString(`    flags="-h --help"` + "\n")
	b.WriteString("    case \"${command}\" in\n")
	for _, c := range m.commands {
		b.WriteString(bashFlagArm(c))
	}
	b.WriteString("    esac\n")
	b.WriteString(`    if [[ "${cur}" == -* ]]; then` + "\n")
	b.WriteString("        COMPREPLY=( $(compgen -W \"${flags}\" -- \"${cur}\") )\n")
	b.WriteString("        return\n    fi\n\n")

	b.WriteString(`    candidates=""` + "\n")
	b.WriteString("    case \"${command}\" in\n")
	for _, c := range m.commands {
		b.WriteString(bashCandidateArm(c))
	}
	b.WriteString("    esac\n\n")

	b.WriteString("    if (( dynamic == 1 )) || [[ -n \"${candidates}\" ]]; then\n")
	b.WriteString("        COMPREPLY=( $(compgen -W \"${candidates}\" -- \"${cur}\") )\n")
	b.WriteString("        return\n    fi\n\n")
	b.WriteString("    compopt -o default\n")
	b.WriteString("}\n\n")
	b.WriteString("complete -F _goobers_completion goobers\n")
	return b.String()
}

func bashFlagArm(c completionCommand) string {
	direct := dashFlags(c.flags)
	var subArms []string
	for _, s := range c.subs {
		extra := dashFlags(subFlagsBeyondParent(c, s))
		if len(extra) == 0 {
			continue
		}
		subArms = append(subArms, fmt.Sprintf("                %s) flags+=\" %s\" ;;\n", s.name, strings.Join(extra, " ")))
	}
	if len(direct) == 0 && len(subArms) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("        " + c.name + ")\n")
	if len(direct) > 0 {
		fmt.Fprintf(&b, "            flags+=\" %s\"\n", strings.Join(direct, " "))
	}
	if len(subArms) > 0 {
		b.WriteString("            case \"${COMP_WORDS[2]:-}\" in\n")
		for _, arm := range subArms {
			b.WriteString(arm)
		}
		b.WriteString("            esac\n")
	}
	b.WriteString("            ;;\n")
	return b.String()
}

func bashCandidateArm(c completionCommand) string {
	statics := subNames(c)
	var subDyn []completionCommand
	for _, s := range c.subs {
		if s.argKind != "" {
			subDyn = append(subDyn, s)
		}
	}
	if len(statics) == 0 && c.argKind == "" && len(subDyn) == 0 {
		return ""
	}
	var parts []string
	parts = append(parts, statics...)
	if c.argKind != "" {
		parts = append(parts, completeCall(c.argKind))
	}
	var b strings.Builder
	b.WriteString("        " + c.name + ")\n")
	b.WriteString("            if (( COMP_CWORD == 2 )); then\n")
	if c.argKind != "" {
		b.WriteString("                dynamic=1\n")
	}
	fmt.Fprintf(&b, "                candidates=%q\n", strings.Join(parts, " "))
	for _, s := range subDyn {
		fmt.Fprintf(&b, "            elif [[ \"${COMP_WORDS[2]:-}\" == %q ]] && (( COMP_CWORD == 3 )); then\n", s.name)
		b.WriteString("                dynamic=1\n")
		fmt.Fprintf(&b, "                candidates=%q\n", completeCall(s.argKind))
	}
	b.WriteString("            fi\n")
	b.WriteString("            ;;\n")
	return b.String()
}

// --- zsh -------------------------------------------------------------------

func renderZshCompletion(m completionModel) string {
	var b strings.Builder
	b.WriteString("#compdef goobers\n\n")
	b.WriteString("(( $+functions[compdef] )) || {\n    autoload -Uz compinit\n    compinit\n}\n\n")
	b.WriteString("_goobers_completion()\n{\n")
	b.WriteString("    local command\n")
	b.WriteString("    local -a commands flags candidates\n\n")

	b.WriteString("    if (( CURRENT == 2 )); then\n")
	b.WriteString("        commands=(\n")
	for _, c := range m.commands {
		fmt.Fprintf(&b, "            %s\n", zshDescribeItem(c.name, c.desc))
	}
	for _, gf := range m.globalFlags {
		fmt.Fprintf(&b, "            %s\n", zshDescribeItem(gf, globalFlagDesc(gf)))
	}
	b.WriteString("        )\n")
	b.WriteString("        _describe 'command' commands\n")
	b.WriteString("        return\n    fi\n\n")

	b.WriteString(`    command="${words[2]}"` + "\n")
	b.WriteString("    flags=(-h --help)\n")
	b.WriteString("    case \"${command}\" in\n")
	for _, c := range m.commands {
		b.WriteString(zshFlagArm(c))
	}
	b.WriteString("    esac\n")
	b.WriteString(`    if [[ "${PREFIX}" == -* ]]; then` + "\n")
	b.WriteString("        compadd -a flags\n")
	b.WriteString("        return\n    fi\n\n")

	b.WriteString("    case \"${command}\" in\n")
	for _, c := range m.commands {
		b.WriteString(zshCandidateArm(c))
	}
	b.WriteString("    esac\n\n")
	b.WriteString("    _directories\n")
	b.WriteString("}\n\n")
	b.WriteString("compdef _goobers_completion goobers\n")
	return b.String()
}

func zshDescribeItem(name, desc string) string {
	if desc == "" {
		return "'" + name + "'"
	}
	return "'" + name + ":" + zshEscape(desc) + "'"
}

func zshEscape(s string) string {
	s = strings.ReplaceAll(s, "'", `'\''`)
	s = strings.ReplaceAll(s, ":", `\:`)
	return s
}

func globalFlagDesc(flag string) string {
	switch flag {
	case "--version":
		return "print the version"
	case "-h", "--help":
		return "show help"
	default:
		return ""
	}
}

func zshFlagArm(c completionCommand) string {
	direct := dashFlags(c.flags)
	var subArms []string
	for _, s := range c.subs {
		extra := dashFlags(subFlagsBeyondParent(c, s))
		if len(extra) == 0 {
			continue
		}
		subArms = append(subArms, fmt.Sprintf("                %s) flags+=(%s) ;;\n", s.name, strings.Join(extra, " ")))
	}
	if len(direct) == 0 && len(subArms) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("        " + c.name + ")\n")
	if len(direct) > 0 {
		fmt.Fprintf(&b, "            flags+=(%s)\n", strings.Join(direct, " "))
	}
	if len(subArms) > 0 {
		b.WriteString("            case \"${words[3]:-}\" in\n")
		for _, arm := range subArms {
			b.WriteString(arm)
		}
		b.WriteString("            esac\n")
	}
	b.WriteString("            ;;\n")
	return b.String()
}

func zshCandidateArm(c completionCommand) string {
	statics := subNames(c)
	var subDyn []completionCommand
	for _, s := range c.subs {
		if s.argKind != "" {
			subDyn = append(subDyn, s)
		}
	}
	if len(statics) == 0 && c.argKind == "" && len(subDyn) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("        " + c.name + ")\n")
	b.WriteString("            if (( CURRENT == 3 )); then\n")
	if len(statics) > 0 {
		fmt.Fprintf(&b, "                candidates=(%s)\n", strings.Join(statics, " "))
	} else {
		b.WriteString("                candidates=()\n")
	}
	if c.argKind != "" {
		fmt.Fprintf(&b, "                candidates+=(\"${(@f)%s}\")\n", completeCall(c.argKind))
	}
	fmt.Fprintf(&b, "                _describe -V %s candidates\n", zshLabel(c.argKind))
	b.WriteString("                return\n")
	for _, s := range subDyn {
		fmt.Fprintf(&b, "            elif [[ \"${words[3]:-}\" == %q ]] && (( CURRENT == 4 )); then\n", s.name)
		fmt.Fprintf(&b, "                candidates=(\"${(@f)%s}\")\n", completeCall(s.argKind))
		fmt.Fprintf(&b, "                _describe -V %s candidates\n", zshLabel(s.argKind))
		b.WriteString("                return\n")
	}
	b.WriteString("            fi\n")
	b.WriteString("            ;;\n")
	return b.String()
}

func zshLabel(argKind string) string {
	switch argKind {
	case "workflows":
		return "'workflow'"
	case "runs":
		return "'run id'"
	case "escalations":
		return "'escalated run id'"
	default:
		return "'command'"
	}
}

// --- fish ------------------------------------------------------------------

func renderFishCompletion(m completionModel) string {
	var b strings.Builder
	b.WriteString("# fish completion for goobers\n")
	for _, kind := range []string{"workflows", "runs", "escalations"} {
		fmt.Fprintf(&b, "function __goobers_completion_%s\n", kind)
		fmt.Fprintf(&b, "    command goobers __complete %s 2>/dev/null\n", kind)
		b.WriteString("end\n\n")
	}

	b.WriteString("complete -c goobers -e\n")
	fmt.Fprintf(&b, "complete -c goobers -n '__fish_use_subcommand' -f -a '%s'\n", strings.Join(commandNames(m), " "))
	b.WriteString("complete -c goobers -s h -l help -d 'Show help'\n")
	b.WriteString("complete -c goobers -l version -d 'Print the version'\n\n")

	// Subcommand and positional-argument candidates.
	for _, c := range m.commands {
		b.WriteString(fishCandidateRules(c))
	}
	b.WriteString("\n")

	// Flags.
	for _, c := range m.commands {
		b.WriteString(fishFlagRules(c))
	}
	return b.String()
}

func fishCandidateRules(c completionCommand) string {
	statics := subNames(c)
	var b strings.Builder
	// Depth-1 candidates for command c (count of tokens before cursor == 2).
	var parts []string
	parts = append(parts, statics...)
	keep := false
	if c.argKind != "" {
		parts = append(parts, fmt.Sprintf("(__goobers_completion_%s)", c.argKind))
		// Keep order when a static subcommand is mixed ahead of the dynamic
		// list (so e.g. "abort" stays before run ids/workflows), or when the
		// dynamic list is intrinsically ordered (newest-first run ids).
		keep = len(statics) > 0 || fishKeepOrder(c.argKind)
	}
	if len(parts) > 0 {
		fmt.Fprintf(&b, "complete -c goobers -n '__fish_seen_subcommand_from %s; and test (count (commandline -opc)) -eq 2' -f %s-a '%s'\n",
			c.name, keepFlag(keep), strings.Join(parts, " "))
	}
	// Depth-2 dynamic candidates for subcommands with an arg kind.
	for _, s := range c.subs {
		if s.argKind == "" {
			continue
		}
		fmt.Fprintf(&b, "complete -c goobers -n '__fish_seen_subcommand_from %s; and __fish_seen_subcommand_from %s; and test (count (commandline -opc)) -eq 3' -f %s-a '(__goobers_completion_%s)'\n",
			c.name, s.name, keepFlag(fishKeepOrder(s.argKind)), s.argKind)
	}
	return b.String()
}

func fishFlagRules(c completionCommand) string {
	var b strings.Builder
	for _, f := range c.flags {
		b.WriteString(fishFlagRule([]string{c.name}, f))
	}
	for _, s := range c.subs {
		for _, f := range subFlagsBeyondParent(c, s) {
			b.WriteString(fishFlagRule([]string{c.name, s.name}, f))
		}
	}
	return b.String()
}

func fishFlagRule(path []string, f completionFlagSpec) string {
	conds := make([]string, 0, len(path))
	for _, p := range path {
		conds = append(conds, "__fish_seen_subcommand_from "+p)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "complete -c goobers -n '%s' -l %s", strings.Join(conds, "; and "), f.name)
	if f.takesArg {
		b.WriteString(" -r")
	}
	switch {
	case f.valueKind != "":
		fmt.Fprintf(&b, " -a '(__goobers_completion_%s)'", f.valueKind)
	case len(f.values) > 0:
		fmt.Fprintf(&b, " -a '%s'", strings.Join(f.values, " "))
	}
	if f.desc != "" {
		fmt.Fprintf(&b, " -d '%s'", fishEscape(f.desc))
	}
	b.WriteString("\n")
	return b.String()
}

func fishEscape(s string) string {
	return strings.ReplaceAll(s, "'", `\'`)
}

func keepFlag(keep bool) string {
	if keep {
		return "-k "
	}
	return ""
}

// fishKeepOrder reports whether a dynamic candidate list is intrinsically
// ordered (newest-first run ids) and must not be re-sorted by fish.
func fishKeepOrder(argKind string) bool {
	return argKind == "runs" || argKind == "escalations"
}

// --- powershell ------------------------------------------------------------

func renderPowerShellCompletion(m completionModel) string {
	var b strings.Builder
	b.WriteString("# PowerShell completion for goobers\n")
	b.WriteString("Register-ArgumentCompleter -Native -CommandName @('goobers') -ScriptBlock {\n")
	b.WriteString("    param($wordToComplete, $commandAst, $cursorPosition)\n\n")
	b.WriteString("    $completions = @(\n")
	for _, completion := range append(append([]string{}, commandNames(m)...), m.globalFlags...) {
		fmt.Fprintf(&b, "        '%s'\n", completion)
	}
	b.WriteString("    )\n\n")
	b.WriteString("    $elements = @($commandAst.CommandElements | Select-Object -Skip 1 | ForEach-Object {\n")
	b.WriteString("        if ($_.Extent) { $_.Extent.Text } else { \"$_\" }\n")
	b.WriteString("    })\n")
	b.WriteString("    if ($elements.Count -gt 1) {\n")
	b.WriteString("        return\n")
	b.WriteString("    }\n\n")
	b.WriteString("    $pattern = if ([string]::IsNullOrEmpty($wordToComplete)) {\n")
	b.WriteString("        '*'\n")
	b.WriteString("    } else {\n")
	b.WriteString("        [System.Management.Automation.WildcardPattern]::Escape($wordToComplete) + '*'\n")
	b.WriteString("    }\n")
	b.WriteString("    $completions | Where-Object { $_ -like $pattern } | ForEach-Object {\n")
	b.WriteString("        [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)\n")
	b.WriteString("    }\n")
	b.WriteString("}\n")
	return b.String()
}
