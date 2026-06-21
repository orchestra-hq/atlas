package worker

import (
	"context"
	"testing"

	"github.com/orchestra-hq/atlas/internal/core"
)

// captureAdapter is an engineAdapter that records the request it last saw, so a
// test can assert what the worker handed the engine after defaulting.
type captureAdapter struct{ last core.Request }

func (c *captureAdapter) Execute(_ context.Context, req core.Request) (core.Response, error) {
	c.last = req
	return core.Response{}, nil
}

func (c *captureAdapter) ExecuteStream(_ context.Context, req core.Request, _ core.StreamSink) error {
	c.last = req
	return nil
}

func (c *captureAdapter) CountTokens(_ context.Context, req core.Request) (int, error) {
	c.last = req
	return 0, nil
}

func (c *captureAdapter) ContextWindow(context.Context) (int, error) { return 0, nil }

func ptr(f float64) *float64 { return &f }

// TestSamplingDefaultsApplied: a request that omits temperature/top_p picks up the
// worker's catalog defaults, while an explicit value is left untouched, and the two
// fields default independently.
func TestSamplingDefaultsApplied(t *testing.T) {
	adp := &captureAdapter{}
	w := &Worker{
		cfg:     Config{Temperature: ptr(0.7), TopP: ptr(0.8)},
		adapter: adp,
	}

	t.Run("both omitted use catalog defaults", func(t *testing.T) {
		if _, err := w.Execute(context.Background(), core.Request{}); err != nil {
			t.Fatal(err)
		}
		if got := adp.last.Temperature; got == nil || *got != 0.7 {
			t.Errorf("temperature = %v, want 0.7", deref(got))
		}
		if got := adp.last.TopP; got == nil || *got != 0.8 {
			t.Errorf("top_p = %v, want 0.8", deref(got))
		}
	})

	t.Run("explicit values win", func(t *testing.T) {
		req := core.Request{Temperature: ptr(0.1), TopP: ptr(0.2)}
		if _, err := w.Execute(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if got := adp.last.Temperature; got == nil || *got != 0.1 {
			t.Errorf("temperature = %v, want 0.1 (client value)", deref(got))
		}
		if got := adp.last.TopP; got == nil || *got != 0.2 {
			t.Errorf("top_p = %v, want 0.2 (client value)", deref(got))
		}
	})

	t.Run("fields default independently", func(t *testing.T) {
		// Client set top_p but not temperature: temperature defaults, top_p survives.
		req := core.Request{TopP: ptr(0.99)}
		if _, err := w.Execute(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if got := adp.last.Temperature; got == nil || *got != 0.7 {
			t.Errorf("temperature = %v, want 0.7 (defaulted)", deref(got))
		}
		if got := adp.last.TopP; got == nil || *got != 0.99 {
			t.Errorf("top_p = %v, want 0.99 (client value)", deref(got))
		}
	})

	t.Run("streaming path defaults too", func(t *testing.T) {
		if err := w.ExecuteStream(context.Background(), core.Request{}, &recordingSink{}); err != nil {
			t.Fatal(err)
		}
		if got := adp.last.Temperature; got == nil || *got != 0.7 {
			t.Errorf("stream temperature = %v, want 0.7", deref(got))
		}
	})

	t.Run("the request copy is mutated, not the caller's", func(t *testing.T) {
		req := core.Request{} // caller's value: no sampling set
		if _, err := w.Execute(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if req.Temperature != nil {
			t.Error("caller's request was mutated; defaulting must act on a copy")
		}
	})
}

// TestSamplingNoDefaults: a worker with no catalog sampling (raw path/spec) leaves
// an omitted field nil, so the engine applies its own default.
func TestSamplingNoDefaults(t *testing.T) {
	adp := &captureAdapter{}
	w := &Worker{adapter: adp} // cfg.Temperature/TopP nil
	if _, err := w.Execute(context.Background(), core.Request{}); err != nil {
		t.Fatal(err)
	}
	if adp.last.Temperature != nil || adp.last.TopP != nil {
		t.Errorf("sampling = (%v, %v), want both nil (no catalog default)", adp.last.Temperature, adp.last.TopP)
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
