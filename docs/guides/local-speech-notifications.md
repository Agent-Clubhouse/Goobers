# Local speech notifications

Goobers can speak failed and escalated run alerts through a local operating
system engine. Speech is disabled by default, uses no cloud service or
credentials, and does not replace the separate desktop-notification setting.

Configure `speech` in `instance.yaml`:

```yaml
speech:
  enabled: false
  engine: auto
  language: en-US
  rate: 180
  timeout: 15s
```

Run preflight and the fixed test phrase before changing `enabled` to `true`:

```console
goobers speech preflight /path/to/instance
goobers speech test /path/to/instance
```

Both commands also support `--json`. Preflight does not emit sound. The test
command speaks only `Goobers speech notifications are ready.` and does not
accept arbitrary text.

## Engines and options

`engine: auto` selects `say` on macOS and eSpeak on Linux. An explicit engine
must be `say` or `espeak`; arbitrary commands and executable paths are not
accepted.

- **macOS:** `say` is included with macOS. Preflight reads `say -v ?` to verify
  configured voices and languages, requires an active GUI login session, and
  checks that System Profiler identifies a default audio output.
- **Linux:** install eSpeak NG so `espeak-ng` is on `PATH`; classic `espeak` is
  also supported as a fallback. Preflight reads `--voices` and requires an
  openable ALSA playback device or a reachable PulseAudio/PipeWire Unix session.
  Merely configured or stale endpoint paths do not pass. A headless host without
  an audio session fails with the missing prerequisite instead of reporting
  success.

`voice` is an installed engine voice. `language` is a BCP 47-style tag such as
`en-US`; when no voice is configured, preflight selects an installed voice for
that language. If both are set, the voice must match the language. `rate` is
bounded to 80-450 words per minute, and `timeout` is bounded to 1 second through
2 minutes.

## Delivery behavior and receipts

Alert text is passed through the synthesizer process's standard input and is
never interpolated into a shell command. The `say` adapter escapes its embedded
command delimiters. The eSpeak adapter preserves the original text across
sequential invocations while splitting adjacent brackets so no invocation can
interpret `[[...]]` as phoneme markup. The sink does not summarize or
paraphrase the text.

One speech sink processes accepted alerts in FIFO admission order. Speech
never overlaps and a newer alert never interrupts one already speaking. The
queue holds at most 32 waiting alerts; overflow, cancellation, and timeout are
recorded as failures.

Each attempt appends `started` and `delivered` or `failed` receipts to
`scheduler/speech-receipts.jsonl`. Receipts contain the notification ID,
engine, timestamps, duration, status, and a bounded single-line error. They do
not contain the spoken text. The receipt log is capped at 1 MiB by default; the
existing contents are discarded before the next receipt would exceed that
limit.
