import { useEffect, useMemo, useState } from "react";
import type { RunEvent } from "../api/types";
import {
  orderedReplayEvents,
  replaySpeeds,
  replayTransition,
  type ReplaySpeed,
} from "../replay";
import { Icon } from "../ui/Icon";

// ReplayScrubber deterministically plays a run's ordered event stream, driving
// the parent's selected sequence so the graph node-state overlay animates
// (DASH-22, on top of DASH-19). It scrubs, steps, and varies speed, and
// compresses long idle gaps.
export function ReplayScrubber({
  events,
  selectedSeq,
  onSeek,
  terminal,
}: {
  events: RunEvent[];
  selectedSeq: number;
  onSeek: (seq: number) => void;
  terminal: boolean;
}) {
  const [playing, setPlaying] = useState(false);
  const [speed, setSpeed] = useState<ReplaySpeed>(1);

  // Memoize on events so the play effect's dependency is stable between renders
  // and only changes when new live events actually arrive.
  const ordered = useMemo(() => orderedReplayEvents(events), [events]);
  const index = ordered.findIndex((event) => event.seq === selectedSeq);
  const atEnd = index >= ordered.length - 1;

  useEffect(() => {
    if (!playing || ordered.length === 0) {
      return;
    }
    if (index < 0) {
      onSeek(ordered[0].seq);
      return;
    }
    if (index >= ordered.length - 1) {
      // Reached the current end. A finished run has nothing more, so stop
      // cleanly. A live run keeps playing: this effect re-runs when `ordered`
      // grows (new events), resuming instead of stalling indefinitely — the
      // DASH-22 regression.
      if (terminal) {
        setPlaying(false);
      }
      return;
    }
    const transition = replayTransition(ordered, index, speed);
    if (!transition) {
      return;
    }
    const timer = window.setTimeout(() => onSeek(ordered[index + 1].seq), transition.playbackDelayMs);
    return () => window.clearTimeout(timer);
  }, [playing, index, ordered, speed, terminal, onSeek]);

  const togglePlay = () => {
    if (!playing && atEnd && terminal && ordered.length > 0) {
      // Pressing play at a FINISHED run's end replays from the top. A live run
      // at its current end instead keeps playing and waits for more events.
      onSeek(ordered[0].seq);
    }
    setPlaying((value) => !value);
  };

  const step = (delta: number) => {
    setPlaying(false);
    const from = index < 0 ? (delta > 0 ? -1 : ordered.length) : index;
    const target = ordered[from + delta];
    if (target) {
      onSeek(target.seq);
    }
  };

  const position = index < 0 ? 0 : index;
  const total = ordered.length;

  if (total === 0) {
    return null;
  }

  return (
    <div aria-label="Replay controls" className="playback-panel" role="group">
      <div className="playback-controls">
        <button
          aria-label="Previous event"
          className="replay-mode-control"
          disabled={position <= 0}
          onClick={() => step(-1)}
          type="button"
        >
          <Icon name="previous" size={16} />
        </button>
        <button
          aria-label={playing ? "Pause replay" : "Play replay"}
          aria-pressed={playing}
          className="replay-mode-control"
          onClick={togglePlay}
          type="button"
        >
          <Icon name={playing ? "pause" : "play"} size={16} />
        </button>
        <button
          aria-label="Next event"
          className="replay-mode-control"
          disabled={atEnd}
          onClick={() => step(1)}
          type="button"
        >
          <Icon name="next" size={16} />
        </button>
        <div aria-label="Playback speed" className="replay-mode" role="group">
          {replaySpeeds.map((option) => (
            <button
              aria-pressed={speed === option}
              className={
                speed === option ? "replay-mode-control replay-mode-live-follow" : "replay-mode-control"
              }
              key={option}
              onClick={() => setSpeed(option)}
              type="button"
            >
              {option}×
            </button>
          ))}
        </div>
      </div>
      <label className="playback-context">
        <span className="sr-only">Scrub to event</span>
        <input
          aria-label="Scrub to event"
          max={total - 1}
          min={0}
          onChange={(event) => {
            setPlaying(false);
            const target = ordered[Number(event.target.value)];
            if (target) {
              onSeek(target.seq);
            }
          }}
          step={1}
          type="range"
          value={position}
        />
      </label>
      <p className="playback-summary">
        <span className="playback-now" aria-live="polite">
          Event {position + 1} of {total}
        </span>
        {atEnd && terminal ? <span className="replay-state"> · end</span> : null}
      </p>
    </div>
  );
}
