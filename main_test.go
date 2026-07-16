package main

import (
	"encoding/json"
	"testing"
)

type codexUsageReportFixture struct {
	Sessions []codexUsageSessionFixture `json:"sessions"`
}

type codexUsageSessionFixture struct {
	InputTokens         int64   `json:"inputTokens"`
	OutputTokens        int64   `json:"outputTokens"`
	CacheCreationTokens int64   `json:"cacheCreationTokens,omitempty"`
	CacheReadTokens     int64   `json:"cacheReadTokens,omitempty"`
	CachedInputTokens   int64   `json:"cachedInputTokens,omitempty"`
	TotalTokens         int64   `json:"totalTokens"`
	CostUSD             float64 `json:"costUSD"`
	LastActivity        string  `json:"lastActivity"`
}

func TestParseCcusageCodex_UsesCurrentCacheFields(t *testing.T) {
	got := parseCodexUsageFixture(t, codexUsageSessionFixture{
		InputTokens: 95538, CacheReadTokens: 490880, OutputTokens: 8657,
		TotalTokens: 595075, CostUSD: 1.27341, LastActivity: "2026-07-16T03:30:10Z",
	})

	assertCodexTokenUsage(t, got, 95538, 490880, 8657, 595075)
}

func TestParseCcusageCodex_SupportsLegacyCachedInputTokens(t *testing.T) {
	got := parseCodexUsageFixture(t, codexUsageSessionFixture{
		InputTokens: 1660, CachedInputTokens: 9600, OutputTokens: 14,
		TotalTokens: 11274, CostUSD: 0.01352, LastActivity: "2026-05-29T10:24:15Z",
	})

	assertCodexTokenUsage(t, got, 1660, 9600, 14, 11274)
}

func TestParseCcusageCodex_InfersCacheFromTotalTokens(t *testing.T) {
	got := parseCodexUsageFixture(t, codexUsageSessionFixture{
		InputTokens: 7345, OutputTokens: 1204, TotalTokens: 1057125,
		CostUSD: 0.18427, LastActivity: "2026-07-15T18:42:09Z",
	})

	assertCodexTokenUsage(t, got, 7345, 1048576, 1204, 1057125)
}

func TestParseCcusageCodex_PreservesRealZeroCache(t *testing.T) {
	got := parseCodexUsageFixture(t, codexUsageSessionFixture{
		InputTokens: 45280, OutputTokens: 10920, TotalTokens: 56200,
		CostUSD: 0.09218, LastActivity: "2026-07-16T02:18:44Z",
	})

	assertCodexTokenUsage(t, got, 45280, 0, 10920, 56200)
}

func TestParseCcusageCodex_PrefersCurrentFieldsOverLegacyField(t *testing.T) {
	got := parseCodexUsageFixture(t, codexUsageSessionFixture{
		InputTokens: 20000, CacheCreationTokens: 5000, CacheReadTokens: 80000,
		CachedInputTokens: 999999, OutputTokens: 2000, TotalTokens: 107000,
		CostUSD: 0.21735, LastActivity: "2026-07-16T03:11:08Z",
	})

	assertCodexTokenUsage(t, got, 20000, 85000, 2000, 107000)
}

func parseCodexUsageFixture(t *testing.T, session codexUsageSessionFixture) SessStat {
	t.Helper()

	data, err := json.Marshal(codexUsageReportFixture{Sessions: []codexUsageSessionFixture{session}})
	if err != nil {
		t.Fatalf("marshal ccusage fixture: %v", err)
	}
	stats := parseCcusageCodex(data)
	if len(stats) != 1 {
		t.Fatalf("parseCcusageCodex returned %d sessions, want 1", len(stats))
	}
	return stats[0]
}

func assertCodexTokenUsage(t *testing.T, got SessStat, wantIn, wantCache, wantOut, wantTotal int64) {
	t.Helper()

	if got.TokIn != wantIn || got.TokCache != wantCache || got.TokOut != wantOut {
		t.Fatalf("token breakdown = input %d, cache %d, output %d; want input %d, cache %d, output %d",
			got.TokIn, got.TokCache, got.TokOut, wantIn, wantCache, wantOut)
	}
	if total := got.TokIn + got.TokCache + got.TokOut; total != wantTotal {
		t.Fatalf("total tokens = %d, want %d; stat: %+v", total, wantTotal, got)
	}
}
