package modelgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/vkviyu/go-self-evolving-agent/internal/observation"
)

func TestResponsesObserverAccountsForBodyWithoutContent(t *testing.T) {
	// Simulate the incident's byte count with *known* fixture contents.
	// Include CRLF, a comment, ignored control fields and a partial data line.
	created := "event: response.created\r\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"sensitive-fixture\"}}\r\n\r\n"
	control := "retry: 1000\r\n\r\n"
	unfinished := "event: response.output_text.delta\ndata: {\"delta\":\"sensitive-fixture"
	comment := ":" + strings.Repeat("z", 32311-len(created+control+unfinished)-3) + "\r\n"
	body := created + control + comment + unfinished
	timeout := &modelDeadline{phase: "stream read", timeout: 2 * time.Minute}
	events, err := observedResponseFixture(t, io.MultiReader(iotest.HalfReader(strings.NewReader(body)), iotest.ErrReader(timeout)))
	if err == nil {
		t.Fatal("expected a stream read timeout")
	}
	summary := events["responses.stream"]
	if summary.Status != "stream_read_timeout" || summary.Labels["stage"] != "frames_without_output" || summary.Labels["last_event"] != "response.created" {
		t.Fatalf("terminal event = %+v", summary)
	}
	for key, want := range map[string]int64{
		"body_bytes": 32311, "frame_count": 1, "frame_bytes": int64(len(created)), "comment_lines": 1,
		"comment_bytes": int64(len(comment)), "control_bytes": int64(len(control)),
		"unfinished_bytes": int64(len(unfinished)), "invalid_frame_bytes": 0, "unclassified_bytes": 0, "text_bytes": 0,
	} {
		if got, ok := summary.Metrics[key]; !ok || got != want {
			t.Errorf("%s = %v, want %d", key, got, want)
		}
	}
	for _, name := range []string{"responses.headers", "responses.first_byte", "responses.first_frame", "responses.first_comment"} {
		if _, ok := events[name]; !ok {
			t.Errorf("missing live boundary %s", name)
		}
	}
	if _, ok := events["responses.first_output"]; ok {
		t.Fatal("reported model output without decoded content")
	}
}

func TestResponsesObserverCountsDecodedContentAndReadAhead(t *testing.T) {
	first := responseFrames(`{"type":"response.reasoning_summary_text.delta","delta":"sensitive-fixture"}`)
	text := responseFrames(`{"type":"response.output_text.delta","delta":"你好"}`)
	unknown := responseFrames(`{"type":"sensitive-fixture.vendor"}`)
	done := responseFrames(`{"type":"response.completed","response":{"status":"completed"}}`)
	extra := ": read ahead\n"
	events, err := observedResponseFixture(t, strings.NewReader(first+text+unknown+done+extra))
	if err != nil {
		t.Fatal(err)
	}
	summary := events["responses.stream"]
	if summary.Status != "completed" || summary.Labels["stage"] != "decoded_output" || summary.Labels["last_event"] != "response.completed" ||
		summary.Metrics["frame_count"] != 4 || summary.Metrics["text_bytes"] != int64(len("你好")) ||
		summary.Metrics["reasoning_bytes"] != int64(len("sensitive-fixture")) ||
		summary.Metrics["unclassified_bytes"] != int64(len(extra)) ||
		summary.Metrics["frame_bytes"] != int64(len(first+text+unknown+done)) {
		t.Fatalf("terminal summary = %+v", summary)
	}
	if events["responses.first_output"].Labels["kind"] != "reasoning" {
		t.Fatalf("first decoded output = %+v", events["responses.first_output"])
	}
	if f := events["responses.frames.other"]; f.Metrics["count"] != 1 || f.Metrics["bytes"] != int64(len(unknown)) {
		t.Fatalf("unknown frame accounting = %+v", f)
	}
}

func observedResponseFixture(t *testing.T, body io.Reader) (map[string]observation.Event, error) {
	t.Helper()
	rec, err := observation.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		headers := http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"sensitive-fixture-request"}}
		return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(body), Request: r}, nil
	})
	m := timingTestModel(t, modelTimingWires["Responses"], "http://fixture.invalid", 5*time.Second, &http.Client{Transport: transport})
	_, resultErr := drainModelStream(rec.Attach(ctx), m)
	if err := rec.Close(ctx); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(rec.Path())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("sensitive-fixture")) {
		t.Fatal("observer emitted raw event data or provider-controlled identifier")
	}
	events := make(map[string]observation.Event)
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		var e observation.Event
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatal(err)
		}
		if e.Component == "modelgateway" {
			key := e.Name
			if key == "responses.frames" {
				key += "." + e.Labels["event"]
			}
			if _, ok := events[key]; ok {
				t.Fatalf("repeated one-shot observation %q", key)
			}
			events[key] = e
		}
	}
	return events, resultErr
}

func TestResponsesObserverSamplesOnlyActiveReads(t *testing.T) {
	recorder, err := observation.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	o := new(responsesObservation)
	o.enable(recorder.Attach(t.Context()))
	start := time.Now()
	for second := 0; second <= 31; second++ {
		o.BodyBytes = uint64(100 * second)
		o.activity(start.Add(time.Duration(second) * time.Second))
	}
	if err := recorder.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(recorder.Path())
	if err != nil {
		t.Fatal(err)
	}
	var samples []int64
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte{'\n'}) {
		var event observation.Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event.Name == "responses.activity" {
			samples = append(samples, event.Metrics["body_bytes"])
		}
	}
	if len(samples) != 2 || samples[0] != 1500 || samples[1] != 3000 {
		t.Fatalf("periodic body byte samples = %v", samples)
	}
}
