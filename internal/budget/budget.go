// Package budget meters a job's model usage against its harness's limits.
// The egress proxy records the token counts it reads from model API
// responses; once a limit is reached, further model requests are refused
// and the job is stopped.
package budget

import (
	"errors"
	"fmt"
	"math"
	"sync"
)

// ErrExhausted is returned by Allow once a limit is reached.
var ErrExhausted = errors.New("budget exhausted")

// ErrBusy is returned by Begin when MaxInFlight requests are running.
var ErrBusy = errors.New("too many concurrent model requests")

// MaxInFlight bounds a job's concurrent metered requests. Every in-flight
// request passed the budget check before any of them was counted, so this
// also bounds how far a budget can be overshot.
const MaxInFlight = 4

// Limits are a job's budget. Zero means "no limit" for that dimension.
type Limits struct {
	MaxTokens  int64
	MaxCostUSD float64
}

// Price is a model's price in USD per million tokens.
type Price struct {
	InputPerMTok  float64 `json:"input" yaml:"input"`
	OutputPerMTok float64 `json:"output" yaml:"output"`
}

// Usage is what a job has used so far.
type Usage struct {
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	CostUSD      float64 `json:"costUSD"`
	// CostKnown is false when the model has no price: cost is then
	// neither counted nor limited.
	CostKnown bool `json:"costKnown"`
}

// Meter tracks one job. It is safe for concurrent use.
type Meter struct {
	limits     Limits
	price      *Price
	onExceeded func(reason string)

	mu       sync.Mutex
	usage    Usage
	exceeded string
	inFlight int
}

// New returns a meter. price may be nil (cost unknown). onExceeded is
// called once, outside the meter's lock, when a limit is first reached.
func New(limits Limits, price *Price, onExceeded func(reason string)) *Meter {
	m := &Meter{limits: limits, price: price, onExceeded: onExceeded}
	m.usage.CostKnown = price != nil
	return m
}

// Allow reports whether another model request may be made.
func (m *Meter) Allow() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.exceeded != "" {
		return fmt.Errorf("%w: %s", ErrExhausted, m.exceeded)
	}
	return nil
}

// Begin admits one metered request: the budget must not be exhausted and
// fewer than MaxInFlight requests may be running. Call release when the
// request (including its response body) is done.
func (m *Meter) Begin() (release func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.exceeded != "" {
		return nil, fmt.Errorf("%w: %s", ErrExhausted, m.exceeded)
	}
	if m.inFlight >= MaxInFlight {
		return nil, ErrBusy
	}
	m.inFlight++
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			m.inFlight--
			m.mu.Unlock()
		})
	}, nil
}

// Fail stops the budget for a reason other than a limit, e.g. a model
// response whose usage cannot be read: without a count, nothing is safe.
func (m *Meter) Fail(reason string) {
	m.mu.Lock()
	first := m.exceeded == ""
	if first {
		m.exceeded = reason
	}
	m.mu.Unlock()
	if first && m.onExceeded != nil {
		m.onExceeded(reason)
	}
}

// Record adds token counts (a whole response's, or a streamed increment).
func (m *Meter) Record(input, output int64) {
	if input < 0 || output < 0 {
		return
	}
	m.mu.Lock()
	m.usage.InputTokens = addSat(m.usage.InputTokens, input)
	m.usage.OutputTokens = addSat(m.usage.OutputTokens, output)
	if m.price != nil {
		m.usage.CostUSD += float64(input)/1e6*m.price.InputPerMTok + float64(output)/1e6*m.price.OutputPerMTok
	}
	var reason string
	if m.exceeded == "" {
		total := addSat(m.usage.InputTokens, m.usage.OutputTokens)
		switch {
		case m.limits.MaxTokens > 0 && total >= m.limits.MaxTokens:
			reason = fmt.Sprintf("used %d of %d tokens", total, m.limits.MaxTokens)
		case m.price != nil && m.limits.MaxCostUSD > 0 && m.usage.CostUSD >= m.limits.MaxCostUSD:
			reason = fmt.Sprintf("spent $%.4f of $%.2f", m.usage.CostUSD, m.limits.MaxCostUSD)
		}
		m.exceeded = reason
	}
	m.mu.Unlock()
	if reason != "" && m.onExceeded != nil {
		m.onExceeded(reason)
	}
}

// Usage returns a snapshot.
func (m *Meter) Usage() Usage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usage
}

// Exceeded returns why the budget is exhausted, or "".
func (m *Meter) Exceeded() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.exceeded
}

// addSat adds non-negative counts, saturating instead of overflowing (a
// saturated count is over every limit).
func addSat(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
