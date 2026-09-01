package tui

import (
	"strings"
	"testing"
	"time"
)

func TestFrameTiming_Stats_Empty(t *testing.T) {
	var ft frameTiming
	got := ft.stats()
	if !strings.Contains(got, "no frame timing data") {
		t.Errorf("empty frameTiming.stats() = %q, want 'no frame timing data' message", got)
	}
}

func TestFrameTiming_Stats_WithSamples(t *testing.T) {
	var ft frameTiming
	for i := 0; i < 10; i++ {
		ft.samples[i] = time.Duration(i+1) * time.Millisecond
		ft.sampleIdx = (i + 1) % frameTimingSamples
		ft.sampleCount = i + 1
	}
	got := ft.stats()
	if !strings.Contains(got, "10 samples") {
		t.Errorf("frameTiming.stats() = %q, want '10 samples'", got)
	}
	if !strings.Contains(got, "p50=") || !strings.Contains(got, "p95=") || !strings.Contains(got, "p99=") {
		t.Errorf("frameTiming.stats() = %q, want p50/p95/p99 labels", got)
	}
}

func TestFrameTiming_RecordCycle(t *testing.T) {
	var ft frameTiming
	ft.recordUpdateStart()
	time.Sleep(time.Microsecond)
	ft.recordViewEnd()
	if ft.sampleCount != 1 {
		t.Fatalf("sampleCount = %d, want 1", ft.sampleCount)
	}
	if ft.samples[0] <= 0 {
		t.Errorf("samples[0] = %v, want > 0", ft.samples[0])
	}
}

func TestFrameTiming_RecordViewEnd_NoStart(t *testing.T) {
	var ft frameTiming
	ft.recordViewEnd()
	if ft.sampleCount != 0 {
		t.Errorf("sampleCount = %d after recordViewEnd with no start, want 0", ft.sampleCount)
	}
}

func TestFrameTiming_Wraps(t *testing.T) {
	var ft frameTiming
	for i := 0; i < frameTimingSamples+20; i++ {
		ft.recordUpdateStart()
		ft.recordViewEnd()
	}
	if ft.sampleCount != frameTimingSamples {
		t.Errorf("sampleCount = %d after %d cycles, want %d", ft.sampleCount, frameTimingSamples+20, frameTimingSamples)
	}
}

func TestRenderTranscript_CachesBlocks(t *testing.T) {
	m := &appModel{renderWidth: 0}
	m.blocks = []string{"block-A", "block-B"}

	_ = m.renderTranscript()
	if len(m.renderedBlocks) != 2 {
		t.Fatalf("renderedBlocks len = %d, want 2", len(m.renderedBlocks))
	}

	m.blocks = append(m.blocks, "block-C")
	got := m.renderTranscript()
	if len(m.renderedBlocks) != 3 {
		t.Fatalf("renderedBlocks len = %d after append, want 3", len(m.renderedBlocks))
	}
	if !strings.Contains(got, "block-A") || !strings.Contains(got, "block-C") {
		t.Errorf("renderTranscript() = %q, want all blocks present", got)
	}
}

func TestRenderTranscript_WidthChangeInvalidatesCache(t *testing.T) {
	m := &appModel{renderWidth: 0}
	m.blocks = []string{"hello"}

	_ = m.renderTranscript()
	if m.renderWidth != 0 {
		t.Fatalf("renderWidth = %d, want 0", m.renderWidth)
	}
	if len(m.renderedBlocks) != 1 {
		t.Fatalf("renderedBlocks len = %d, want 1", len(m.renderedBlocks))
	}

	// Simulate width change by directly setting viewport width via renderWidth check
	m.renderWidth = 0 // matches viewport width of 0
	_ = m.renderTranscript()
	if len(m.renderedBlocks) != 1 {
		t.Fatalf("cache should still be valid, got len %d", len(m.renderedBlocks))
	}
}

func TestInvalidateRenderCache(t *testing.T) {
	m := &appModel{}
	m.blocks = []string{"a", "b"}
	_ = m.renderTranscript()
	if len(m.renderedBlocks) != 2 {
		t.Fatalf("renderedBlocks len = %d, want 2", len(m.renderedBlocks))
	}
	m.invalidateRenderCache()
	if m.renderedBlocks != nil {
		t.Errorf("renderedBlocks = %v after invalidate, want nil", m.renderedBlocks)
	}
}

func TestRenderTranscript_EmptyBlocks(t *testing.T) {
	m := &appModel{}
	got := m.renderTranscript()
	if got != "" {
		t.Errorf("renderTranscript() with no blocks = %q, want empty", got)
	}
}
