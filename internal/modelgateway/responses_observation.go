package modelgateway

import (
	"bufio"
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/vkviyu/go-self-evolving-agent/internal/modeltelemetry"
	"github.com/vkviyu/go-self-evolving-agent/internal/observation"
)

// The existing model receipt and the optional observer use the same HTTP read
// counter and timestamps. One goroutine owns the decoder after headers arrive.
type responsesObservation struct {
	modeltelemetry.Stream
	stats *responsesFrameStats
}

type frameCount struct {
	count, bytes int64
}

type responsesFrameStats struct {
	ctx                                   context.Context
	frames                                map[string]frameCount // keys are limited by responsesEventLabel
	parsed                                frameCount
	pending, comments, control, invalid   int64
	commentLines, lineBytes               int64
	lineComplete                          bool
	firstFrame, firstOutput, nextActivity time.Time
	textBytes, reasoningBytes, toolChunks int64
}

func (o *responsesObservation) enable(ctx context.Context) {
	if observation.Enabled(ctx) {
		o.stats = &responsesFrameStats{ctx: ctx, frames: make(map[string]frameCount)}
	}
}

// emit publishes stream diagnostics with the request context, labels and counters
// collected by the Responses parser.
func (s *responsesFrameStats) emit(name, status string, labels map[string]string, metrics map[string]int64) {
	observation.Emit(s.ctx, observation.Event{
		Component: "modelgateway", Name: name, Status: status, Labels: labels, Metrics: metrics,
	})
}

func (o *responsesObservation) headers() {
	if s := o.stats; s != nil {
		s.emit("responses.headers", "received", map[string]string{"content_type": o.ContentType},
			map[string]int64{"http_status": int64(o.Status), "elapsed_ms": o.HeadersAt.Sub(o.StartedAt).Milliseconds()})
	}
}

// activity runs after a successful HTTP read but before the parser consumes
// those bytes. It emits at most one sample per interval, using no timer or
// goroutine. The last sample is a lower bound while the next read blocks.
const responsesActivityInterval = 15 * time.Second

func (o *responsesObservation) activity(now time.Time) {
	s := o.stats
	if s.nextActivity.IsZero() {
		s.nextActivity = now.Add(responsesActivityInterval)
		return
	}
	if now.Before(s.nextActivity) {
		return
	}
	s.nextActivity = now.Add(responsesActivityInterval)
	s.emit("responses.activity", "receiving", map[string]string{"last_event": o.LastEvent}, map[string]int64{
		"body_bytes": int64(o.BodyBytes), "frame_bytes": s.parsed.bytes, "frame_count": s.parsed.count,
		"comment_bytes": s.comments, "comment_lines": s.commentLines, "text_bytes": s.textBytes,
		"reasoning_bytes": s.reasoningBytes, "tool_chunks": s.toolChunks,
	})
}

// Use the parser's exact LF/CRLF/EOF behavior. Count the consumed bytes, not
// the decoded string length (ScanLines strips CR/LF and data may be UTF-8).
func (s *responsesFrameStats) scanLines(data []byte, atEOF bool) (int, []byte, error) {
	advance, token, err := bufio.ScanLines(data, atEOF)
	if token != nil {
		s.lineBytes = int64(advance)
		s.lineComplete = advance > 0 && data[advance-1] == '\n'
	}
	return advance, token, err
}

// line accounts for SSE comments, control lines and pending data before a frame has been
// decoded.
func (o *responsesObservation) line(line string, readFailed bool) {
	s := o.stats
	switch {
	case !s.lineComplete && readFailed:
		// ScanLines returns the final fragment even after a transport error.
		s.pending += s.lineBytes
	case strings.HasPrefix(line, ":"):
		s.comments += s.lineBytes
		s.commentLines++
		if s.commentLines == 1 {
			s.emit("responses.first_comment", "received", nil, nil)
		}
	case strings.HasPrefix(line, "event:"), strings.HasPrefix(line, "data:"), line == "" && s.pending != 0:
		s.pending += s.lineBytes
	default:
		s.control += s.lineBytes
	}
}

func (o *responsesObservation) emptyFrame() {
	if s := o.stats; s != nil {
		s.control += s.pending
		s.pending = 0
	}
}

func (o *responsesObservation) invalidFrame() {
	if s := o.stats; s != nil {
		s.invalid += s.pending
		s.pending = 0
	}
}

// frame attributes accumulated data bytes to the parsed event type and reports the first
// frame.
func (o *responsesObservation) frame(label string) {
	if s := o.stats; s != nil {
		s.parsed.count++
		s.parsed.bytes += s.pending
		f := s.frames[label]
		f.count++
		f.bytes += s.pending
		s.frames[label] = f
		s.pending = 0
		if s.firstFrame.IsZero() {
			s.firstFrame = time.Now()
			s.emit("responses.first_frame", "parsed", map[string]string{"event": label}, nil)
		}
	}
}

type responsesObservingSender struct {
	responsesMessageSender
	observation *responsesObservation
}

// Send measures outgoing message content before handing it to a concurrent consumer and
// reports a closed reader.
func (w responsesObservingSender) Send(msg *schema.Message, err error) bool {
	// Capture sizes before handing the message to a concurrent consumer.
	s := w.observation.stats
	kind := ""
	switch {
	case msg.Content != "":
		s.textBytes += int64(len(msg.Content))
		kind = "text"
	case msg.ReasoningContent != "":
		s.reasoningBytes += int64(len(msg.ReasoningContent))
		kind = "reasoning"
	case len(msg.ToolCalls) > 0:
		s.toolChunks++
		kind = "tool"
	}
	if kind != "" && s.firstOutput.IsZero() {
		s.firstOutput = time.Now()
		s.emit("responses.first_output", "decoded", map[string]string{"kind": kind}, nil)
	}
	return w.responsesMessageSender.Send(msg, err)
}

// BodyBytes counts bytes returned by HTTP Body.Read, after any HTTP-level
// decoding. frame_bytes includes event/data lines and their blank delimiter;
// comments (including their line endings) and other control lines are separate.
// unfinished_bytes counts scanned event data without a dispatch boundary.
// unclassified_bytes includes scanner read-ahead after terminal or invalid
// frames and unread scanner tokens over its size limit, never guessed as text.
// The per-type frame totals partition frame_bytes. Decoded text_bytes and
// reasoning_bytes are UTF-8 payload sizes, a separate measurement.
func (o *responsesObservation) finish() {
	s := o.stats
	if s == nil {
		return
	}
	frames := s.parsed
	body := int64(o.BodyBytes)
	var httpErrorBody int64
	if o.Status != 0 && (o.Status < 200 || o.Status >= 300) {
		httpErrorBody = body
	}
	metrics := map[string]int64{
		"http_status": int64(o.Status), "body_bytes": body, "frame_count": frames.count, "frame_bytes": frames.bytes,
		"comment_lines": s.commentLines, "comment_bytes": s.comments, "control_bytes": s.control,
		"invalid_frame_bytes": s.invalid, "unfinished_bytes": s.pending, "http_error_body_bytes": httpErrorBody,
		"unclassified_bytes": body - (frames.bytes + s.comments + s.control + s.invalid + s.pending + httpErrorBody),
		"text_bytes":         s.textBytes, "reasoning_bytes": s.reasoningBytes, "tool_chunks": s.toolChunks,
	}
	for key, t := range map[string]time.Time{
		"headers_ms": o.HeadersAt, "first_byte_ms": o.FirstByteAt, "last_byte_ms": o.LastByteAt,
		"first_frame_ms": s.firstFrame, "first_output_ms": s.firstOutput, "duration_ms": o.EndedAt,
	} {
		if !t.IsZero() {
			metrics[key] = t.Sub(o.StartedAt).Milliseconds()
		}
	}
	// Record the furthest boundary we can prove locally, including when
	// the stream terminates before it produces any model output.
	stage := "awaiting_headers"
	switch {
	case o.Status != 0 && (o.Status < 200 || o.Status >= 300):
		stage = "http_error"
	case !s.firstOutput.IsZero():
		stage = "decoded_output"
	case frames.count > 0:
		stage = "frames_without_output"
	case body > 0:
		stage = "bytes_without_frame"
	case !o.HeadersAt.IsZero():
		stage = "headers_without_bytes"
	}
	s.emit("responses.stream", o.Outcome, map[string]string{"stage": stage, "last_event": o.LastEvent, "content_type": o.ContentType}, metrics)
	for _, label := range slices.Sorted(maps.Keys(s.frames)) {
		f := s.frames[label]
		s.emit("responses.frames", "parsed", map[string]string{"event": label}, map[string]int64{"count": f.count, "bytes": f.bytes})
	}
}
