package api

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/sharanharsoor/runkite/internal/metrics"
)

func counterValue(t *testing.T, op string) float64 {
	t.Helper()
	c, err := metrics.StatusTransitionErrorsTotal.GetMetricWithLabelValues(op)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues(%q): %v", op, err)
	}
	m := &dto.Metric{}
	if err := c.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return m.GetCounter().GetValue()
}

func TestTryStatusTransition_retriesOnceThenSucceeds(t *testing.T) {
	calls := 0
	ok := tryStatusTransition("test_retry_ok", "t1", "r1", func() error {
		calls++
		if calls == 1 {
			return errors.New("transient")
		}
		return nil
	})
	if !ok {
		t.Fatal("expected success after retry")
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (fail then succeed)", calls)
	}
}

func TestTryStatusTransition_noRetryOnSuccess(t *testing.T) {
	calls := 0
	ok := tryStatusTransition("test_ok", "t1", "r1", func() error {
		calls++
		return nil
	})
	if !ok || calls != 1 {
		t.Fatalf("ok=%v calls=%d, want ok=true calls=1", ok, calls)
	}
}

func TestTryStatusTransition_incrementsMetricOnPersistentFailure(t *testing.T) {
	op := "test_persistent_fail"
	before := counterValue(t, op)
	calls := 0
	ok := tryStatusTransition(op, "t-stuck", "r-stuck", func() error {
		calls++
		return errors.New("store down")
	})
	if ok {
		t.Fatal("expected failure")
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (initial + retry)", calls)
	}
	after := counterValue(t, op)
	if after-before != 1 {
		t.Fatalf("metric delta = %v, want 1", after-before)
	}
}
