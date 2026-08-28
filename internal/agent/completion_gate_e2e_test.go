package agent

import (
	"context"
	"errors"
	"testing"

	"reasonix/internal/agent/testutil"
	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

func TestCompletionGateTerminalMatrix(t *testing.T) {
	for _, reason := range []string{"", "stop", "tool_calls", "function_call", "refusal", " STOP "} {
		t.Run("admit_"+reason, func(t *testing.T) {
			if err := admitCompletion(reason); err != nil {
				t.Fatalf("admitCompletion(%q) = %v, want admitted", reason, err)
			}
		})
	}
	for _, reason := range []string{
		"length", "max_tokens", "max_output_tokens", "incomplete",
		"content_filter", "repetition_truncation",
	} {
		t.Run("reject_"+reason, func(t *testing.T) {
			err := admitCompletion(reason)
			var rejected *CompletionRejectedError
			if !errors.As(err, &rejected) {
				t.Fatalf("admitCompletion(%q) = %v, want CompletionRejectedError", reason, err)
			}
			if rejected.FinishReason != reason {
				t.Fatalf("finish reason = %q, want %q", rejected.FinishReason, reason)
			}
		})
	}
}

func TestRunRejectsObservedLengthLimitedToolEnvelope(t *testing.T) {
	const partial = `I will write the implementation now.
<tool_call>
<function=write_file>
<parameter=path>internal/agent/completion_gate.go</parameter>
<parameter=content>
#!/usr/bin/env python3
func admitCompletion(`
	mp := testutil.NewMock("observed-omlx", testutil.Turn{Chunks: []provider.Chunk{
		{Type: provider.ChunkText, Text: partial},
		{Type: provider.ChunkUsage, Usage: &provider.Usage{
			CompletionTokens: 4096,
			FinishReason:     "length",
		}},
		{Type: provider.ChunkDone},
	}})
	sess := NewSession("")
	sink := &recordSink{}
	a := New(mp, tool.NewRegistry(), sess, Options{MaxOutputTokens: 4096}, sink)

	err := a.Run(withNoClosedLoop(context.Background()), "continue implementing")
	var rejected *CompletionRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("Run error = %v, want CompletionRejectedError", err)
	}
	if !errors.Is(err, ErrCompletionRejected) {
		t.Fatalf("Run error = %v, want ErrCompletionRejected", err)
	}
	if rejected.FinishReason != "length" {
		t.Fatalf("finish reason = %q, want length", rejected.FinishReason)
	}
	if mp.CallCount() != 1 {
		t.Fatalf("provider calls = %d, want 1 (no completion retry)", mp.CallCount())
	}
	localOnly := 0
	for _, msg := range sess.Messages {
		if msg.Role == provider.RoleAssistant && !msg.LocalOnly {
			t.Fatalf("length-limited assistant was committed: %+v", msg)
		}
		if msg.LocalOnly && msg.InterruptedTurn != nil && msg.InterruptedTurn.Pending {
			localOnly++
		}
	}
	if localOnly != 1 {
		t.Fatalf("pending LocalOnly recovery records = %d, want 1", localOnly)
	}
	attempts := sink.kinds(event.StreamAttempt)
	if len(attempts) != 2 || attempts[0].StreamAttempt.Action != event.StreamAttemptBegin || attempts[1].StreamAttempt.Action != event.StreamAttemptDiscard {
		t.Fatalf("stream attempt lifecycle = %+v, want begin then discard", attempts)
	}
	if attempts[1].StreamAttempt.Reason != event.StreamAttemptReasonCompletionRejected {
		t.Fatalf("discard reason = %q, want %q", attempts[1].StreamAttempt.Reason, event.StreamAttemptReasonCompletionRejected)
	}
}

func TestRunNeverExecutesLengthLimitedStructuredToolCall(t *testing.T) {
	writer := &countingWriterTool{}
	reg := tool.NewRegistry()
	reg.Add(writer)
	mp := testutil.NewMock("length-tool-call", testutil.Turn{Chunks: []provider.Chunk{
		{Type: provider.ChunkToolCall, ToolCall: &provider.ToolCall{
			ID: "write-1", Name: "write_file",
			Arguments: `{"path":"x.txt","content":"must not execute"}`,
		}},
		{Type: provider.ChunkUsage, Usage: &provider.Usage{
			CompletionTokens: 4096,
			FinishReason:     "length",
		}},
		{Type: provider.ChunkDone},
	}})
	sess := NewSession("")
	sink := &recordSink{}
	a := New(mp, reg, sess, Options{MaxOutputTokens: 4096}, sink)

	err := a.Run(withNoClosedLoop(context.Background()), "write x.txt")
	var rejected *CompletionRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("Run error = %v, want CompletionRejectedError", err)
	}
	if mp.CallCount() != 1 {
		t.Fatalf("provider calls = %d, want 1 (no completion retry)", mp.CallCount())
	}
	if writer.calls.Load() != 0 {
		t.Fatalf("writer executed %d times, want 0", writer.calls.Load())
	}
	for _, msg := range sess.Messages {
		if msg.Role == provider.RoleAssistant && !msg.LocalOnly {
			t.Fatalf("length-limited assistant was committed: %+v", msg)
		}
		if msg.Role == provider.RoleTool && !msg.LocalOnly {
			t.Fatalf("length-limited tool result was committed: %+v", msg)
		}
	}
	usageEvents := sink.kinds(event.Usage)
	if len(usageEvents) != 1 || usageEvents[0].Usage == nil || usageEvents[0].Usage.FinishReason != "length" {
		t.Fatalf("usage events = %+v, want one record preserving finish_reason=length", usageEvents)
	}
}
