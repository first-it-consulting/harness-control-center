package egress

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

func tapAll(contentType string, chunks ...string) (int64, int64) {
	var recIn, recOut int64
	t := newUsageTap(contentType, func(in, out int64) { recIn += in; recOut += out })
	for _, c := range chunks {
		_, _ = t.Write([]byte(c))
	}
	in, out, _, _ := t.finish()
	if in != recIn || out != recOut {
		panic(fmt.Sprintf("recorded %d/%d but counted %d/%d", recIn, recOut, in, out))
	}
	return in, out
}

func TestUsageJSON(t *testing.T) {
	anthropic := `{"id":"msg_1","type":"message","usage":{"input_tokens":16,"cache_read_input_tokens":4,"output_tokens":40}}`
	if in, out := tapAll("application/json", anthropic[:20], anthropic[20:]); in != 20 || out != 40 {
		t.Errorf("anthropic = %d/%d", in, out)
	}
	openai := `{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`
	if in, out := tapAll("application/json; charset=utf-8", openai); in != 11 || out != 7 {
		t.Errorf("openai = %d/%d", in, out)
	}
	if in, out := tapAll("application/json", `not json`); in != 0 || out != 0 {
		t.Errorf("garbage = %d/%d", in, out)
	}
}

func TestUsageAnthropicStream(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":120,"cache_creation_input_tokens":30,"output_tokens":1}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"text":"hi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":57}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	// Split at awkward places, like the network would.
	var chunks []string
	for i := 0; i < len(stream); i += 7 {
		chunks = append(chunks, stream[i:min(i+7, len(stream))])
	}
	if in, out := tapAll("text/event-stream", chunks...); in != 150 || out != 57 {
		t.Errorf("stream = %d/%d, want 150/57", in, out)
	}
}

func TestUsageOpenAIStreamWithoutTrailingNewline(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{}}]}\n\ndata: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3}}"
	if in, out := tapAll("text/event-stream", stream); in != 9 || out != 3 {
		t.Errorf("stream = %d/%d", in, out)
	}
}

func TestUsageBounded(t *testing.T) {
	huge := strings.Repeat("x", maxUsageJSON+1)
	tap := newUsageTap("application/json", nil)
	_, _ = tap.Write([]byte(huge))
	if tap.buf.Len() != 0 {
		t.Error("oversized JSON kept in memory")
	}
	sse := newUsageTap("text/event-stream", nil)
	_, _ = sse.Write([]byte("data: " + strings.Repeat("y", 2*maxSSELine)))
	if sse.buf.Len() > maxSSELine {
		t.Errorf("SSE line buffer grew to %d", sse.buf.Len())
	}
}

func TestMeteredBodyEndsOnce(t *testing.T) {
	var calls int
	var gotIn, gotOut int64
	var final bool
	b := &meteredBody{
		ReadCloser: io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":5,"output_tokens":6}}`)),
		tap:        newUsageTap("application/json", func(in, out int64) { gotIn += in; gotOut += out }),
		onEnd:      func(_, _ int64, _, f bool) { calls++; final = f },
	}
	if _, err := io.ReadAll(b); err != nil {
		t.Fatal(err)
	}
	_ = b.Close()
	if calls != 1 || !final || gotIn != 5 || gotOut != 6 {
		t.Errorf("calls %d final %v usage %d/%d", calls, final, gotIn, gotOut)
	}
}

// Streamed usage is recorded as it arrives, so the budget can stop a long
// stream before it ends.
func TestStreamRecordedIncrementally(t *testing.T) {
	var events [][2]int64
	tap := newUsageTap("text/event-stream", func(in, out int64) { events = append(events, [2]int64{in, out}) })
	_, _ = tap.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"output_tokens\":1}}}\n\n"))
	if len(events) != 1 || events[0] != [2]int64{100, 1} {
		t.Fatalf("after message_start: %v", events)
	}
	_, _ = tap.Write([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":40}}\n\n"))
	if len(events) != 2 || events[1] != [2]int64{0, 39} {
		t.Errorf("after message_delta: %v (want the increment)", events)
	}
}
