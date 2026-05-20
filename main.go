// codex-ui — 实时观测 Codex 与 Claude Code 会话的查看器。
// 纯 Go 标准库 + 内嵌单页应用，零构建依赖。
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed index.html
var indexHTML []byte

//go:embed tailwind.js
var tailwindJS []byte

const headLimit = 64 * 1024 // 启动时每个文件最多读取的头部字节

// srcDef 是一个数据源（codex / claude）及其根目录。
type srcDef struct{ name, root string }

var (
	cfgCache     atomic.Value // 缓存的 Config，由 reloadConfig 刷新
	cliCodexRoot string       // 命令行参数覆盖的 codex 数据源目录
)

// Summary 是单个会话在列表中的概要（仅元信息，统计交由前端计算）。
type Summary struct {
	ID         string `json:"id"`
	File       string `json:"file"`
	Source     string `json:"source"` // codex | claude
	Sid        string `json:"sid"`    // 会话编号（Codex session id / Claude sessionId）
	Title      string `json:"title"`
	Preview    string `json:"preview"`
	Cwd        string `json:"cwd"`
	Model      string `json:"model"`
	Originator string `json:"originator"`
	CliVersion string `json:"cliVersion"`
	AgentRole  string `json:"agentRole"`
	AgentName  string `json:"agentName"`
	GitBranch  string `json:"gitBranch"`
	StartTime  string `json:"startTime"`
	Mtime      int64  `json:"mtime"`
	Active     bool   `json:"active"`
}

// state 跟踪一个会话文件的解析进度。
type state struct {
	sum        Summary
	source     string
	root       string
	offset     int64 // 已处理到的字节偏移（仅含完整行）
	threadName bool  // 标题是否已锁定
}

// fileEntry 是一次扫描中发现的会话文件。
type fileEntry struct{ path, source, root string }

var (
	mu     sync.Mutex
	states = map[string]*state{}

	clientsMu sync.Mutex
	clients   = map[chan string]bool{}
)

func main() {
	if len(os.Args) > 1 {
		cliCodexRoot = os.Args[1]
	}
	reloadConfig() // 建立配置缓存

	scanParallel() // 首轮并行扫描（仅读头部），建立基线
	go func() {
		for {
			cfg := reloadConfig() // 每轮重读配置，外部改动也能生效
			time.Sleep(time.Duration(cfg.Perf.ScanIntervalMs) * time.Millisecond)
			scan()
		}
	}()
	go func() {
		refreshIssues() // 启动即拉一次
		for {
			time.Sleep(time.Duration(liveConfig().Issue.RefreshMinutes) * time.Minute)
			refreshIssues()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/tailwind.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "max-age=86400")
		w.Write(tailwindJS)
	})
	mux.HandleFunc("/api/sessions", sessionsHandler)
	mux.HandleFunc("/api/session", sessionDetailHandler)
	mux.HandleFunc("/api/stream", streamHandler)
	mux.HandleFunc("/api/resume", resumeHandler)
	mux.HandleFunc("/api/send", sendHandler)
	mux.HandleFunc("/api/config", configHandler)
	mux.HandleFunc("/api/issues", issuesHandler)
	mux.HandleFunc("/api/branches", branchesHandler)
	mux.HandleFunc("/api/issue-run", issueRunHandler)
	mux.HandleFunc("/api/gh/status", ghStatusHandler)
	mux.HandleFunc("/api/gh/login", ghLoginHandler)
	mux.HandleFunc("/api/gh/switch", ghSwitchHandler)
	mux.HandleFunc("/api/claude/projects", claudeProjectsHandler)
	mux.HandleFunc("/api/rescan", rescanHandler)

	ln, port := listen()
	if pf := os.Getenv("CODEXUI_PORTFILE"); pf != "" {
		os.WriteFile(pf, []byte(strconv.Itoa(port)), 0644)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	fmt.Printf("\n  ◆ Session Viewer — Codex & Claude\n")
	for _, s := range currentSources() {
		fmt.Printf("  ◆ 数据源 %-7s %s\n", s.name, s.root)
	}
	fmt.Printf("  ◆ 打开           %s\n\n", url)
	if os.Getenv("NO_OPEN") == "" {
		go openBrowser(url)
	}
	log.Fatal(http.Serve(ln, mux))
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ── HTTP 处理 ───────────────────────────────────────────────

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func sessionsHandler(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	list := make([]Summary, 0, len(states))
	for _, st := range states {
		list = append(list, st.sum)
	}
	mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Mtime > list[j].Mtime })
	writeJSON(w, map[string]any{"sessions": list, "sources": sourceNames()})
}

func sessionDetailHandler(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	mu.Lock()
	st := states[id]
	var sum Summary
	if st != nil {
		sum = st.sum
	}
	mu.Unlock()
	if st == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	data, err := os.ReadFile(sum.File)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	all := splitLines(data)
	total := len(all)

	// 尾部窗口：超大会话只返回最近的事件，避免压垮浏览器。
	perf := liveConfig().Perf
	start, budget := 0, 0
	for i := len(all) - 1; i >= 0; i-- {
		budget += len(all[i])
		if budget > perf.DetailBudget || total-i > perf.DetailMaxN {
			start = i + 1
			break
		}
	}
	writeJSON(w, map[string]any{
		"summary":   sum,
		"events":    all[start:],
		"total":     total,
		"truncated": start > 0,
	})
}

// resumeHandler 在 iTerm2 新窗口里继续指定会话。
func resumeHandler(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	mu.Lock()
	st := states[id]
	var sum Summary
	if st != nil {
		sum = st.sum
	}
	mu.Unlock()
	if st == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if sum.Sid == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "该会话缺少 session id"})
		return
	}
	cfg := liveConfig()
	var tmpl string
	switch sum.Source {
	case "claude":
		tmpl = cfg.Claude.ResumeCmd
	case "codex":
		tmpl = cfg.Codex.ResumeCmd
	default:
		writeJSON(w, map[string]any{"ok": false, "error": "未知来源"})
		return
	}
	cmd := fillTemplate(tmpl, map[string]string{"sid": sum.Sid, "cwd": sum.Cwd}, true)
	if sum.Cwd != "" {
		cmd = "cd " + shellQuote(sum.Cwd) + " && " + cmd
	}
	if err := launchITerm(cmd, cfg.General); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "cmd": cmd})
}

// shellQuote 用单引号安全包裹 shell 参数。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellJoin 把参数逐个 shell 转义后用空格连接。
func shellJoin(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

// fillTemplate 把命令模版里的 {key} 占位符替换为对应值；
// quote=true 时对每个值做 shell 转义（防注入）。
func fillTemplate(tmpl string, vals map[string]string, quote bool) string {
	out := tmpl
	for k, v := range vals {
		rep := v
		if quote {
			rep = shellQuote(v)
		}
		out = strings.ReplaceAll(out, "{"+k+"}", rep)
	}
	return out
}

// launchITerm 在终端新窗口运行命令。按通用配置选 iTerm2 / Terminal，
// 其它值则自动选择（装了 iTerm 用 iTerm，否则 Terminal）。
func launchITerm(cmd string, g GeneralConfig) error {
	esc := strings.ReplaceAll(cmd, `\`, `\\`)
	esc = strings.ReplaceAll(esc, `"`, `\"`)
	itermScript := "tell application \"iTerm\"\n" +
		"\tactivate\n" +
		"\tset w to (create window with default profile)\n" +
		"\ttell current session of w to write text \"" + esc + "\"\n" +
		"end tell"
	terminalScript := "tell application \"Terminal\"\n\tactivate\n\tdo script \"" + esc + "\"\nend tell"
	var script string
	switch g.Terminal {
	case "terminal":
		script = terminalScript
	case "iterm":
		script = itermScript
	default:
		if _, err := os.Stat("/Applications/iTerm.app"); err == nil {
			script = itermScript
		} else {
			script = terminalScript
		}
	}
	return exec.Command("osascript", "-e", script).Start()
}

// sendHandler 后台运行 codex exec resume 向 Codex 会话发送一条消息。
// 输出全部丢弃 —— 运行结果会通过会话文件被实时监控自动捕获。
func sendHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	msg := strings.TrimSpace(string(body))
	mu.Lock()
	st := states[id]
	var sum Summary
	if st != nil {
		sum = st.sum
	}
	mu.Unlock()
	if st == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if sum.Source != "codex" {
		writeJSON(w, map[string]any{"ok": false, "error": "仅 Codex 会话支持发送消息"})
		return
	}
	if sum.Sid == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "该会话缺少 session id"})
		return
	}
	if msg == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "消息为空"})
		return
	}
	sh := fillTemplate(liveConfig().Codex.SendCmd,
		map[string]string{"sid": sum.Sid, "input": msg}, true)
	if sum.Cwd != "" {
		sh = "cd " + shellQuote(sum.Cwd) + " && " + sh
	}
	// 登录 shell 取得完整 PATH；不接管 stdout/stderr（丢弃），后台跑到结束。
	cmd := exec.Command("/bin/zsh", "-lc", sh)
	if err := cmd.Start(); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	go cmd.Wait() // 回收进程，不读取输出
	writeJSON(w, map[string]any{"ok": true})
}

func streamHandler(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan string, 256)
	clientsMu.Lock()
	clients[ch] = true
	clientsMu.Unlock()
	defer func() {
		clientsMu.Lock()
		delete(clients, ch)
		clientsMu.Unlock()
	}()

	fmt.Fprint(w, "retry: 2000\n\n")
	fl.Flush()
	for {
		select {
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			fl.Flush()
		case <-time.After(20 * time.Second):
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func sourceNames() []string {
	srcs := currentSources()
	out := make([]string, len(srcs))
	for i, s := range srcs {
		out[i] = s.name
	}
	return out
}

// ── 扫描与解析 ──────────────────────────────────────────────

// resolveRoot 依次按 配置值 → 命令行参数 → 环境变量 → 默认值 解析数据源目录。
func resolveRoot(configured, cli, envKey, def string) string {
	r := strings.TrimSpace(configured)
	if r == "" {
		r = cli
	}
	if r == "" {
		r = env(envKey, def)
	}
	if abs, err := filepath.Abs(r); err == nil {
		r = abs
	}
	return r
}

// currentSources 按当前配置返回启用且存在的数据源列表。
func currentSources() []srcDef {
	cfg := liveConfig()
	home, _ := os.UserHomeDir()
	var out []srcDef
	if cfg.Codex.Enabled {
		root := resolveRoot(cfg.Codex.SessionsPath, cliCodexRoot,
			"CODEX_SESSIONS", filepath.Join(home, ".codex", "sessions"))
		if fi, err := os.Stat(root); err == nil && fi.IsDir() {
			out = append(out, srcDef{"codex", root})
		}
	}
	if cfg.Claude.Enabled {
		root := resolveRoot(cfg.Claude.ProjectsPath, "",
			"CLAUDE_PROJECTS", filepath.Join(home, ".claude", "projects"))
		if fi, err := os.Stat(root); err == nil && fi.IsDir() {
			out = append(out, srcDef{"claude", root})
		}
	}
	return out
}

// listFiles 返回所有启用数据源下的 .jsonl 文件。
// Claude 若配置了 watchedProjects，则只遍历选中的项目子目录。
func listFiles() []fileEntry {
	cfg := liveConfig()
	var out []fileEntry
	for _, s := range currentSources() {
		name, root := s.name, s.root
		walkRoots := []string{root}
		if name == "claude" && len(cfg.Claude.WatchedProjects) > 0 {
			walkRoots = walkRoots[:0]
			for _, p := range cfg.Claude.WatchedProjects {
				if p = strings.TrimSpace(p); p != "" {
					walkRoots = append(walkRoots, filepath.Join(root, p))
				}
			}
		}
		for _, wr := range walkRoots {
			filepath.WalkDir(wr, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if !d.IsDir() && strings.HasSuffix(p, ".jsonl") {
					out = append(out, fileEntry{p, name, root})
				}
				return nil
			})
		}
	}
	return out
}

// scan 轮询：处理文件的新增 / 增长 / 失活。
func scan() {
	for _, fe := range listFiles() {
		if fi, err := os.Stat(fe.path); err == nil {
			processFile(fe, fi, true, false)
		}
	}
}

// scanParallel 并行扫描所有文件，仅用于启动建立基线。
func scanParallel() {
	files := listFiles()
	sem := make(chan struct{}, runtime.NumCPU()*2)
	var wg sync.WaitGroup
	for _, fe := range files {
		fi, err := os.Stat(fe.path)
		if err != nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(fe fileEntry, fi os.FileInfo) {
			defer wg.Done()
			defer func() { <-sem }()
			processFile(fe, fi, false, true)
		}(fe, fi)
	}
	wg.Wait()
}

// processFile 处理单个会话文件。startup=true 时新文件仅解析头部。
func processFile(fe fileEntry, fi os.FileInfo, broadcast, startup bool) {
	id := fe.source + "/" + relOf(fe.root, fe.path)
	mu.Lock()
	st := states[id]
	isNew := st == nil
	if isNew {
		st = &state{
			sum:    Summary{ID: id, File: fe.path, Source: fe.source},
			source: fe.source, root: fe.root,
		}
		if strings.Contains(fe.path, "/subagents/") {
			st.sum.AgentRole = "subagent"
		}
		states[id] = st
	}
	size := fi.Size()
	var fresh []json.RawMessage
	changed := isNew

	switch {
	case isNew:
		to := size
		if startup && to > headLimit {
			to = headLimit
		}
		parseInto(st, fe.path, 0, to)
		st.offset = size
	case size > st.offset:
		ev, no := readLines(fe.path, st.offset, -1)
		for _, ln := range ev {
			updateSummary(st, ln)
		}
		st.offset = no
		fresh = ev
		if len(ev) > 0 {
			changed = true
		}
	case size < st.offset:
		role := st.sum.AgentRole
		st.sum = Summary{ID: id, File: fe.path, Source: fe.source, AgentRole: role}
		st.threadName = false
		to := size
		if startup && to > headLimit {
			to = headLimit
		}
		parseInto(st, fe.path, 0, to)
		st.offset = size
		changed = true
	}

	st.sum.Mtime = fi.ModTime().UnixMilli()
	activeWindow := time.Duration(liveConfig().Perf.ActiveWindowSec) * time.Second
	active := time.Since(fi.ModTime()) < activeWindow
	if active != st.sum.Active {
		st.sum.Active = active
		changed = true
	}
	sum := st.sum
	mu.Unlock()

	if broadcast && changed {
		sendSSE(map[string]any{"t": "session", "session": sum})
		if len(fresh) > 0 {
			sendSSE(map[string]any{"t": "append", "id": id, "events": fresh})
		}
	}
}

func parseInto(st *state, path string, from, to int64) {
	lines, _ := readLines(path, from, to)
	for _, ln := range lines {
		updateSummary(st, ln)
	}
}

// readLines 读取文件 [from,to) 字节（to<0 表示读到末尾），
// 返回其中的完整行以及下一个偏移。
func readLines(path string, from, to int64) ([]json.RawMessage, int64) {
	f, err := os.Open(path)
	if err != nil {
		return nil, from
	}
	defer f.Close()
	if from > 0 {
		if _, err := f.Seek(from, io.SeekStart); err != nil {
			return nil, from
		}
	}
	var rd io.Reader = f
	if to >= 0 {
		rd = io.LimitReader(f, to-from)
	}
	data, err := io.ReadAll(rd)
	if err != nil {
		return nil, from
	}
	nl := bytes.LastIndexByte(data, '\n')
	if nl < 0 {
		return nil, from
	}
	return splitLines(data[:nl+1]), from + int64(nl) + 1
}

func splitLines(data []byte) []json.RawMessage {
	var out []json.RawMessage
	for _, ln := range bytes.Split(data, []byte("\n")) {
		ln = bytes.TrimSpace(ln)
		if len(ln) == 0 {
			continue
		}
		cp := make([]byte, len(ln))
		copy(cp, ln)
		out = append(out, cp)
	}
	return out
}

// updateSummary 按数据源分发到对应解析器。
func updateSummary(st *state, raw json.RawMessage) {
	if st.source == "claude" {
		updateClaudeSummary(st, raw)
	} else {
		updateCodexSummary(st, raw)
	}
}

// ── Codex 解析 ──────────────────────────────────────────────

// codexMeta 定向解码 Codex 事件，跳过 encrypted_content 等大字段。
type codexMeta struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type          string          `json:"type"`
		ID            string          `json:"id"`
		Cwd           string          `json:"cwd"`
		Originator    string          `json:"originator"`
		CliVersion    string          `json:"cli_version"`
		ModelProvider string          `json:"model_provider"`
		AgentRole     string          `json:"agent_role"`
		AgentNickname string          `json:"agent_nickname"`
		Model         string          `json:"model"`
		ThreadName    string          `json:"thread_name"`
		Message       json.RawMessage `json:"message"`
	} `json:"payload"`
}

func updateCodexSummary(st *state, raw json.RawMessage) {
	var e codexMeta
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	if e.Timestamp != "" && st.sum.StartTime == "" {
		st.sum.StartTime = e.Timestamp
	}
	p := e.Payload
	switch e.Type {
	case "session_meta":
		setIf(&st.sum.Sid, p.ID)
		setIf(&st.sum.Cwd, p.Cwd)
		setIf(&st.sum.Originator, p.Originator)
		setIf(&st.sum.CliVersion, p.CliVersion)
		if st.sum.AgentRole == "" {
			setIf(&st.sum.AgentRole, p.AgentRole)
		}
		setIf(&st.sum.AgentName, p.AgentNickname)
		if st.sum.Model == "" {
			setIf(&st.sum.Model, p.ModelProvider)
		}
	case "turn_context":
		if p.Model != "" {
			st.sum.Model = p.Model
		}
	case "event_msg":
		switch p.Type {
		case "user_message":
			if msg := jsonStr(p.Message); msg != "" && st.sum.Preview == "" {
				st.sum.Preview = trunc(oneline(msg), 160)
				if !st.threadName {
					st.sum.Title = trunc(oneline(msg), 80)
				}
			}
		case "thread_name_updated":
			if p.ThreadName != "" {
				st.sum.Title = trunc(oneline(p.ThreadName), 80)
				st.threadName = true
			}
		}
	}
}

// ── Claude 解析 ─────────────────────────────────────────────

// claudeMeta 定向解码 Claude Code 事件。
type claudeMeta struct {
	Type       string `json:"type"`
	Timestamp  string `json:"timestamp"`
	Cwd        string `json:"cwd"`
	GitBranch  string `json:"gitBranch"`
	Version    string `json:"version"`
	SessionId  string `json:"sessionId"`
	LastPrompt string `json:"lastPrompt"`
	AiTitle    string `json:"aiTitle"`
	Message    struct {
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

func updateClaudeSummary(st *state, raw json.RawMessage) {
	var e claudeMeta
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	if e.Timestamp != "" && st.sum.StartTime == "" {
		st.sum.StartTime = e.Timestamp
	}
	setIf(&st.sum.Sid, e.SessionId)
	setIf(&st.sum.Cwd, e.Cwd)
	setIf(&st.sum.GitBranch, e.GitBranch)
	setIf(&st.sum.CliVersion, e.Version)
	switch e.Type {
	case "user":
		if e.Message.Role == "user" && st.sum.Preview == "" {
			if txt := claudeText(e.Message.Content); txt != "" {
				st.sum.Preview = trunc(oneline(txt), 160)
				if st.sum.Title == "" {
					st.sum.Title = trunc(oneline(txt), 80)
				}
			}
		}
	case "assistant":
		if st.sum.Model == "" {
			setIf(&st.sum.Model, e.Message.Model)
		}
	case "ai-title":
		if e.AiTitle != "" { // AI 生成的会话标题，优先采用
			st.sum.Title = trunc(oneline(e.AiTitle), 80)
		}
	case "last-prompt":
		if st.sum.Title == "" && e.LastPrompt != "" {
			st.sum.Title = trunc(oneline(e.LastPrompt), 80)
		}
	}
}

// claudeText 从 Claude 的 content（字符串或块数组）中提取纯文本。
func claudeText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		json.Unmarshal(raw, &s)
		return s
	}
	if raw[0] == '[' {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &blocks) != nil {
			return ""
		}
		var b strings.Builder
		for _, x := range blocks {
			if x.Type == "text" && x.Text != "" {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(x.Text)
			}
		}
		return b.String()
	}
	return ""
}

// ── 工具函数 ────────────────────────────────────────────────

func setIf(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

func jsonStr(r json.RawMessage) string {
	if len(r) == 0 || r[0] != '"' {
		return ""
	}
	var s string
	if json.Unmarshal(r, &s) != nil {
		return ""
	}
	return s
}

func oneline(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.Join(strings.Fields(s), " ")
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func relOf(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return filepath.ToSlash(r)
	}
	return filepath.ToSlash(p)
}

func sendSSE(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	msg := string(b)
	clientsMu.Lock()
	for ch := range clients {
		select {
		case ch <- msg:
		default: // 慢客户端丢弃，避免阻塞
		}
	}
	clientsMu.Unlock()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func listen() (net.Listener, int) {
	base := 7800
	if p := os.Getenv("PORT"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			base = n
		}
	}
	for p := base; p < base+40; p++ {
		if ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p)); err == nil {
			return ln, p
		}
	}
	log.Fatal("找不到可用端口")
	return nil, 0
}

func openBrowser(url string) {
	time.Sleep(300 * time.Millisecond)
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	c.Start()
}

// ── 仓库配置与 Issue ────────────────────────────────────────

// RepoMap 是一条「GitHub 仓库 → 本地代码 → 主分支」映射。
type RepoMap struct {
	Repo      string `json:"repo"` // owner/name
	LocalPath string `json:"localPath"`
	Branch    string `json:"branch"`
}

// AgentConfig 是 Codex 的监控与命令配置。
type AgentConfig struct {
	Enabled      bool   `json:"enabled"`      // 是否监控该数据源
	SessionsPath string `json:"sessionsPath"` // 数据源目录；空 = 默认 ~/.codex/sessions
	ResumeCmd    string `json:"resumeCmd"`    // iTerm 续聊命令模版
	SendCmd      string `json:"sendCmd"`      // 后台发消息命令模版
}

// ClaudeConfig 是 Claude 的监控与命令配置。
type ClaudeConfig struct {
	Enabled         bool     `json:"enabled"`
	ProjectsPath    string   `json:"projectsPath"`    // 空 = 默认 ~/.claude/projects
	WatchedProjects []string `json:"watchedProjects"` // 监控的项目子目录名；空 = 全部
	ResumeCmd       string   `json:"resumeCmd"`       // iTerm 续聊命令模版
	ContextWindow   int      `json:"contextWindow"`   // 上下文窗口大小
}

// IssueConfig 是 issue 拉取过滤与菜单栏展示配置。
type IssueConfig struct {
	Assignee       string   `json:"assignee"`       // @me / 用户名 / 空=不限
	State          string   `json:"state"`          // open|closed|all
	Labels         []string `json:"labels"`         // 标签过滤
	Limit          int      `json:"limit"`          // 拉取上限
	RefreshMinutes int      `json:"refreshMinutes"` // 刷新间隔（分钟）
	MenuMax        int      `json:"menuMax"`        // 菜单栏最多显示条数
	ShowInMenu     bool     `json:"showInMenu"`     // 是否在菜单栏显示 issue
	DetailCmd      string   `json:"detailCmd"`      // 「详情」按钮命令模版
}

// GeneralConfig 是浏览器 / 终端等通用集成配置。
type GeneralConfig struct {
	Browser     string `json:"browser"`     // chrome|safari|default|custom
	BrowserPath string `json:"browserPath"` // custom 时的 .app 路径
	Terminal    string `json:"terminal"`    // iterm|terminal|auto
}

// PerfConfig 是扫描与性能相关配置。
type PerfConfig struct {
	ScanIntervalMs  int `json:"scanIntervalMs"`  // 扫描轮询间隔
	DetailBudget    int `json:"detailBudget"`    // 大会话返回字节预算
	DetailMaxN      int `json:"detailMaxN"`      // 大会话返回条数上限
	ActiveWindowSec int `json:"activeWindowSec"` // 「活跃」判定时间窗（秒）
}

// StartupConfig 是 macOS 外壳的启动行为配置（由 Swift 侧读取应用）。
type StartupConfig struct {
	LaunchAtLogin      bool   `json:"launchAtLogin"`
	OpenWindowOnLaunch bool   `json:"openWindowOnLaunch"`
	OnWindowClose      string `json:"onWindowClose"` // background|quit
}

// Config 是持久化的应用配置（~/.codex-viewer.json）。
type Config struct {
	Repos   []RepoMap     `json:"repos"`
	Codex   AgentConfig   `json:"codex"`
	Claude  ClaudeConfig  `json:"claude"`
	Issue   IssueConfig   `json:"issue"`
	General GeneralConfig `json:"general"`
	Perf    PerfConfig    `json:"perf"`
	Startup StartupConfig `json:"startup"`
}

const (
	defCodexResume  = "codex resume {sid}"
	defCodexSend    = "codex exec resume {sid} {input}"
	defClaudeResume = "claude --resume {sid} --permission-mode bypassPermissions --allow-dangerously-skip-permissions"
	defIssueDetail  = `claude --permission-mode bypassPermissions --allow-dangerously-skip-permissions "/issue info {number}"`
)

// defaultConfig 返回所有字段都为默认值的配置。
// loadConfig 先以它打底，再用文件内容覆盖，从而对老配置（仅含 repos）向后兼容。
func defaultConfig() Config {
	return Config{
		Codex:   AgentConfig{Enabled: true, ResumeCmd: defCodexResume, SendCmd: defCodexSend},
		Claude:  ClaudeConfig{Enabled: true, ResumeCmd: defClaudeResume, ContextWindow: 1000000},
		Issue:   IssueConfig{Assignee: "@me", State: "open", Limit: 50, RefreshMinutes: 3, MenuMax: 20, ShowInMenu: true, DetailCmd: defIssueDetail},
		General: GeneralConfig{Browser: "chrome", Terminal: "iterm"},
		Perf:    PerfConfig{ScanIntervalMs: 800, DetailBudget: 6 << 20, DetailMaxN: 6000, ActiveWindowSec: 90},
		Startup: StartupConfig{OpenWindowOnLaunch: true, OnWindowClose: "background"},
	}
}

// sanitize 把非法 / 空值夹回安全范围。
func (c *Config) sanitize() {
	if c.Issue.Limit <= 0 {
		c.Issue.Limit = 50
	}
	if c.Issue.RefreshMinutes <= 0 {
		c.Issue.RefreshMinutes = 3
	}
	if c.Issue.MenuMax <= 0 {
		c.Issue.MenuMax = 20
	}
	if c.Issue.State == "" {
		c.Issue.State = "open"
	}
	if c.Issue.DetailCmd == "" {
		c.Issue.DetailCmd = defIssueDetail
	}
	if c.Perf.ScanIntervalMs < 100 {
		c.Perf.ScanIntervalMs = 800
	}
	if c.Perf.DetailBudget <= 0 {
		c.Perf.DetailBudget = 6 << 20
	}
	if c.Perf.DetailMaxN <= 0 {
		c.Perf.DetailMaxN = 6000
	}
	if c.Perf.ActiveWindowSec <= 0 {
		c.Perf.ActiveWindowSec = 90
	}
	if c.Claude.ContextWindow <= 0 {
		c.Claude.ContextWindow = 1000000
	}
	if c.Codex.ResumeCmd == "" {
		c.Codex.ResumeCmd = defCodexResume
	}
	if c.Codex.SendCmd == "" {
		c.Codex.SendCmd = defCodexSend
	}
	if c.Claude.ResumeCmd == "" {
		c.Claude.ResumeCmd = defClaudeResume
	}
	if c.General.Browser == "" {
		c.General.Browser = "chrome"
	}
	if c.General.Terminal == "" {
		c.General.Terminal = "iterm"
	}
	if c.Startup.OnWindowClose == "" {
		c.Startup.OnWindowClose = "background"
	}
}

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex-viewer.json")
}

// loadConfig 以默认值打底，再用配置文件覆盖已出现的字段（缺失字段保留默认）。
func loadConfig() Config {
	c := defaultConfig()
	if data, err := os.ReadFile(configPath()); err == nil {
		json.Unmarshal(data, &c)
	}
	c.sanitize()
	return c
}

func saveConfig(c Config) error {
	c.sanitize()
	data, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(configPath(), data, 0644)
}

// reloadConfig 从磁盘重读配置并刷新内存缓存。
func reloadConfig() Config {
	c := loadConfig()
	cfgCache.Store(c)
	return c
}

// liveConfig 返回缓存的配置（不读盘）。
func liveConfig() Config {
	if c, ok := cfgCache.Load().(Config); ok {
		return c
	}
	return defaultConfig()
}

// Label 是 issue 标签（名称 + 颜色）。
type Label struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// Issue 是聚合后的单条 issue。
type Issue struct {
	Repo      string  `json:"repo"`
	Number    int     `json:"number"`
	Title     string  `json:"title"`
	URL       string  `json:"url"`
	Labels    []Label `json:"labels"`
	UpdatedAt string  `json:"updatedAt"`
}

var (
	issuesMu  sync.Mutex // 保护 issue 缓存字段
	issues    []Issue
	issuesAt  time.Time
	issuesErr string
	refreshMu sync.Mutex // 串行化刷新，避免并发 gh 调用
)

// ghIssues 按 issue 过滤配置调用 gh CLI 拉取某仓库的 issue。
// 经登录 shell 运行以取得完整 PATH（gh 通常在 /opt/homebrew/bin）。
func ghIssues(repo string, ic IssueConfig) ([]Issue, error) {
	args := []string{"issue", "list", "--repo", repo,
		"--json", "number,title,labels,updatedAt,url"}
	if ic.State != "" {
		args = append(args, "--state", ic.State)
	}
	if strings.TrimSpace(ic.Assignee) != "" {
		args = append(args, "--assignee", ic.Assignee)
	}
	for _, l := range ic.Labels {
		if l = strings.TrimSpace(l); l != "" {
			args = append(args, "--label", l)
		}
	}
	if ic.Limit > 0 {
		args = append(args, "--limit", strconv.Itoa(ic.Limit))
	}
	sh := "gh " + shellJoin(args)
	cmd := exec.Command("/bin/zsh", "-lc", sh)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s", msg)
	}
	var raw []struct {
		Number    int     `json:"number"`
		Title     string  `json:"title"`
		URL       string  `json:"url"`
		UpdatedAt string  `json:"updatedAt"`
		Labels    []Label `json:"labels"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	list := make([]Issue, 0, len(raw))
	for _, r := range raw {
		list = append(list, Issue{
			Repo: repo, Number: r.Number, Title: r.Title, URL: r.URL,
			Labels: r.Labels, UpdatedAt: r.UpdatedAt,
		})
	}
	return list, nil
}

// refreshIssues 遍历配置中的仓库，刷新内存里的 issue 缓存。
func refreshIssues() {
	refreshMu.Lock()
	defer refreshMu.Unlock()

	cfg := liveConfig()
	var all []Issue
	var errs []string
	for _, rm := range cfg.Repos {
		if strings.TrimSpace(rm.Repo) == "" {
			continue
		}
		got, err := ghIssues(rm.Repo, cfg.Issue)
		if err != nil {
			errs = append(errs, rm.Repo+": "+err.Error())
			continue
		}
		all = append(all, got...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].UpdatedAt > all[j].UpdatedAt })

	issuesMu.Lock()
	issues = all
	issuesAt = time.Now()
	issuesErr = strings.Join(errs, " · ")
	issuesMu.Unlock()
}

// configHandler：GET 返回配置；POST 保存配置并触发一次 issue 刷新。
func configHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		c := defaultConfig() // 以默认值打底，容忍部分字段的导入 / POST
		if err := json.Unmarshal(body, &c); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": "JSON 解析失败"})
			return
		}
		if err := saveConfig(c); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		reloadConfig() // 立即刷新缓存，让新配置即时生效
		go refreshIssues()
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	writeJSON(w, liveConfig())
}

// issuesHandler 返回缓存的 issue 列表；?refresh=1 强制同步刷新。
func issuesHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") != "" {
		refreshIssues()
	}
	ic := liveConfig().Issue
	issuesMu.Lock()
	defer issuesMu.Unlock()
	writeJSON(w, map[string]any{
		"issues":     issues,
		"updated":    issuesAt.UnixMilli(),
		"error":      issuesErr,
		"menuMax":    ic.MenuMax,
		"showInMenu": ic.ShowInMenu,
	})
}

// branchesHandler 返回本地仓库的分支列表（供设置页主分支下拉）。
func branchesHandler(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		writeJSON(w, map[string]any{"branches": []string{}})
		return
	}
	out, err := exec.Command("git", "-C", path, "branch", "--format=%(refname:short)").Output()
	if err != nil {
		writeJSON(w, map[string]any{"branches": []string{}, "error": "不是有效的 git 仓库"})
		return
	}
	var branches []string
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			branches = append(branches, ln)
		}
	}
	writeJSON(w, map[string]any{"branches": branches})
}

// issueRunHandler 在该 issue 仓库对应的本地映射目录里，
// 用 iTerm2 新窗口跑 `claude … "/issue info <number>"`。
func issueRunHandler(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	number := strings.TrimSpace(r.URL.Query().Get("number"))
	if repo == "" || number == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "缺少 repo 或 number"})
		return
	}
	// 编号必须是纯数字，杜绝命令注入。
	if _, err := strconv.Atoi(number); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "issue 编号非法"})
		return
	}
	cfg := liveConfig()
	var localPath string
	for _, rm := range cfg.Repos {
		if rm.Repo == repo {
			localPath = strings.TrimSpace(rm.LocalPath)
			break
		}
	}
	if localPath == "" {
		writeJSON(w, map[string]any{"ok": false,
			"error": "未配置「" + repo + "」的本地目录 —— 请在设置里填写"})
		return
	}
	if fi, err := os.Stat(localPath); err != nil || !fi.IsDir() {
		writeJSON(w, map[string]any{"ok": false, "error": "本地目录不存在：" + localPath})
		return
	}
	// number 已校验为纯数字、repo 为受限字符集、path 取自用户自己的配置，故原样替换。
	inner := fillTemplate(cfg.Issue.DetailCmd, map[string]string{
		"number": number, "repo": repo, "path": localPath}, false)
	cmd := "cd " + shellQuote(localPath) + " && " + inner
	if err := launchITerm(cmd, cfg.General); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "cmd": cmd})
}

// ── 设置相关 API ────────────────────────────────────────────

// ghStatusHandler 返回 `gh auth status` 的输出（GitHub 账号登录情况）。
func ghStatusHandler(w http.ResponseWriter, r *http.Request) {
	cmd := exec.Command("/bin/zsh", "-lc", "gh auth status")
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	text := strings.TrimSpace(outBuf.String() + "\n" + errBuf.String())
	if text == "" {
		text = "未检测到 gh CLI —— 请先安装 GitHub CLI"
	}
	writeJSON(w, map[string]any{"ok": err == nil, "text": text})
}

// ghLoginHandler 在终端里启动交互式 `gh auth login`。
func ghLoginHandler(w http.ResponseWriter, r *http.Request) {
	if err := launchITerm("gh auth login", liveConfig().General); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ghSwitchHandler 在终端里启动 `gh auth switch`（切换活跃账号）。
func ghSwitchHandler(w http.ResponseWriter, r *http.Request) {
	if err := launchITerm("gh auth switch", liveConfig().General); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// claudeProjectsHandler 列出 Claude 项目目录下的子目录（供监控多选）。
func claudeProjectsHandler(w http.ResponseWriter, r *http.Request) {
	home, _ := os.UserHomeDir()
	root := resolveRoot(liveConfig().Claude.ProjectsPath, "",
		"CLAUDE_PROJECTS", filepath.Join(home, ".claude", "projects"))
	ents, err := os.ReadDir(root)
	if err != nil {
		writeJSON(w, map[string]any{"projects": []any{}, "root": root,
			"error": "无法读取目录：" + root})
		return
	}
	type proj struct {
		Name  string `json:"name"`
		Mtime int64  `json:"mtime"`
	}
	list := make([]proj, 0, len(ents))
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		var mt int64
		if fi, err := e.Info(); err == nil {
			mt = fi.ModTime().UnixMilli()
		}
		list = append(list, proj{Name: e.Name(), Mtime: mt})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Mtime > list[j].Mtime })
	writeJSON(w, map[string]any{"projects": list, "root": root})
}

// rescanHandler 立即触发一次全量扫描。
func rescanHandler(w http.ResponseWriter, r *http.Request) {
	go scan()
	writeJSON(w, map[string]any{"ok": true})
}
