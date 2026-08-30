# bash completion for goobers
_goobers_completion()
{
    local cur command candidates flags dynamic
    cur="${COMP_WORDS[COMP_CWORD]}"
    dynamic=0

    if (( COMP_CWORD == 1 )); then
        candidates="version init connect examples scaffold validate up down service dashboard getting-started run signal workflow status stats trace escalations completion help --version -h --help"
        COMPREPLY=( $(compgen -W "${candidates}" -- "${cur}") )
        return
    fi

    command="${COMP_WORDS[1]}"
    flags="-h --help"
    case "${command}" in
        version)
            flags+=" --json"
            ;;
        versions)
            flags+=" --json"
            ;;
        init)
            flags+=" --demo --insecure --guided --template --source-tree --json"
            ;;
        connect)
            flags+=" --token-env --seed --replace --json"
            ;;
        preflight)
            flags+=" --distro --launch-wsl"
            ;;
        onboarding)
            case "${COMP_WORDS[2]:-}" in
                stub-agent-instructions) flags+=" --source-tree --harness --json" ;;
                stub-sample) flags+=" --destination --work-tracking --token-env --force --json" ;;
            esac
            ;;
        scaffold)
            case "${COMP_WORDS[2]:-}" in
                goober) flags+=" --force" ;;
                workflow) flags+=" --force" ;;
                gaggle) flags+=" --force --from" ;;
            esac
            ;;
        agent-kit)
            case "${COMP_WORDS[2]:-}" in
                install) flags+=" --harness" ;;
                update) flags+=" --dry-run --write --replace-modified" ;;
            esac
            ;;
        validate)
            flags+=" --json --github-annotations --check-harness --check-repos --source-tree --strict"
            ;;
        lint)
            flags+=" --json --github-annotations --check-harness --check-repos --source-tree --strict"
            ;;
        fix)
            flags+=" --to --write"
            ;;
        doctor)
            flags+=" --k8s --repo --av-exclusions --work-root --kubeconfig --context --report --oidc-issuer --registry --egress --timeout"
            ;;
        netpol-render)
            flags+=" --out --check --baseline --write-baseline --timeout --print-blob-endpoint"
            ;;
        config)
            case "${COMP_WORDS[2]:-}" in
                diff) flags+=" --against" ;;
                show) flags+=" --json" ;;
            esac
            ;;
        speech)
            case "${COMP_WORDS[2]:-}" in
                preflight) flags+=" --json" ;;
                test) flags+=" --json" ;;
            esac
            ;;
        fleet)
            case "${COMP_WORDS[2]:-}" in
                join) flags+=" --url --enrollment-token-file --grant-local-admin --no-grant-local-admin" ;;
                status) flags+=" --json" ;;
            esac
            ;;
        up)
            flags+=" --quiet --diagnostics --notify --skip-preflight --watch-config --drain-timeout --cleanup-spans-only-runs --disable-read-model-reads"
            ;;
        self-update)
            flags+=" --policy --branch --target --health-ticks --health-timeout"
            ;;
        service)
            case "${COMP_WORDS[2]:-}" in
                status) flags+=" --json" ;;
            esac
            ;;
        engine-start)
            flags+=" --gaggle --temporal-hostport --temporal-namespace --task-queue --dedupe-key --direct --live-journal"
            ;;
        engine-queues)
            flags+=" --temporal-hostport --temporal-namespace --task-queue --timeout --json"
            ;;
        engine-project)
            flags+=" --gaggle --temporal-hostport --temporal-namespace"
            ;;
        worker)
            flags+=" --instance --blob-store --daemon-api --dispatch-namespace --config-reload-interval --task-queue --temporal-hostport --temporal-namespace --drain-timeout --work-root"
            ;;
        dashboard)
            flags+=" --port --listen --no-open --dev-assets --wait-for-daemon"
            ;;
        getting-started)
            flags+=" --port --no-open --workdir"
            ;;
        run)
            flags+=" --gaggle --pr --no-wait"
            ;;
        approve)
            flags+=" --decision --actor"
            ;;
        override)
            flags+=" --rationale --decision --actor"
            ;;
        rerun-stage)
            flags+=" --addendum --actor"
            ;;
        workflow)
            case "${COMP_WORDS[2]:-}" in
                show) flags+=" --dot" ;;
            esac
            ;;
        runs)
            case "${COMP_WORDS[2]:-}" in
                list) flags+=" --json --phase --workflow --gaggle --limit" ;;
                du) flags+=" --json" ;;
            esac
            ;;
        status)
            flags+=" --agents --daemon --json --phase --workflow --gaggle --limit --watch --interval"
            ;;
        stats)
            flags+=" --since --json"
            ;;
        features)
            flags+=" --json --dsl-version --used"
            ;;
        schema)
            flags+=" --list --human"
            ;;
        explain)
            flags+=" --human"
            ;;
        blocked)
            case "${COMP_WORDS[2]:-}" in
                list) flags+=" --json" ;;
            esac
            ;;
        claims)
            case "${COMP_WORDS[2]:-}" in
                list) flags+=" --json --stale --gaggle --provider" ;;
                release) flags+=" --gaggle --provider --force" ;;
            esac
            ;;
        trace)
            flags+=" --json --follow --summary --verdicts --transcripts --transcript"
            ;;
        e2e)
            case "${COMP_WORDS[2]:-}" in
                verify) flags+=" --run --gaggle --expected --out --print-runner-class" ;;
                kill-inject) flags+=" --run --stage --stage-class --namespace --poll-timeout --out" ;;
            esac
            ;;
        escalations)
            flags+=" --json"
            case "${COMP_WORDS[2]:-}" in
                show) flags+=" --include-verdict" ;;
            esac
            ;;
        telemetry)
            case "${COMP_WORDS[2]:-}" in
                stats) flags+=" --json --workflow --gaggle --branch --model --harness-version --group-by --since --until --rebuild" ;;
                errors) flags+=" --json --workflow --gaggle --class --limit --since --until --rebuild" ;;
                export) flags+=" --since --until" ;;
                prune) flags+=" --dry-run" ;;
                prune-orphans) flags+=" --delete --min-age" ;;
                compact) flags+=" --dry-run" ;;
            esac
            ;;
        journal)
            case "${COMP_WORDS[2]:-}" in
                redact) flags+=" --run --path --reason --secret-file" ;;
            esac
            ;;
        backlog-health)
            flags+=" --feedback"
            ;;
        backlog-query)
            flags+=" --claim --debug --release --read-only --reconcile"
            ;;
        file-issues)
            flags+=" --check"
            ;;
        reconcile-branches)
            flags+=" --delete --max --min-age --after"
            ;;
        set-milestone)
            flags+=" --item --milestone"
            ;;
        reconcile-post-merge)
            flags+=" --max --lookback"
            ;;
        telemetry-query)
            flags+=" --window --aggregate --learning-action --threshold --format --gaggle --workflow"
            ;;
        docs-churn)
            flags+=" --repo --workflow --gaggle --since --buffer-multiplier --format"
            ;;
        ios-simulator-test)
            flags+=" --project --workspace --scheme --device --runtime --only-testing --result-bundle"
            ;;
        gather-sibling-context)
            flags+=" --no-cache --no-verdict-cache"
            ;;
        apply-verdict)
            flags+=" --gate"
            ;;
        elect-lander)
            flags+=" --gate"
            ;;
        pr-claim)
            flags+=" --release"
            ;;
        remediation-checkpoint)
            flags+=" --budget --escalate --escalation-outcome"
            ;;
        respond-to-findings)
            flags+=" --check"
            ;;
        mcp-io)
            flags+=" --config"
            ;;
    esac
    if [[ "${cur}" == -* ]]; then
        COMPREPLY=( $(compgen -W "${flags}" -- "${cur}") )
        return
    fi

    candidates=""
    case "${command}" in
        onboarding)
            if (( COMP_CWORD == 2 )); then
                candidates="stub-agent-instructions stub-sample"
            fi
            ;;
        examples)
            if (( COMP_CWORD == 2 )); then
                candidates="list show"
            elif [[ "${COMP_WORDS[2]:-}" == "show" ]] && (( COMP_CWORD == 3 )); then
                dynamic=1
                candidates="$(command goobers __complete examples 2>/dev/null)"
            fi
            ;;
        scaffold)
            if (( COMP_CWORD == 2 )); then
                candidates="goober workflow gaggle"
            fi
            ;;
        agent-kit)
            if (( COMP_CWORD == 2 )); then
                candidates="install check update"
            fi
            ;;
        config)
            if (( COMP_CWORD == 2 )); then
                candidates="diff materialize show"
            fi
            ;;
        speech)
            if (( COMP_CWORD == 2 )); then
                candidates="preflight test"
            fi
            ;;
        fleet)
            if (( COMP_CWORD == 2 )); then
                candidates="join status leave"
            fi
            ;;
        service)
            if (( COMP_CWORD == 2 )); then
                candidates="install uninstall stop start status"
            fi
            ;;
        run)
            if (( COMP_CWORD == 2 )); then
                dynamic=1
                candidates="abort cancel $(command goobers __complete workflows 2>/dev/null)"
            elif [[ "${COMP_WORDS[2]:-}" == "abort" ]] && (( COMP_CWORD == 3 )); then
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
            if (( COMP_CWORD == 2 )); then
                candidates="list du"
            fi
            ;;
        workspace)
            if (( COMP_CWORD == 2 )); then
                candidates="reset"
            fi
            ;;
        blocked)
            if (( COMP_CWORD == 2 )); then
                candidates="list clear"
            fi
            ;;
        claims)
            if (( COMP_CWORD == 2 )); then
                candidates="list release"
            fi
            ;;
        trace)
            if (( COMP_CWORD == 2 )); then
                dynamic=1
                candidates="$(command goobers __complete runs 2>/dev/null)"
            fi
            ;;
        e2e)
            if (( COMP_CWORD == 2 )); then
                candidates="verify kill-inject"
            fi
            ;;
        escalations)
            if (( COMP_CWORD == 2 )); then
                candidates="show"
            elif [[ "${COMP_WORDS[2]:-}" == "show" ]] && (( COMP_CWORD == 3 )); then
                dynamic=1
                candidates="$(command goobers __complete escalations 2>/dev/null)"
            fi
            ;;
        completion)
            if (( COMP_CWORD == 2 )); then
                candidates="bash zsh fish powershell"
            fi
            ;;
        telemetry)
            if (( COMP_CWORD == 2 )); then
                candidates="stats errors export prune prune-orphans compact"
            fi
            ;;
        journal)
            if (( COMP_CWORD == 2 )); then
                candidates="redact"
            fi
            ;;
        help)
            if (( COMP_CWORD == 2 )); then
                candidates="all stages instance gaggle goober workflow stage gate harness capability"
            fi
            ;;
    esac

    if (( dynamic == 1 )) || [[ -n "${candidates}" ]]; then
        COMPREPLY=( $(compgen -W "${candidates}" -- "${cur}") )
        return
    fi

    compopt -o default
}

complete -F _goobers_completion goobers
