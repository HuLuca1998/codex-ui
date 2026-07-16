package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type sessionsAPIResponse struct {
	Sessions []Summary `json:"sessions"`
}

func TestSessionsAPI_ClaudeTeammateUsesTeamIdentityInsteadOfRepeatedAITitle(t *testing.T) {
	resetSessionState(t)

	lines := []string{
		`{"type":"ai-title","aiTitle":"分析并优化 Local AI 桌面设计方案","sessionId":"231c68e2-cb4b-417c-987d-153e2fcefc2d"}`,
		`{"type":"agent-name","agentName":"分析并优化 Local AI 桌面设计方案","sessionId":"231c68e2-cb4b-417c-987d-153e2fcefc2d"}`,
		`{"parentUuid":null,"teamName":"session-74589d7d","agentName":"fix-d-concurrency","type":"user","message":{"role":"user","content":"<teammate-message teammate_id=\"team-lead\">\n你是 loom 团队后端工程师。技术主管派你做安全加固批次 D：并发正确性与数据丢失。\n</teammate-message>"},"timestamp":"2026-07-16T06:35:50.318Z","cwd":"/Users/luca/work/local-ai","sessionId":"231c68e2-cb4b-417c-987d-153e2fcefc2d","version":"2.1.211","gitBranch":"main"}`,
	}

	got := processTranscriptAndFetchSummary(t, "claude", "teammate.jsonl", lines)
	if got.TeamName != "session-74589d7d" || got.TeamTitle != "分析并优化 Local AI 桌面设计方案" {
		t.Fatalf("team metadata = name %q title %q", got.TeamName, got.TeamTitle)
	}
	if got.AgentName != "fix-d-concurrency" || got.AgentRole != "teammate" {
		t.Fatalf("agent identity = name %q role %q", got.AgentName, got.AgentRole)
	}
	if got.Title != "fix-d-concurrency" {
		t.Fatalf("title = %q, want canonical teammate name", got.Title)
	}
	if strings.Contains(got.Preview, "<teammate-message") || !strings.HasPrefix(got.Preview, "你是 loom 团队后端工程师") {
		t.Fatalf("preview did not expose clean task text: %q", got.Preview)
	}
}

func TestSessionsAPI_ClaudeRegularSessionKeepsAITitlePriority(t *testing.T) {
	resetSessionState(t)

	lines := []string{
		`{"type":"user","message":{"role":"user","content":"请检查登录页面的错误提示"},"timestamp":"2026-07-16T05:10:00.000Z","cwd":"/Users/luca/github/HuLuca1998/codex-ui","sessionId":"6c2911ec-cbf8-45da-ac66-b77eaf87f04d","version":"2.1.211","gitBranch":"main"}`,
		`{"type":"ai-title","aiTitle":"诊断登录页错误提示","sessionId":"6c2911ec-cbf8-45da-ac66-b77eaf87f04d"}`,
	}

	got := processTranscriptAndFetchSummary(t, "claude", "regular.jsonl", lines)
	if got.Title != "诊断登录页错误提示" {
		t.Fatalf("title = %q, want AI title for a regular Claude session", got.Title)
	}
	if got.TeamName != "" || got.AgentRole != "" {
		t.Fatalf("regular session was incorrectly classified as a team member: %+v", got)
	}
}

func TestSessionsAPI_CodexSubagentKeepsRootAndDirectParentRelationships(t *testing.T) {
	resetSessionState(t)

	lines := []string{
		`{"timestamp":"2026-07-16T04:19:51.102Z","type":"session_meta","payload":{"id":"019f676f-8a98-75e0-8be4-ed7d629f6796","session_id":"019f64e8-409a-75c2-90ce-d7b7b9a6f698","parent_thread_id":"019f6750-b845-7ee2-b427-b4831f434720","cwd":"/Users/luca/github/BDBGAME2024/pp-game","originator":"codex_cli_rs","cli_version":"0.107.0","model_provider":"openai","agent_nickname":"Carson","agent_path":"/root/downstream_impl/concurrency_self_review"}}`,
		`{"timestamp":"2026-07-16T04:19:51.102Z","type":"session_meta","payload":{"id":"019f64e8-409a-75c2-90ce-d7b7b9a6f698","session_id":"019f64e8-409a-75c2-90ce-d7b7b9a6f698","cwd":"/Users/luca/github/BDBGAME2024/pp-game/.worktrees/parent","originator":"codex_cli_rs"}}`,
		`{"timestamp":"2026-07-16T04:20:03.000Z","type":"event_msg","payload":{"type":"agent_message","message":"I will review the concurrency boundaries and report concrete findings."}}`,
	}

	got := processTranscriptAndFetchSummary(t, "codex", "rollout-subagent.jsonl", lines)
	if got.Sid != "019f676f-8a98-75e0-8be4-ed7d629f6796" {
		t.Fatalf("sid = %q, inherited parent session_meta overwrote the child", got.Sid)
	}
	if got.ParentSid != "019f64e8-409a-75c2-90ce-d7b7b9a6f698" ||
		got.DirectParentSid != "019f6750-b845-7ee2-b427-b4831f434720" {
		t.Fatalf("relationships = root %q direct %q", got.ParentSid, got.DirectParentSid)
	}
	if got.AgentRole != "subagent" || got.AgentName != "Carson" {
		t.Fatalf("agent identity = role %q name %q", got.AgentRole, got.AgentName)
	}
	if got.AgentPath != "/root/downstream_impl/concurrency_self_review" || got.Title != "concurrency_self_review" {
		t.Fatalf("task display = path %q title %q", got.AgentPath, got.Title)
	}
	if got.Cwd != "/Users/luca/github/BDBGAME2024/pp-game" {
		t.Fatalf("cwd = %q, inherited parent metadata must not replace child metadata", got.Cwd)
	}
}

func resetSessionState(t *testing.T) {
	t.Helper()
	mu.Lock()
	oldStates := states
	states = map[string]*state{}
	mu.Unlock()
	pinsMu.Lock()
	oldPins := pins
	pins = map[string]bool{}
	pinsMu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		states = oldStates
		mu.Unlock()
		pinsMu.Lock()
		pins = oldPins
		pinsMu.Unlock()
	})
}

func processTranscriptAndFetchSummary(t *testing.T, source, name string, lines []string) Summary {
	t.Helper()
	root := t.TempDir()
	transcript := filepath.Join(root, name)
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write realistic %s transcript: %v", source, err)
	}
	fi, err := os.Stat(transcript)
	if err != nil {
		t.Fatalf("stat transcript: %v", err)
	}
	processFile(fileEntry{path: transcript, source: source, root: root}, fi, false, false)

	req := httptest.NewRequest("GET", "/api/sessions?range=all", nil)
	rr := httptest.NewRecorder()
	sessionsHandler(rr, req)
	if rr.Code != 200 {
		t.Fatalf("sessions API status = %d, body %s", rr.Code, rr.Body.String())
	}
	var resp sessionsAPIResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode sessions API response: %v", err)
	}
	if len(resp.Sessions) != 1 {
		t.Fatalf("sessions API returned %d sessions, want 1: %+v", len(resp.Sessions), resp)
	}
	return resp.Sessions[0]
}

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
