// Package replay implements the append-only terminal event log.
//
// Every frame that passes through a session is durably recorded here.
// The log is append-only by design: no record is ever modified or deleted.
// This gives us a complete, replayable history of every session.
//
// Storage layout:
//   - Segment files: fixed-size chunks of LZ4-compressed TerminalEvents.
//   - Index files:   sparse binary search index mapping timestamps to segment
//     offsets, enabling O(log n) seeks without scanning the full log.
//
// Delta encoding stores the diff between consecutive frames rather than full
// screen state, reducing storage by ~10x for typical terminal sessions.
package replay
