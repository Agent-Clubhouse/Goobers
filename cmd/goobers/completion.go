package main

import (
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/goobers/goobers/internal/instance"
)

const recentCompletionRunLimit = 100

func runCompletion(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		completionUsage(stdout)
		return 0
	}
	completionUsage(stderr)
	return 2
}

func runCompletionScript(script string, args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		completionUsage(stderr)
		return 2
	}
	pf(stdout, "%s", script)
	return 0
}

func completionUsage(w io.Writer) {
	pf(w, "Usage: goobers completion <bash|zsh|fish>\n\n"+
		"Generate a shell completion script. Source the output in the target shell.\n")
}

// runCompletionCandidates is intentionally hidden from help. Generated scripts
// call it on every dynamic completion request, so all discovery failures are
// silent and leave the shell prompt usable.
func runCompletionCandidates(args []string, stdout io.Writer) int {
	if len(args) != 1 {
		return 0
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 0
	}
	for _, candidate := range completionCandidates(args[0], cwd) {
		pln(stdout, candidate)
	}
	return 0
}

func completionCandidates(kind, start string) []string {
	root, ok := completionInstanceRoot(start)
	if !ok {
		return nil
	}
	layout := instance.NewLayout(root)

	switch kind {
	case "workflows":
		set, _, err := instance.LoadConfigDir(layout.ConfigDir())
		if err != nil {
			return nil
		}
		names := make([]string, 0, len(set.Workflows))
		seen := make(map[string]struct{}, len(set.Workflows))
		for _, workflow := range set.Workflows {
			if workflow.Name == "" {
				continue
			}
			if _, exists := seen[workflow.Name]; exists {
				continue
			}
			seen[workflow.Name] = struct{}{}
			names = append(names, workflow.Name)
		}
		sort.Strings(names)
		return names
	case "runs":
		runs, err := listRuns(layout.RunsDir())
		if err != nil {
			return nil
		}
		return recentCompletionRunIDs(runs)
	default:
		return nil
	}
}

func completionInstanceRoot(start string) (string, bool) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	for {
		info, err := os.Stat(filepath.Join(dir, instance.ConfigFileName))
		if err == nil && !info.IsDir() {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func recentCompletionRunIDs(runs []runSummary) []string {
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].StartedAt.Equal(runs[j].StartedAt) {
			return runs[i].RunID < runs[j].RunID
		}
		return runs[i].StartedAt.After(runs[j].StartedAt)
	})
	if len(runs) > recentCompletionRunLimit {
		runs = runs[:recentCompletionRunLimit]
	}
	ids := make([]string, len(runs))
	for i := range runs {
		ids[i] = runs[i].RunID
	}
	return ids
}

const bashCompletion = `# bash completion for goobers
_goobers_completion()
{
    local cur command candidates flags dynamic
    cur="${COMP_WORDS[COMP_CWORD]}"
    dynamic=0

    if (( COMP_CWORD == 1 )); then
        candidates="init scaffold validate up dashboard run signal workflow runs status stats trace telemetry telemetry-query journal backlog-query push-branch open-pr issue-close-out reset-rate-limit merge-pr merge-queue-poll pr-select gather-sibling-context apply-verdict post-merge gather-pr-context rebase-pr remediation-checkpoint completion version help --version -h --help"
        COMPREPLY=( $(compgen -W "${candidates}" -- "${cur}") )
        return
    fi

    command="${COMP_WORDS[1]}"
    flags="-h --help"
    case "${command}" in
        validate) flags+=" --check-harness --source-tree" ;;
        up) flags+=" --quiet" ;;
        dashboard) flags+=" --port --no-open --dev-assets" ;;
        run) flags+=" --no-wait" ;;
        workflow)
            [[ "${COMP_WORDS[2]:-}" == "show" ]] && flags+=" --dot"
            ;;
        runs)
            case "${COMP_WORDS[2]:-}" in
                list) flags+=" --json --limit" ;;
                du) flags+=" --json" ;;
            esac
            ;;
        status) flags+=" --daemon --json --phase --workflow --limit --watch --interval" ;;
        stats) flags+=" --since --json" ;;
        trace) flags+=" --json --transcripts --transcript" ;;
        telemetry)
            case "${COMP_WORDS[2]:-}" in
                stats) flags+=" --workflow --gaggle --rebuild" ;;
                errors) flags+=" --workflow --gaggle --class --limit --rebuild" ;;
            esac
            ;;
        journal)
            [[ "${COMP_WORDS[2]:-}" == "redact" ]] && flags+=" --run --path --reason --secret-file"
            ;;
        backlog-query) flags+=" --claim --release" ;;
        apply-verdict) flags+=" --gate" ;;
        telemetry-query) flags+=" --window" ;;
        remediation-checkpoint) flags+=" --budget" ;;
        scaffold)
            [[ "${COMP_WORDS[2]:-}" == "goober" || "${COMP_WORDS[2]:-}" == "workflow" ]] && flags+=" --force"
            ;;
    esac
    if [[ "${cur}" == -* ]]; then
        COMPREPLY=( $(compgen -W "${flags}" -- "${cur}") )
        return
    fi

    candidates=""
    case "${command}" in
        completion)
            (( COMP_CWORD == 2 )) && candidates="bash zsh fish"
            ;;
        scaffold)
            (( COMP_CWORD == 2 )) && candidates="goober workflow"
            ;;
        run)
            if (( COMP_CWORD == 2 )); then
                dynamic=1
                candidates="abort $(command goobers __complete workflows 2>/dev/null)"
            elif [[ "${COMP_WORDS[2]:-}" == "abort" ]] && (( COMP_CWORD == 3 )); then
                dynamic=1
                candidates="$(command goobers __complete runs 2>/dev/null)"
            fi
            ;;
        trace)
            if (( COMP_CWORD == 2 )); then
                dynamic=1
                candidates="$(command goobers __complete runs 2>/dev/null)"
            fi
            ;;
        workflow)
            if (( COMP_CWORD == 2 )); then
                candidates="show"
            elif [[ "${COMP_WORDS[2]:-}" == "show" ]] && (( COMP_CWORD == 3 )); then
                dynamic=1
                candidates="$(command goobers __complete workflows 2>/dev/null)"
            fi
            ;;
        runs)
            (( COMP_CWORD == 2 )) && candidates="list du"
            ;;
        telemetry)
            (( COMP_CWORD == 2 )) && candidates="stats errors"
            ;;
        journal)
            (( COMP_CWORD == 2 )) && candidates="redact"
            ;;
    esac

    if (( dynamic == 1 )) || [[ -n "${candidates}" ]]; then
        COMPREPLY=( $(compgen -W "${candidates}" -- "${cur}") )
        return
    fi

    compopt -o default
}

complete -F _goobers_completion goobers
`

const zshCompletion = `#compdef goobers

(( $+functions[compdef] )) || {
    autoload -Uz compinit
    compinit
}

_goobers_completion()
{
    local command
    local -a commands flags candidates

    if (( CURRENT == 2 )); then
        commands=(
            'init:scaffold an instance root'
            'scaffold:scaffold a goober or workflow in a gaggle'
            'validate:validate instance configuration'
            'up:run the scheduler and runner daemon'
            'dashboard:serve and open the local operations portal'
            'run:trigger or abort a run'
            'signal:fire an external signal'
            'workflow:inspect workflows'
            'runs:list runs'
            'status:show run status'
            'stats:show the instance lifetime summary'
            'trace:show a run journal'
            'telemetry:query telemetry'
            'telemetry-query:emit telemetry signals'
            'journal:manage run journals'
            'backlog-query:query or claim backlog work'
            'push-branch:push the current run branch'
            'open-pr:open or update the current run pull request'
            'issue-close-out:close the claimed issue'
            'reset-rate-limit:reset the run rate limit'
            'merge-pr:merge an eligible pull request'
            'merge-queue-poll:watch an enqueued pull request until the merge queue resolves it'
            'pr-select:select a pull request'
            'gather-sibling-context:gather sibling pull request context'
            'apply-verdict:apply a review verdict'
            'post-merge:perform post-merge updates'
            'gather-pr-context:gather pull request context'
            'rebase-pr:rebase a pull request'
            'remediation-checkpoint:record a remediation checkpoint'
            'completion:generate shell completion'
            'version:print the version'
            'help:show help'
            '--version:print the version'
            '-h:show help'
            '--help:show help'
        )
        _describe 'command' commands
        return
    fi

    command="${words[2]}"
    flags=(-h --help)
    case "${command}" in
        validate) flags+=(--check-harness --source-tree) ;;
        up) flags+=(--quiet) ;;
        dashboard) flags+=(--port --no-open --dev-assets) ;;
        run) flags+=(--no-wait) ;;
        workflow)
            [[ "${words[3]:-}" == "show" ]] && flags+=(--dot)
            ;;
        runs)
            case "${words[3]:-}" in
                list) flags+=(--json --limit) ;;
                du) flags+=(--json) ;;
            esac
            ;;
        status) flags+=(--daemon --json --phase --workflow --limit --watch --interval) ;;
        stats) flags+=(--since --json) ;;
        trace) flags+=(--json --transcripts --transcript) ;;
        telemetry)
            case "${words[3]:-}" in
                stats) flags+=(--workflow --gaggle --rebuild) ;;
                errors) flags+=(--workflow --gaggle --class --limit --rebuild) ;;
            esac
            ;;
        journal)
            [[ "${words[3]:-}" == "redact" ]] && flags+=(--run --path --reason --secret-file)
            ;;
        backlog-query) flags+=(--claim --release) ;;
        apply-verdict) flags+=(--gate) ;;
        telemetry-query) flags+=(--window) ;;
        remediation-checkpoint) flags+=(--budget) ;;
        scaffold)
            [[ "${words[3]:-}" == "goober" || "${words[3]:-}" == "workflow" ]] && flags+=(--force)
            ;;
    esac
    if [[ "${PREFIX}" == -* ]]; then
        compadd -a flags
        return
    fi

    case "${command}" in
        completion)
            if (( CURRENT == 3 )); then
                candidates=(bash zsh fish)
                _describe 'shell' candidates
                return
            fi
            ;;
        scaffold)
            if (( CURRENT == 3 )); then
                candidates=(goober workflow)
                _describe 'command' candidates
                return
            fi
            ;;
        run)
            if (( CURRENT == 3 )); then
                candidates=(abort)
                candidates+=("${(@f)$(command goobers __complete workflows 2>/dev/null)}")
                _describe -V 'workflow' candidates
                return
            elif [[ "${words[3]:-}" == "abort" ]] && (( CURRENT == 4 )); then
                candidates=("${(@f)$(command goobers __complete runs 2>/dev/null)}")
                _describe -V 'run id' candidates
                return
            fi
            ;;
        trace)
            if (( CURRENT == 3 )); then
                candidates=("${(@f)$(command goobers __complete runs 2>/dev/null)}")
                _describe -V 'run id' candidates
                return
            fi
            ;;
        workflow)
            if (( CURRENT == 3 )); then
                candidates=(show)
                _describe 'command' candidates
                return
            elif [[ "${words[3]:-}" == "show" ]] && (( CURRENT == 4 )); then
                candidates=("${(@f)$(command goobers __complete workflows 2>/dev/null)}")
                _describe 'workflow' candidates
                return
            fi
            ;;
        runs)
            if (( CURRENT == 3 )); then
                candidates=(list du)
                _describe 'command' candidates
                return
            fi
            ;;
        telemetry)
            if (( CURRENT == 3 )); then
                candidates=(stats errors)
                _describe 'command' candidates
                return
            fi
            ;;
        journal)
            if (( CURRENT == 3 )); then
                candidates=(redact)
                _describe 'command' candidates
                return
            fi
            ;;
    esac

    _directories
}

compdef _goobers_completion goobers
`

const fishCompletion = `# fish completion for goobers
function __goobers_completion_workflows
    command goobers __complete workflows 2>/dev/null
end

function __goobers_completion_runs
    command goobers __complete runs 2>/dev/null
end

complete -c goobers -e
complete -c goobers -n '__fish_use_subcommand' -f -a 'init scaffold validate up dashboard run signal workflow runs status stats trace telemetry telemetry-query journal backlog-query push-branch open-pr issue-close-out reset-rate-limit merge-pr merge-queue-poll pr-select gather-sibling-context apply-verdict post-merge gather-pr-context rebase-pr remediation-checkpoint completion version help'
complete -c goobers -s h -l help -d 'Show help'
complete -c goobers -l version -d 'Print the version'

complete -c goobers -n '__fish_seen_subcommand_from completion; and test (count (commandline -opc)) -eq 2' -f -a 'bash zsh fish'
complete -c goobers -n '__fish_seen_subcommand_from scaffold; and test (count (commandline -opc)) -eq 2' -f -a 'goober workflow'

complete -c goobers -n '__fish_seen_subcommand_from run; and test (count (commandline -opc)) -eq 2' -f -k -a 'abort (__goobers_completion_workflows)'
complete -c goobers -n '__fish_seen_subcommand_from run; and __fish_seen_subcommand_from abort; and test (count (commandline -opc)) -eq 3' -f -k -a '(__goobers_completion_runs)'
complete -c goobers -n '__fish_seen_subcommand_from trace; and test (count (commandline -opc)) -eq 2' -f -k -a '(__goobers_completion_runs)'

complete -c goobers -n '__fish_seen_subcommand_from workflow; and test (count (commandline -opc)) -eq 2' -f -a 'show'
complete -c goobers -n '__fish_seen_subcommand_from workflow; and __fish_seen_subcommand_from show; and test (count (commandline -opc)) -eq 3' -f -a '(__goobers_completion_workflows)'
complete -c goobers -n '__fish_seen_subcommand_from runs; and test (count (commandline -opc)) -eq 2' -f -a 'list du'
complete -c goobers -n '__fish_seen_subcommand_from telemetry; and test (count (commandline -opc)) -eq 2' -f -a 'stats errors'
complete -c goobers -n '__fish_seen_subcommand_from journal; and test (count (commandline -opc)) -eq 2' -f -a 'redact'

complete -c goobers -n '__fish_seen_subcommand_from scaffold' -l force -d 'Replace generated files that already exist'
complete -c goobers -n '__fish_seen_subcommand_from validate' -l check-harness -d 'Check agent harnesses'
complete -c goobers -n '__fish_seen_subcommand_from validate' -l source-tree -d 'Validate a checked-in config source tree'
complete -c goobers -n '__fish_seen_subcommand_from up' -l quiet -d 'Suppress liveness heartbeats'
complete -c goobers -n '__fish_seen_subcommand_from dashboard' -l port -r -d 'Dashboard port or auto'
complete -c goobers -n '__fish_seen_subcommand_from dashboard' -l no-open -d 'Print the URL without opening a browser'
complete -c goobers -n '__fish_seen_subcommand_from dashboard' -l dev-assets -r -d 'Serve a local portal build'
complete -c goobers -n '__fish_seen_subcommand_from workflow; and __fish_seen_subcommand_from show' -l dot -d 'Emit Graphviz DOT'
complete -c goobers -n '__fish_seen_subcommand_from runs; and __fish_seen_subcommand_from list' -l json -d 'Emit JSON'
complete -c goobers -n '__fish_seen_subcommand_from runs; and __fish_seen_subcommand_from list' -l limit -r -d 'Maximum runs'
complete -c goobers -n '__fish_seen_subcommand_from runs; and __fish_seen_subcommand_from du' -l json -d 'Emit JSON'
complete -c goobers -n '__fish_seen_subcommand_from status' -l daemon -d 'Report daemon health and identity'
complete -c goobers -n '__fish_seen_subcommand_from status' -l json -d 'Emit JSON'
complete -c goobers -n '__fish_seen_subcommand_from status' -l phase -r -d 'Filter by phase'
complete -c goobers -n '__fish_seen_subcommand_from status' -l workflow -r -a '(__goobers_completion_workflows)' -d 'Filter by workflow'
complete -c goobers -n '__fish_seen_subcommand_from status' -l limit -r -d 'Maximum runs'
complete -c goobers -n '__fish_seen_subcommand_from status' -l watch -d 'Refresh the status board until interrupted'
complete -c goobers -n '__fish_seen_subcommand_from status' -l interval -r -d 'Watch refresh interval'
complete -c goobers -n '__fish_seen_subcommand_from stats' -l since -r -d 'Only include activity from the preceding duration'
complete -c goobers -n '__fish_seen_subcommand_from stats' -l json -d 'Emit JSON'
complete -c goobers -n '__fish_seen_subcommand_from run' -l no-wait -d 'Return after the run is dispatched'
complete -c goobers -n '__fish_seen_subcommand_from trace' -l json -d 'Emit JSON'
complete -c goobers -n '__fish_seen_subcommand_from trace' -l transcripts -d 'Show every recorded agent-stage transcript'
complete -c goobers -n '__fish_seen_subcommand_from trace' -l transcript -r -d 'Show recorded transcript data for one stage'
complete -c goobers -n '__fish_seen_subcommand_from telemetry; and __fish_seen_subcommand_from stats errors' -l workflow -r -a '(__goobers_completion_workflows)' -d 'Filter by workflow'
complete -c goobers -n '__fish_seen_subcommand_from telemetry; and __fish_seen_subcommand_from stats errors' -l gaggle -r -d 'Filter by gaggle'
complete -c goobers -n '__fish_seen_subcommand_from telemetry; and __fish_seen_subcommand_from errors' -l class -r -d 'Filter by error class'
complete -c goobers -n '__fish_seen_subcommand_from telemetry; and __fish_seen_subcommand_from errors' -l limit -r -d 'Maximum errors'
complete -c goobers -n '__fish_seen_subcommand_from telemetry; and __fish_seen_subcommand_from stats errors' -l rebuild -d 'Rebuild telemetry'
complete -c goobers -n '__fish_seen_subcommand_from journal; and __fish_seen_subcommand_from redact' -l run -r -a '(__goobers_completion_runs)' -d 'Run id'
complete -c goobers -n '__fish_seen_subcommand_from journal; and __fish_seen_subcommand_from redact' -l path -r -d 'Journal path'
complete -c goobers -n '__fish_seen_subcommand_from journal; and __fish_seen_subcommand_from redact' -l reason -r -d 'Redaction reason'
complete -c goobers -n '__fish_seen_subcommand_from journal; and __fish_seen_subcommand_from redact' -l secret-file -r -d 'Secret file'
complete -c goobers -n '__fish_seen_subcommand_from backlog-query' -l claim -d 'Claim an item'
complete -c goobers -n '__fish_seen_subcommand_from backlog-query' -l release -d 'Release a claim'
complete -c goobers -n '__fish_seen_subcommand_from apply-verdict' -l gate -r -d 'Gate name'
complete -c goobers -n '__fish_seen_subcommand_from telemetry-query' -l window -r -d 'Lookback window'
complete -c goobers -n '__fish_seen_subcommand_from remediation-checkpoint' -l budget -r -d 'Repass budget'
`
