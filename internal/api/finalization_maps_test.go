package api

import (
	"sync/atomic"
	"testing"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/transport"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

type finalizationTestSpan struct {
	trace.Span
	ended atomic.Bool
}

func (s *finalizationTestSpan) End(...trace.SpanEndOption) { s.ended.Store(true) }

func TestFinalizationLocalStateBelongsToOriginalRecord(t *testing.T) {
	server := NewServer(nil, nil, nil, nil)
	_, base := noop.NewTracerProvider().Tracer("finalization-test").Start(t.Context(), "record")
	oldSpan := &finalizationTestSpan{Span: base}
	newSpan := &finalizationTestSpan{Span: base}
	oldKey := transport.RunRecordKey("same-run", "old-record")
	newKey := transport.RunRecordKey("same-run", "new-record")
	server.runSpans.Store(oldKey, oldSpan)
	server.runSpans.Store(newKey, newSpan)
	old := &models.Run{RunID: "same-run", IncarnationID: "old-record", Status: models.RunStatusSuccess}
	if err := server.finishRun(old); err != nil {
		t.Fatal(err)
	}
	if !oldSpan.ended.Load() || newSpan.ended.Load() {
		t.Fatal("old finalization ended the wrong record's span")
	}
	if _, ok := server.runSpans.Load(newKey); !ok {
		t.Fatal("replacement span was removed")
	}
	if _, ok := server.finishedRuns.Load(newKey); ok {
		t.Fatal("old finalization marked its replacement finished")
	}
	current := &models.Run{RunID: "same-run", IncarnationID: "new-record", Status: models.RunStatusSuccess}
	if err := server.finishRun(current); err != nil {
		t.Fatal(err)
	}
	if !newSpan.ended.Load() {
		t.Fatal("current finalization failed to end its own span")
	}
	if _, ok := server.finishedRuns.Load(newKey); !ok {
		t.Fatal("current finalization was suppressed by an older record")
	}
}
