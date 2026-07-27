import { useEffect, useMemo, useState } from "react";
import type { RunEvent } from "../api/types";
import {
  formatReplayClock,
  formatReplayDuration,
  replaySpeeds,
  replayTimeline,
  replayTransition,
  type ReplayChapter,
  type ReplayChapterKind,
  type ReplaySpeed,
} from "../replay";
import { eventHeading, eventSummary } from "../runDetailData";
import { Icon } from "../ui/Icon";

const chapterPresentation = {
  transition: { label: "Workflow transition", glyph: "●" },
  decision: { label: "Gate decision", glyph: "◆" },
  failure: { label: "Failure", glyph: "!" },
  escalation: { label: "Escalation", glyph: "↑" },
  external: { label: "External result", glyph: "↗" },
  terminal: { label: "Terminal outcome", glyph: "■" },
} satisfies Record<ReplayChapterKind, { label: string; glyph: string }>;

// ReplayScrubber plays every durable event while adding semantic chapter
// navigation over the same deterministic sequence.
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
  const timeline = useMemo(() => replayTimeline(events), [events]);
  const ordered = timeline.events;
  const index = ordered.findIndex((event) => event.seq === selectedSeq);
  const position = index < 0 ? 0 : index;
  const currentPoint = timeline.points[position];
  const atEnd = index >= ordered.length - 1;
  const selectedChapter = timeline.chapters.find((chapter) => chapter.index === index);
  const previousChapter = [...timeline.chapters]
    .reverse()
    .find((chapter) => chapter.index < index);
  const nextChapter = timeline.chapters.find((chapter) => chapter.index > index);

  useEffect(() => {
    if (!playing || ordered.length === 0) {
      return;
    }
    if (index < 0) {
      onSeek(ordered[0].seq);
      return;
    }
    if (index >= ordered.length - 1) {
      // A live run remains armed at its current end and advances when the
      // event stream grows; a terminal run stops until explicitly replayed.
      if (terminal) {
        setPlaying(false);
      }
      return;
    }
    const transition = replayTransition(ordered, index, speed);
    if (!transition) {
      return;
    }
    const timer = window.setTimeout(
      () => onSeek(ordered[index + 1].seq),
      transition.playbackDelayMs,
    );
    return () => window.clearTimeout(timer);
  }, [playing, index, ordered, speed, terminal, onSeek]);

  if (ordered.length === 0 || !currentPoint) {
    return null;
  }

  const seek = (event: RunEvent) => {
    setPlaying(false);
    onSeek(event.seq);
  };

  const togglePlay = () => {
    if (!playing && atEnd && terminal) {
      onSeek(ordered[0].seq);
    }
    setPlaying((value) => !value);
  };

  const stepEvent = (delta: number) => {
    const from = index < 0 ? (delta > 0 ? -1 : ordered.length) : index;
    const target = ordered[from + delta];
    if (target) {
      seek(target);
    }
  };

  const seekChapter = (chapter: ReplayChapter | undefined) => {
    if (chapter) {
      seek(chapter.event);
    }
  };

  const seekTimelineOffset = (offsetMs: number) => {
    let nearest = timeline.points[0];
    for (const point of timeline.points.slice(1)) {
      if (
        Math.abs(point.compressedOffsetMs - offsetMs) <
        Math.abs(nearest.compressedOffsetMs - offsetMs)
      ) {
        nearest = point;
      }
    }
    seek(nearest.event);
  };

  const heading = eventHeading(currentPoint.event);
  const summary = eventSummary(currentPoint.event);
  const currentChapterPosition = selectedChapter
    ? timeline.chapters.indexOf(selectedChapter) + 1
    : undefined;
  const totalIdleTime = timeline.idleGaps.reduce(
    (total, gap) => total + gap.realDelayMs,
    0,
  );
  const compressedIdleTime = timeline.idleGaps.reduce(
    (total, gap) => total + gap.compressedDelayMs,
    0,
  );

  return (
    <section aria-label="Replay controls" className="playback-panel">
      <div aria-live="polite" className="playback-summary">
        <span
          aria-hidden="true"
          className={`event-mark event-mark-${selectedChapter?.kind ?? "raw"}`}
        >
          {selectedChapter ? chapterPresentation[selectedChapter.kind].glyph : "·"}
        </span>
        <div className="playback-now">
          <span>
            {currentChapterPosition
              ? `Chapter ${currentChapterPosition} of ${timeline.chapters.length}`
              : `Raw event ${position + 1} of ${ordered.length}`}
            {" · "}Sequence {currentPoint.event.seq}
          </span>
          <strong>{heading}</strong>
          <span>{summary}</span>
        </div>
        <span className="playback-position">
          {formatReplayClock(currentPoint.realOffsetMs)} /{" "}
          {formatReplayClock(timeline.realDurationMs)}
        </span>
      </div>

      <div className="replay-timeline">
        <div aria-hidden="true" className="replay-track">
          <span
            className="replay-track-progress"
            style={{ width: `${currentPoint.percent}%` }}
          />
        </div>
        {timeline.idleGaps.map((gap) => {
          const label = `Compressed idle gap between sequences ${gap.fromSeq} and ${gap.toSeq}: ${formatReplayDuration(gap.realDelayMs)} shown as ${formatReplayDuration(gap.compressedDelayMs)}.`;
          return (
            <span
              aria-label={label}
              className="replay-idle-gap"
              key={`${gap.fromSeq}-${gap.toSeq}`}
              role="note"
              style={{
                left: `${gap.startPercent}%`,
                width: `${gap.endPercent - gap.startPercent}%`,
              }}
              tabIndex={0}
              title={label}
            />
          );
        })}
        <input
          aria-label="Scrub replay timeline"
          aria-valuetext={`Event ${position + 1} of ${ordered.length}, ${formatReplayClock(currentPoint.realOffsetMs)} elapsed`}
          max={Math.max(timeline.compressedDurationMs, 1)}
          min={0}
          onChange={(event) => seekTimelineOffset(Number(event.target.value))}
          onKeyDown={(event) => {
            let delta: number | undefined;
            if (event.key === "ArrowLeft" || event.key === "ArrowDown") {
              delta = -1;
            } else if (event.key === "ArrowRight" || event.key === "ArrowUp") {
              delta = 1;
            } else if (event.key === "Home") {
              event.preventDefault();
              seek(ordered[0]);
            } else if (event.key === "End") {
              event.preventDefault();
              seek(ordered[ordered.length - 1]);
            }
            if (delta !== undefined) {
              event.preventDefault();
              stepEvent(delta);
            }
          }}
          step={1}
          type="range"
          value={currentPoint.compressedOffsetMs}
        />
        {timeline.chapters.map((chapter) => {
          const chapterHeading = eventHeading(chapter.event);
          const chapterSummary = eventSummary(chapter.event);
          const label = `Go to ${chapterPresentation[chapter.kind].label} chapter at event ${chapter.index + 1}: ${chapterHeading}. ${chapterSummary}`;
          return (
            <button
              aria-current={chapter.index === index ? "step" : undefined}
              aria-label={label}
              className={`replay-chapter replay-chapter-${chapter.kind} ${
                chapter.index === index ? "replay-chapter-selected" : ""
              }`}
              key={`${chapter.event.branch}-${chapter.event.seq}`}
              onClick={() => seek(chapter.event)}
              style={{ left: `${chapter.percent}%` }}
              title={label}
              type="button"
            >
              <span aria-hidden="true" className="replay-chapter-shape">
                {chapterPresentation[chapter.kind].glyph}
              </span>
            </button>
          );
        })}
        <span
          aria-hidden="true"
          className="replay-playhead"
          style={{ left: `${currentPoint.percent}%` }}
        />
      </div>

      <div className="playback-controls">
        <div aria-label="Replay transport" className="playback-transport" role="group">
          <button
            aria-label="Previous chapter"
            className="step-button chapter-step-button"
            disabled={!previousChapter}
            onClick={() => seekChapter(previousChapter)}
            title="Previous major chapter"
            type="button"
          >
            <Icon name="previous" size={15} />
          </button>
          <button
            aria-label="Previous raw event"
            className="step-button"
            disabled={position <= 0}
            onClick={() => stepEvent(-1)}
            title="Previous durable event"
            type="button"
          >
            <span aria-hidden="true">←</span>
          </button>
          <button
            aria-label={playing ? "Pause replay" : "Play replay"}
            aria-pressed={playing}
            className="play-button"
            onClick={togglePlay}
            type="button"
          >
            <Icon name={playing ? "pause" : "play"} size={16} />
          </button>
          <button
            aria-label="Next raw event"
            className="step-button"
            disabled={atEnd}
            onClick={() => stepEvent(1)}
            title="Next durable event"
            type="button"
          >
            <span aria-hidden="true">→</span>
          </button>
          <button
            aria-label="Next chapter"
            className="step-button chapter-step-button"
            disabled={!nextChapter}
            onClick={() => seekChapter(nextChapter)}
            title="Next major chapter"
            type="button"
          >
            <Icon name="next" size={15} />
          </button>
        </div>
        <div className="playback-options">
          <details className="chapter-legend">
            <summary>Chapter key</summary>
            <ul aria-label="Chapter marker key" className="chapter-legend-list">
              {Object.entries(chapterPresentation).map(([kind, presentation]) => (
                <li className={`replay-chapter-${kind}`} key={kind}>
                  <span aria-hidden="true" className="replay-chapter-shape">
                    {presentation.glyph}
                  </span>
                  <span>{presentation.label}</span>
                </li>
              ))}
            </ul>
          </details>
          <div aria-label="Playback speed" className="speed-control" role="group">
            {replaySpeeds.map((option) => (
              <button
                aria-label={`Set playback speed to ${option}×`}
                aria-pressed={speed === option}
                className={
                  speed === option
                    ? "speed-button speed-button-active"
                    : "speed-button"
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
      </div>

      <div className="idle-compression-status">
        <Icon name="clock" size={14} />
        <strong>Idle compression on</strong>
        <span>
          {timeline.idleGaps.length === 0
            ? "No long idle gaps in this run."
            : `${timeline.idleGaps.length} long ${
                timeline.idleGaps.length === 1 ? "gap" : "gaps"
              } · ${formatReplayDuration(totalIdleTime)} shown as ${formatReplayDuration(compressedIdleTime)}. Focus a hatched band to inspect it.`}
        </span>
      </div>
    </section>
  );
}
