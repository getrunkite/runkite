package main

import (
	"context"
	"testing"
)

func TestBridgeWaitInputMustMatchSavedReceipt(t *testing.T) {
	for _, kind := range []string{"changed_message", "extra_input_property"} {
		t.Run(kind, func(t *testing.T) {
			upstream := newBridgeTestUpstream(t, true)
			caller := &bridge{config: bridgeTestConfig(t, upstream)}
			intent := receiptTestInvocation()
			result, first := bridgeTestInvoke(t, context.Background(), caller, intent)
			if result.IsError || first.Outcome != "success" {
				t.Fatal("initial invocation did not establish a successful retained operation")
			}

			message := intent.Message
			if kind == "changed_message" {
				message = "A different operation replaced the run after preflight."
			}
			replacement := map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": message}},
			}
			if kind == "extra_input_property" {
				replacement["unrequested_instruction"] = "An input property outside the saved intent."
			}
			upstream.mu.Lock()
			upstream.waitInputOverride = replacement
			upstream.mu.Unlock()

			result, observed := bridgeTestInvoke(t, context.Background(), caller, intent)
			if !result.IsError || observed.Outcome != "delivery_unknown" || observed.RunID != first.RunID {
				t.Fatalf("a changed wait response became this saved operation's success: %+v", observed)
			}
			upstream.mu.Lock()
			defer upstream.mu.Unlock()
			if upstream.reads != 1 || upstream.waits != 1 || upstream.calls != 1 {
				t.Fatalf("expected valid preflight, changed wait and no redispatch; reads=%d waits=%d calls=%d",
					upstream.reads, upstream.waits, upstream.calls)
			}
		})
	}
}
