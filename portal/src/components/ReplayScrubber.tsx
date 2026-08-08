import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import type { RunEvent, WorkflowGraph } from "../api/types";
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
import { eventHeading, eventNodeId, eventSummary, nodeOwner } from "../runDetailData";
import { Icon } from "../ui/Icon";

const chapterPresentation = {
  transition: { label: "Workflow transition", glyph: "●" },
  decision: { label: "Gate decision", glyph: "◆" },
  failure: { label: "Failure", glyph: "!" },
  escalation: { label: "Escalation", glyph: "↑" },
  external: { label: "External result", glyph: "↗" },
  terminal: { label: "Terminal outcome", glyph: "■" },
} satisfies Record<ReplayChapterKind, { label: string; glyph: string }>;

const chapterMarkerWidth = 30;

interface ReplayChapterGroup {
  chapters: ReplayChapter[];
  key: string;
  left: string;
}

function groupReplayChapters(
  chapters: ReplayChapter[],
  trackWidth: number,
): ReplayChapterGroup[] {
  if (trackWidth <= 0) {
    return chapters.map((chapter) => ({
      chapters: [chapter],
      key: `${chapter.event.branch}-${chapter.event.seq}`,
      left: `${chapter.percent}%`,
    }));
  }

  const edgeInset = Math.min(chapterMarkerWidth / 2, trackWidth / 2);
  const positioned = chapters.map((chapter) => ({
    chapter,
    center: Math.min(
      Math.max((chapter.percent / 100) * trackWidth, edgeInset),
      trackWidth - edgeInset,
    ),
  }));
  const groups: Array<{
    chapters: ReplayChapter[];
    centerTotal: number;
  }> = [];

  for (const positionedChapter of positioned) {
    const current = groups.at(-1);
    const currentCenter = current
      ? current.centerTotal / current.chapters.length
      : undefined;
    if (
      current &&
      currentCenter !== undefined &&
      positionedChapter.center - currentCenter < chapterMarkerWidth
    ) {
      current.chapters.push(positionedChapter.chapter);
      current.centerTotal += positionedChapter.center;
      continue;
    }
    groups.push({
      chapters: [positionedChapter.chapter],
      centerTotal: positionedChapter.center,
    });
  }

  return groups.map((group) => {
    const first = group.chapters[0].event;
    const last = group.chapters.at(-1)?.event ?? first;
    const center = group.centerTotal / group.chapters.length;
    return {
      chapters: group.chapters,
      key: `${first.branch}-${first.seq}-${last.branch}-${last.seq}`,
      left: `${center}px`,
    };
  });
}

// ReplayScrubber plays every durable event while adding semantic chapter
// navigation over the same deterministic sequence.
export function ReplayScrubber({
  events,
  graph,
  runId,
  selectedSeq,
  onSeek,
  terminal,
  workflow,
}: {
  events: RunEvent[];
  graph?: WorkflowGraph;
  runId: string;
  selectedSeq: number;
  onSeek: (seq: number) => void;
  terminal: boolean;
  workflow?: string;
}) {
  const [playing, setPlaying] = useState(false);
  const [speed, setSpeed] = useState<ReplaySpeed>(1);
  const [trackWidth, setTrackWidth] = useState(0);
  const [expandedChapterGroup, setExpandedChapterGroup] = useState<string>();
  const timelineRef = useRef<HTMLDivElement>(null);
  const timeline = useMemo(
    () => replayTimeline(events, graph, runId),
    [events, graph, runId],
  );
  const chapterGroups = useMemo(
    () => groupReplayChapters(timeline.chapters, trackWidth),
    [timeline.chapters, trackWidth],
  );
  const expandedGroup = chapterGroups.find(
    (group) => group.key === expandedChapterGroup && group.chapters.length > 1,
  );
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

  useLayoutEffect(() => {
    const element = timelineRef.current;
    if (!element) {
      return;
    }
    const measure = () => setTrackWidth(element.clientWidth);
    measure();
    window.addEventListener("resize", measure);
    const observer =
      typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(measure);
    observer?.observe(element);
    return () => {
      observer?.disconnect();
      window.removeEventListener("resize", measure);
    };
  }, []);

  useEffect(() => {
    if (expandedChapterGroup && !expandedGroup) {
      setExpandedChapterGroup(undefined);
    }
  }, [expandedChapterGroup, expandedGroup]);

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

  const chapterDescription = (chapter: ReplayChapter) => {
    const heading = eventHeading(chapter.event);
    const summary = eventSummary(chapter.event, undefined, runId);
    const stageId = eventNodeId(chapter.event, runId);
    const stageLabel = timeline.stageSegments.find(
      (segment) => segment.stageId === stageId,
    )?.label;
    const owner = nodeOwner(graph, stageId);
    const scope = [stageLabel, owner].filter(Boolean).join(" · ");
    return {
      heading,
      summary,
      stageLabel,
      owner,
      label: `Go to ${chapterPresentation[chapter.kind].label} chapter at event ${chapter.index + 1}: ${heading}. ${summary}${
        scope ? ` ${scope}.` : ""
      }`,
    };
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
  const summary = eventSummary(currentPoint.event, undefined, runId);
  const currentStageId = eventNodeId(currentPoint.event, runId);
  const currentStageLabel = timeline.stageSegments.find(
    (segment) => segment.stageId === currentStageId,
  )?.label;
  const currentOwner = nodeOwner(graph, currentStageId);
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
          {(workflow || currentStageLabel) && (
            <span aria-label="Workflow, stage, and goober" className="playback-scope" role="group">
              {workflow && <span className="playback-workflow">{workflow}</span>}
              {workflow && currentStageLabel && <span aria-hidden="true">·</span>}
              {currentStageLabel && <span className="playback-stage">{currentStageLabel}</span>}
              {currentOwner && <span className="playback-owner">{currentOwner}</span>}
            </span>
          )}
          <strong>{heading}</strong>
          <span>{summary}</span>
        </div>
        <span className="playback-position">
          {formatReplayClock(currentPoint.realOffsetMs)} /{" "}
          {formatReplayClock(timeline.realDurationMs)}
        </span>
      </div>

      <div className="replay-timeline" ref={timelineRef}>
        <div aria-hidden="true" className="replay-track">
          <span
            className="replay-track-progress"
            style={{ width: `${currentPoint.percent}%` }}
          />
        </div>
        <div aria-label="Run stages" className="replay-stage-lane" role="group">
          {timeline.stageSegments.map((segment) => {
            const label = segment.owner
              ? `Stage ${segment.label}, owned by ${segment.owner}`
              : `Stage ${segment.label}`;
            return (
              <span
                aria-label={label}
                className={`replay-stage-segment replay-stage-segment-${segment.colorIndex % 4}`}
                key={segment.key}
                role="note"
                style={{
                  left: `${segment.startPercent}%`,
                  width: `${Math.max(segment.endPercent - segment.startPercent, 1)}%`,
                }}
                tabIndex={0}
                title={label}
              />
            );
          })}
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
        {chapterGroups.map((group) => {
          if (group.chapters.length === 1) {
            const chapter = group.chapters[0];
            const description = chapterDescription(chapter);
            return (
              <button
                aria-current={chapter.index === index ? "step" : undefined}
                aria-label={description.label}
                className={`replay-chapter replay-chapter-${chapter.kind} ${
                  chapter.index === index ? "replay-chapter-selected" : ""
                }`}
                key={group.key}
                onClick={() => seek(chapter.event)}
                style={{ left: group.left }}
                title={description.label}
                type="button"
              >
                <span aria-hidden="true" className="replay-chapter-shape">
                  {chapterPresentation[chapter.kind].glyph}
                </span>
              </button>
            );
          }

          const containsSelectedChapter = group.chapters.some(
            (chapter) => chapter.index === index,
          );
          const panelId = `replay-chapter-cluster-${group.key}`;
          const label = `Show ${group.chapters.length} clustered chapters`;
          return (
            <button
              aria-current={containsSelectedChapter ? "step" : undefined}
              aria-controls={panelId}
              aria-expanded={group.key === expandedChapterGroup}
              aria-label={label}
              className={`replay-chapter replay-chapter-cluster ${
                containsSelectedChapter ? "replay-chapter-selected" : ""
              }`}
              key={group.key}
              onClick={() =>
                setExpandedChapterGroup((current) =>
                  current === group.key ? undefined : group.key,
                )
              }
              style={{ left: group.left }}
              title={label}
              type="button"
            >
              <span aria-hidden="true" className="replay-chapter-shape">
                {group.chapters.length}
              </span>
            </button>
          );
        })}
        {expandedGroup ? (
          <div
            aria-label={`${expandedGroup.chapters.length} clustered chapters`}
            className="replay-chapter-cluster-panel"
            id={`replay-chapter-cluster-${expandedGroup.key}`}
            role="region"
          >
            <div className="replay-chapter-cluster-heading">
              <strong>{expandedGroup.chapters.length} chapters</strong>
              <span>Choose a chapter in this crowded timeline stretch.</span>
              <button
                aria-label="Close chapter cluster"
                onClick={() => setExpandedChapterGroup(undefined)}
                type="button"
              >
                ×
              </button>
            </div>
            <ul aria-label="Chapters in cluster">
              {expandedGroup.chapters.map((chapter) => {
                const description = chapterDescription(chapter);
                return (
                  <li key={`${chapter.event.branch}-${chapter.event.seq}`}>
                    <button
                      aria-current={chapter.index === index ? "step" : undefined}
                      aria-label={description.label}
                      className={`replay-chapter-cluster-option replay-chapter-${chapter.kind}`}
                      onClick={() => {
                        setExpandedChapterGroup(undefined);
                        seek(chapter.event);
                      }}
                      type="button"
                    >
                      <span aria-hidden="true" className="replay-chapter-shape">
                        {chapterPresentation[chapter.kind].glyph}
                      </span>
                      <span className="replay-chapter-cluster-copy">
                        {(description.stageLabel || description.owner) && (
                          <span className="replay-chapter-cluster-scope">
                            {[description.stageLabel, description.owner]
                              .filter(Boolean)
                              .join(" · ")}
                          </span>
                        )}
                        <strong>{description.heading}</strong>
                        <span>
                          Event {chapter.index + 1} · {description.summary}
                        </span>
                      </span>
                    </button>
                  </li>
                );
              })}
            </ul>
          </div>
        ) : null}
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
