package budget

import (
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
)

func TestTokenLimit(t *testing.T) {
	var reasons []string
	m := New(Limits{MaxTokens: 1000}, nil, func(r string) { reasons = append(reasons, r) })
	if err := m.Allow(); err != nil {
		t.Fatal(err)
	}
	m.Record(400, 100)
	if err := m.Allow(); err != nil || len(reasons) != 0 {
		t.Fatalf("under the limit: %v %v", err, reasons)
	}
	m.Record(300, 200) // 1000: reached
	if err := m.Allow(); !errors.Is(err, ErrExhausted) || !strings.Contains(err.Error(), "1000 of 1000 tokens") {
		t.Errorf("Allow = %v", err)
	}
	m.Record(10, 10) // after the limit: counted, but no second callback
	if len(reasons) != 1 {
		t.Errorf("onExceeded calls = %v, want exactly one", reasons)
	}
	if u := m.Usage(); u.InputTokens != 710 || u.OutputTokens != 310 || u.CostKnown {
		t.Errorf("usage = %+v", u)
	}
}

func TestCostLimitOnlyWithAPrice(t *testing.T) {
	unpriced := New(Limits{MaxCostUSD: 0.01}, nil, nil)
	unpriced.Record(1_000_000, 1_000_000)
	if unpriced.Allow() != nil || unpriced.Usage().CostUSD != 0 {
		t.Error("cost limited without a price")
	}

	priced := New(Limits{MaxCostUSD: 1}, &Price{InputPerMTok: 3, OutputPerMTok: 15}, nil)
	priced.Record(100_000, 40_000) // 0.30 + 0.60 = 0.90
	if priced.Allow() != nil {
		t.Fatal("limited too early")
	}
	if c := priced.Usage().CostUSD; math.Abs(c-0.90) > 1e-9 {
		t.Errorf("cost = %v, want 0.90", c)
	}
	priced.Record(0, 10_000) // +0.15
	if err := priced.Allow(); err == nil || !strings.Contains(err.Error(), "of $1.00") {
		t.Errorf("Allow = %v", err)
	}
}

func TestNoLimitsAndNegativeCounts(t *testing.T) {
	m := New(Limits{}, nil, nil)
	m.Record(-5, 3)
	m.Record(1<<40, 1<<40)
	if m.Allow() != nil {
		t.Error("limited without limits")
	}
	if u := m.Usage(); u.InputTokens != 1<<40 {
		t.Errorf("negative counts must be ignored: %+v", u)
	}
}

func TestConcurrentRecord(t *testing.T) {
	calls := 0
	var mu sync.Mutex
	m := New(Limits{MaxTokens: 5000}, nil, func(string) { mu.Lock(); calls++; mu.Unlock() })
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() { defer wg.Done(); m.Record(50, 50) }()
	}
	wg.Wait()
	if u := m.Usage(); u.InputTokens+u.OutputTokens != 10000 || calls != 1 {
		t.Errorf("usage %+v, callbacks %d", u, calls)
	}
}
