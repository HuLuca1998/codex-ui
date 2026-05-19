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
	"time"
)

//go:embed index.html
var indexHTML []byte

//go:embed tailwind.js
var tailwindJS []byte

const (
	activeWindow = 90 * time.Second // 此窗口内有写入视为「进行中」
	headLimit    = 64 * 1024        // 启动时每个文件最多读取的头部字节
	detailBudget = 6 << 20          // /api/session 返回事件的字节预算
	detailMaxN   = 6000             // /api/session 返回事件的条数上限
)

// srcDef 是一个数据源（codex / claude）及其根目录。
type srcDef struct{ name, root string }

var sources []srcDef

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
	home, _ := os.UserHomeDir()
	codex := env("CODEX_SESSIONS", filepath.Join(home, ".codex", "sessions"))
	claude := env("CLAUDE_PROJECTS", filepath.Join(home, ".claude", "projects"))
	if len(os.Args) > 1 {
		codex = os.Args[1]
	}
	for _, d := range []srcDef{{"codex", codex}, {"claude", claude}} {
		if abs, err := filepath.Abs(d.root); err == nil {
			d.root = abs
		}
		if fi, err := os.Stat(d.root); err == nil && fi.IsDir() {
			sources = append(sources, d)
		}
	}
	if len(sources) == 0 {
		log.Fatal("未找到 ~/.codex/sessions 或 ~/.claude/projects")
	}

	scanParallel() // 首轮并行扫描（仅读头部），建立基线
	go func() {
		for {
			time.Sleep(800 * time.Millisecond)
			scan()
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

	ln, port := listen()
	if pf := os.Getenv("CODEXUI_PORTFILE"); pf != "" {
		os.WriteFile(pf, []byte(strconv.Itoa(port)), 0644)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	fmt.Printf("\n  ◆ Session Viewer — Codex & Claude\n")
	for _, s := range sources {
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
	start, budget := 0, 0
	for i := len(all) - 1; i >= 0; i-- {
		budget += len(all[i])
		if budget > detailBudget || total-i > detailMaxN {
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
	var cmd string
	switch sum.Source {
	case "claude":
		cmd = "claude --resume " + sum.Sid +
			" --permission-mode bypassPermissions --allow-dangerously-skip-permissions"
	case "codex":
		cmd = "codex resume " + sum.Sid
	default:
		writeJSON(w, map[string]any{"ok": false, "error": "未知来源"})
		return
	}
	if sum.Cwd != "" {
		cmd = "cd " + shellQuote(sum.Cwd) + " && " + cmd
	}
	if err := launchITerm(cmd); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "cmd": cmd})
}

// shellQuote 用单引号安全包裹 shell 参数。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// launchITerm 在 iTerm2 新窗口运行命令（无 iTerm 时回退到 Terminal）。
func launchITerm(cmd string) error {
	esc := strings.ReplaceAll(cmd, `\`, `\\`)
	esc = strings.ReplaceAll(esc, `"`, `\"`)
	var script string
	if _, err := os.Stat("/Applications/iTerm.app"); err == nil {
		script = "tell application \"iTerm\"\n" +
			"\tactivate\n" +
			"\tset w to (create window with default profile)\n" +
			"\ttell current session of w to write text \"" + esc + "\"\n" +
			"end tell"
	} else {
		script = "tell application \"Terminal\"\n\tactivate\n\tdo script \"" + esc + "\"\nend tell"
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
	sh := "codex exec resume " + shellQuote(sum.Sid) + " " + shellQuote(msg)
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
	out := make([]string, len(sources))
	for i, s := range sources {
		out[i] = s.name
	}
	return out
}

// ── 扫描与解析 ──────────────────────────────────────────────

// listFiles 返回所有数据源下的 .jsonl 文件。
func listFiles() []fileEntry {
	var out []fileEntry
	for _, s := range sources {
		name, root := s.name, s.root
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if !d.IsDir() && strings.HasSuffix(p, ".jsonl") {
				out = append(out, fileEntry{p, name, root})
			}
			return nil
		})
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
