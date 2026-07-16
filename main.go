// codex-ui — 实时观测 Codex 与 Claude Code 会话的查看器。
// 纯 Go 标准库 + 内嵌单页应用，零构建依赖。
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
	serverToken  string       // 启动时生成；LAN 访问必须携带，loopback 免
	// version 由发布脚本注入 7 位 commit hash，例如 53a82fa。
	// 本地 go run 时为 "dev"，跳过更新检查。
	version = "dev"
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
	ParentSid  string `json:"parentSid"` // Claude 子代理：父会话 sid（== 路径中 <uuid>/subagents/ 的 uuid）
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
	// Swift 从 Dock 启动时 PATH 非常贫瘠，导致 exec.Command 找不到
	// claude / codex / gh / git 等。这里抓一次 login shell 的 PATH 写回
	// 进程级别，所有后续 exec.Command 都能受益。
	enrichProcessPath()
	// 允许通过环境变量固定 token（重启保持原链接）；否则启动时随机
	if t := strings.TrimSpace(os.Getenv("CODEXUI_TOKEN")); t != "" {
		serverToken = t
	} else {
		serverToken = genToken()
	}
	reloadConfig() // 建立配置缓存
	loadPins()     // 读取置顶会话列表
	loadStats()    // 读取用量报表缓存（不自动重算，由用户触发）

	scanParallel() // 首轮并行扫描（仅读头部），建立基线
	go func() {
		for {
			cfg := reloadConfig() // 每轮重读配置，外部改动也能生效
			time.Sleep(time.Duration(cfg.Perf.ScanIntervalMs) * time.Millisecond)
			scan()
		}
	}()
	go func() {
		refreshAll() // 启动即拉一次 issue 与 PR
		for {
			time.Sleep(time.Duration(liveConfig().Issue.RefreshMinutes) * time.Minute)
			refreshAll()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/tailwind.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "max-age=86400")
		w.Write(tailwindJS)
	})
	// /api/* 全部走 token 鉴权；loopback 自动放行
	api := func(p string, h http.HandlerFunc) { mux.HandleFunc(p, requireToken(h)) }
	api("/api/sessions", sessionsHandler)
	api("/api/session", sessionDetailHandler)
	api("/api/pin", pinHandler)
	api("/api/stats", statsHandler)
	api("/api/stats/recompute", recomputeHandler)
	api("/api/stream", streamHandler)
	api("/api/resume", resumeHandler)
	api("/api/send", sendHandler)
	api("/api/config", configHandler)
	api("/api/issues", issuesHandler)
	api("/api/menubar", menubarHandler)
	api("/api/branches", branchesHandler)
	api("/api/issue-run", issueRunHandler)
	api("/api/gh/status", ghStatusHandler)
	api("/api/gh/login", ghLoginHandler)
	api("/api/gh/switch", ghSwitchHandler)
	api("/api/claude/projects", claudeProjectsHandler)
	api("/api/rescan", rescanHandler)
	api("/api/fs/ls", fsLsHandler)
	api("/api/fs/home", fsHomeHandler)
	api("/api/run", runHandler)
	api("/api/run/list", runListHandler)
	api("/api/run/kill", runKillHandler)
	api("/api/lan-url", lanURLHandler)
	// 更新机制
	api("/api/version", versionHandler)
	api("/api/update/check", updateCheckHandler)
	api("/api/update/download", updateDownloadHandler)
	api("/api/update/apply", updateApplyHandler)
	api("/api/restart", restartHandler)
	// 仓库设置：gh repos 下拉 + Finder 选目录
	api("/api/gh/repos", ghReposHandler)
	api("/api/folder/pick", folderPickHandler)
	api("/api/open-in-editor", openInEditorHandler)
	// GitHub 模块页面：按 state 实时查询 issue / PR + 正文评论详情
	api("/api/gh/issues", ghPageIssuesHandler)
	api("/api/gh/prs", ghPagePRsHandler)
	api("/api/gh/detail", ghDetailHandler)

	// SIGTERM/SIGINT：Swift 关 App 时 serverProcess.terminate() 走这里 —— 把
	// 自己 spawn 的 claude/codex 子进程一并杀掉，避免留下永生孤儿吃 API 配额。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		killAllProcs()
		os.Exit(0)
	}()

	// 启动更新检查后台轮询（开发构建时跳过）
	startUpdater()

	ln, port := listen()
	if pf := os.Getenv("CODEXUI_PORTFILE"); pf != "" {
		os.WriteFile(pf, []byte(strconv.Itoa(port)), 0644)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	fmt.Printf("\n  ◆ Session Viewer — Codex & Claude\n")
	for _, s := range currentSources() {
		fmt.Printf("  ◆ 数据源 %-7s %s\n", s.name, s.root)
	}
	fmt.Printf("  ◆ 本机           %s\n", url)
	fmt.Printf("  ◆ token          %s\n", serverToken)
	for _, u := range lanURLs(port) {
		fmt.Printf("  ◆ 局域网         %s\n", u)
	}
	fmt.Println()
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
	rng := r.URL.Query().Get("range") // today|week|month|all，默认 today
	if rng == "" {
		rng = "today"
	}
	mu.Lock()
	list := make([]Summary, 0, len(states))
	for _, st := range states {
		// 范围内的会话照常返回；置顶会话不受日期限制，始终带上
		if inDateRange(st.sum.Mtime, rng) || isPinned(st.sum.ID) {
			list = append(list, st.sum)
		}
	}
	mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Mtime > list[j].Mtime })
	writeJSON(w, map[string]any{"sessions": list, "sources": sourceNames(), "pins": pinnedList()})
}

// inDateRange 判断毫秒时间戳是否落在 today|week|month|all 范围内。
func inDateRange(ms int64, rng string) bool {
	if rng == "all" || ms == 0 {
		return true
	}
	t := time.UnixMilli(ms)
	now := time.Now()
	switch rng {
	case "week":
		start := now.AddDate(0, 0, -((int(now.Weekday())+6)%7)) // 周一为一周起点
		start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, now.Location())
		return !t.Before(start)
	case "month":
		return t.Year() == now.Year() && t.Month() == now.Month()
	default: // today
		return t.Year() == now.Year() && t.YearDay() == now.YearDay()
	}
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
	pid := cmd.Process.Pid
	registerProc(&runProc{
		Sid: sum.Sid, Agent: "codex", Cwd: sum.Cwd, Prompt: msg,
		Pid: pid, StartedAt: time.Now().UnixMilli(), cmd: cmd,
	})
	go func() { cmd.Wait(); unregisterProc(sum.Sid, pid) }()
	writeJSON(w, map[string]any{"ok": true, "pid": pid})
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
		if idx := strings.Index(fe.path, "/subagents/"); idx >= 0 {
			st.sum.AgentRole = "subagent"
			// 父 sid = 包含 subagents 目录的上一层目录名
			st.sum.ParentSid = filepath.Base(fe.path[:idx])
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
		role, parent := st.sum.AgentRole, st.sum.ParentSid
		st.sum = Summary{ID: id, File: fe.path, Source: fe.source, AgentRole: role, ParentSid: parent}
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
	// 默认绑全网卡，让局域网可达；可用 CODEXUI_HOST=127.0.0.1 锁回本机
	host := env("CODEXUI_HOST", "0.0.0.0")
	for p := base; p < base+40; p++ {
		if ln, err := net.Listen("tcp", host+":"+strconv.Itoa(p)); err == nil {
			return ln, p
		}
	}
	log.Fatal("找不到可用端口")
	return nil, 0
}

// genToken 生成 24 字节 hex token；优先 crypto/rand，失败回退时间戳。
func genToken() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
}

// lanIPv4s 列出所有可用于 LAN 访问的 IPv4 地址（剔除回环 / down）。
func lanIPv4s() []string {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || ip4.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, ip4.String())
		}
	}
	sort.Strings(out)
	return out
}

// lanURLs 返回带 token 的 LAN 分享链接；同一 IP 不重复。
func lanURLs(port int) []string {
	ips := lanIPv4s()
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, fmt.Sprintf("http://%s:%d/?t=%s", ip, port, serverToken))
	}
	return out
}

// isLoopback 判断请求来源是否为本机 loopback；用于免 token 放行。
func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// requireToken 在非 loopback 请求上强制校验 token。
// 来源任意之一：?t= 查询、X-Token 头、codex_token cookie。
func requireToken(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if isLoopback(r) {
			h(w, r)
			return
		}
		got := r.URL.Query().Get("t")
		if got == "" {
			got = r.Header.Get("X-Token")
		}
		if got == "" {
			if c, err := r.Cookie("codex_token"); err == nil {
				got = c.Value
			}
		}
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(serverToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
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
	Watch     bool   `json:"watch"` // 是否关注（菜单栏 issue/PR 仅汇总关注的仓库）
}

// UnmarshalJSON 让缺省的 watch 字段视为「关注」，兼容升级前的旧配置。
func (r *RepoMap) UnmarshalJSON(data []byte) error {
	type alias RepoMap
	a := alias{Watch: true}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*r = RepoMap(a)
	return nil
}

// watchedRepos 返回所有已关注且仓库名非空的映射。
func watchedRepos() []RepoMap {
	var out []RepoMap
	for _, rm := range liveConfig().Repos {
		if rm.Watch && strings.TrimSpace(rm.Repo) != "" {
			out = append(out, rm)
		}
	}
	return out
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
	ExtraPaths  string `json:"extraPaths"`  // 额外前置到 PATH 的目录（换行/冒号/逗号分隔）；用于让 ccusage 等找到 node/npx/claude
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
	OnWindowClose      string `json:"onWindowClose"`    // background|quit
	NotifyOnNewItems   bool   `json:"notifyOnNewItems"` // 有新 issue/PR 时发系统通知
}

// RecentConfig 跟踪 web 启动 CLI 时用过的工作目录，做下拉快捷。
type RecentConfig struct {
	Cwds []string `json:"cwds"`
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
	Recent  RecentConfig  `json:"recent"`
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
		Startup: StartupConfig{OpenWindowOnLaunch: true, OnWindowClose: "background", NotifyOnNewItems: true},
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
	applyExtraPaths(c.General.ExtraPaths) // 把用户配置的命令目录前置进 PATH
	return c
}

// splitPathList 把用户输入的目录列表（换行 / 冒号 / 逗号 / 分号分隔）拆成干净的切片。
func splitPathList(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ':' || r == ',' || r == ';'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// applyExtraPaths 把设置里配置的目录前置到进程 PATH（已存在的跳过，幂等）。
// 用于解决 Dock 启动 PATH 贫瘠、nvm 等非默认前缀导致 ccusage 找不到 node/npx 的问题。
func applyExtraPaths(extra string) {
	dirs := splitPathList(extra)
	if len(dirs) == 0 {
		return
	}
	cur := os.Getenv("PATH")
	have := map[string]bool{}
	for _, p := range strings.Split(cur, ":") {
		if p != "" {
			have[p] = true
		}
	}
	var prefix []string
	for _, d := range dirs {
		if !have[d] {
			prefix = append(prefix, d)
			have[d] = true
		}
	}
	if len(prefix) == 0 {
		return
	}
	os.Setenv("PATH", strings.Join(prefix, ":")+":"+cur)
}

// liveConfig 返回缓存的配置（不读盘）。
func liveConfig() Config {
	if c, ok := cfgCache.Load().(Config); ok {
		return c
	}
	return defaultConfig()
}

// ── 会话置顶 ───────────────────────────────────────────────
// 置顶会话 ID 持久化到独立文件，与应用配置解耦：配置面板保存时会
// 整体覆写 ~/.codex-viewer.json，若放在一起会被清空。
var (
	pinsMu sync.Mutex
	pins   = map[string]bool{}
)

func pinsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex-viewer-pins.json")
}

// loadPins 从磁盘读取置顶会话列表。
func loadPins() {
	m := map[string]bool{}
	if data, err := os.ReadFile(pinsPath()); err == nil {
		var ids []string
		if json.Unmarshal(data, &ids) == nil {
			for _, id := range ids {
				m[id] = true
			}
		}
	}
	pinsMu.Lock()
	pins = m
	pinsMu.Unlock()
}

// pinnedList 返回当前所有置顶会话 ID。
func pinnedList() []string {
	pinsMu.Lock()
	defer pinsMu.Unlock()
	ids := make([]string, 0, len(pins))
	for id := range pins {
		ids = append(ids, id)
	}
	return ids
}

// savePins 把当前置顶列表写回磁盘。
func savePins() {
	ids := pinnedList()
	sort.Strings(ids)
	data, _ := json.MarshalIndent(ids, "", "  ")
	os.WriteFile(pinsPath(), data, 0644)
}

// pinHandler 切换会话置顶状态：POST /api/pin?id=<id>&on=1|0
func pinHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	on := r.URL.Query().Get("on") == "1"
	pinsMu.Lock()
	if on {
		pins[id] = true
	} else {
		delete(pins, id)
	}
	pinsMu.Unlock()
	savePins()
	writeJSON(w, map[string]any{"ok": true, "pinned": on})
}

// isPinned 判断会话是否被置顶。
func isPinned(id string) bool {
	pinsMu.Lock()
	defer pinsMu.Unlock()
	return pins[id]
}

// ── 用量报表 ───────────────────────────────────────────────
// 用量数据来自 ccusage（npx ccusage <source> session --json）—— 它读取同样
// 的会话日志，但按 sessionId 去重并计算成本，比自行解析更准。结果缓存到
// 磁盘，由用户「重新计算」触发刷新。前端自行按日/月/年汇总。

// statsVersion 是用量缓存的格式版本，结构变更时递增以作废旧缓存。
const statsVersion = 3

// SessStat 是单个会话的用量聚合（来自 ccusage）。
type SessStat struct {
	Source   string  `json:"source"`  // codex | claude
	Project  string  `json:"project"` // 项目名（Codex 无项目，归为 —）
	Model    string  `json:"model"`   // 主力模型
	Date     string  `json:"date"`    // YYYY-MM-DD（本地时区）
	TokIn    int64   `json:"tokIn"`    // 输入 token（不含缓存）
	TokCache int64   `json:"tokCache"` // 缓存 token（命中读取 + 创建）
	TokOut   int64   `json:"tokOut"`   // 输出 token
	Cost     float64 `json:"cost"`     // 成本（USD）
}

var (
	statsMu        sync.Mutex
	statsList      []SessStat
	statsAt        int64  // 上次计算完成时间（毫秒）
	statsComputing bool
	statsErr       string // 上次计算的错误信息（如 ccusage 不可用）
)

func statsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex-viewer-stats.json")
}

type statsFile struct {
	Version    int        `json:"version"`
	ComputedAt int64      `json:"computedAt"`
	Sessions   []SessStat `json:"sessions"`
}

// loadStats 从磁盘读取用量缓存；格式版本不符则忽略（等用户重算）。
func loadStats() {
	var c statsFile
	if data, err := os.ReadFile(statsPath()); err == nil {
		json.Unmarshal(data, &c)
	}
	if c.Version != statsVersion {
		return
	}
	statsMu.Lock()
	statsList = c.Sessions
	statsAt = c.ComputedAt
	statsMu.Unlock()
}

// saveStats 把用量缓存写回磁盘。
func saveStats() {
	statsMu.Lock()
	defer statsMu.Unlock()
	data, _ := json.MarshalIndent(statsFile{statsVersion, statsAt, statsList}, "", "  ")
	os.WriteFile(statsPath(), data, 0644)
}

// computeStats 调用 ccusage 重新统计用量并刷新缓存。
func computeStats() {
	statsMu.Lock()
	if statsComputing {
		statsMu.Unlock()
		return
	}
	statsComputing = true
	statsMu.Unlock()
	sendSSE(map[string]any{"t": "stats", "computing": true})

	cl, e1 := runCcusage("claude")
	cx, e2 := runCcusage("codex")
	all := append(cl, cx...)
	errMsg := ""
	if e1 != nil && e2 != nil {
		errMsg = "ccusage 调用失败，请确认已安装 Node / npx：" + e1.Error()
	}

	statsMu.Lock()
	statsList = all
	statsAt = time.Now().UnixMilli()
	statsErr = errMsg
	statsComputing = false
	statsMu.Unlock()
	saveStats()
	sendSSE(map[string]any{"t": "stats", "computing": false, "computedAt": statsAt})
}

// nodeBinDirs 探测常见的 node/npx 安装目录。GUI（Dock）启动时走的是 login
// shell（zsh -l 只读 .zprofile，不读 .zshrc），nvm/volta 等把 PATH 写在
// .zshrc 里的安装方式就会让 npx 找不到（exit 127）。这里把这些目录显式探测出来，
// 前置到 npx 命令的 PATH，避免依赖用户的 shell 配置。
func nodeBinDirs() []string {
	home, _ := os.UserHomeDir()
	var dirs []string
	// 设置里手填的目录最优先（applyExtraPaths 用的同一来源）
	dirs = append(dirs, splitPathList(liveConfig().General.ExtraPaths)...)
	// nvm：~/.nvm/versions/node/*/bin（可能多版本，全部加入，由 npx 选第一个可用的）
	if ms, _ := filepath.Glob(filepath.Join(home, ".nvm/versions/node/*/bin")); len(ms) > 0 {
		dirs = append(dirs, ms...)
	}
	// 其它常见前缀：homebrew、官方 pkg、volta、pnpm、fnm
	dirs = append(dirs,
		"/opt/homebrew/bin",
		"/usr/local/bin",
		filepath.Join(home, ".volta/bin"),
		filepath.Join(home, "Library/pnpm"),
		filepath.Join(home, ".local/state/fnm_multishells"),
		filepath.Join(home, ".local/bin"),
	)
	// 去重 + 仅保留真实存在的目录
	seen := map[string]bool{}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// runCcusage 经登录 shell 调用 `npx ccusage <source> session --json --offline`，
// 登录 shell 用于取得 nvm 等环境里的 npx；再叠加 nodeBinDirs() 兜底自动探测。
func runCcusage(source string) ([]SessStat, error) {
	script := ""
	if dirs := nodeBinDirs(); len(dirs) > 0 {
		script = `export PATH="` + strings.Join(dirs, ":") + `:$PATH"; `
	}
	script += "npx --prefer-offline -y ccusage " + source + " session --json --offline"
	cmd := exec.Command("/bin/zsh", "-lc", script)
	out, err := cmd.Output()
	if err != nil {
		log.Printf("ccusage %s 失败: %v", source, err)
		return nil, err
	}
	if source == "claude" {
		return parseCcusageClaude(out), nil
	}
	return parseCcusageCodex(out), nil
}

func parseCcusageClaude(data []byte) []SessStat {
	var d struct {
		Sessions []struct {
			InputTokens         int64    `json:"inputTokens"`
			OutputTokens        int64    `json:"outputTokens"`
			CacheCreationTokens int64    `json:"cacheCreationTokens"`
			CacheReadTokens     int64    `json:"cacheReadTokens"`
			TotalCost           float64  `json:"totalCost"`
			LastActivity        string   `json:"lastActivity"`
			ProjectPath         string   `json:"projectPath"`
			ModelsUsed          []string `json:"modelsUsed"`
			ModelBreakdowns     []struct {
				ModelName string  `json:"modelName"`
				Cost      float64 `json:"cost"`
			} `json:"modelBreakdowns"`
		} `json:"sessions"`
	}
	if json.Unmarshal(data, &d) != nil {
		return nil
	}
	out := make([]SessStat, 0, len(d.Sessions))
	for _, s := range d.Sessions {
		model := ""
		if len(s.ModelsUsed) > 0 {
			model = s.ModelsUsed[0]
		}
		best := -1.0
		for _, m := range s.ModelBreakdowns {
			if m.Cost > best {
				best, model = m.Cost, m.ModelName
			}
		}
		out = append(out, SessStat{
			Source: "claude", Project: cleanProject(s.ProjectPath), Model: shortModel(model),
			Date: ccDate(s.LastActivity), TokIn: s.InputTokens,
			TokCache: s.CacheCreationTokens + s.CacheReadTokens,
			TokOut:   s.OutputTokens, Cost: s.TotalCost,
		})
	}
	return out
}

func parseCcusageCodex(data []byte) []SessStat {
	var d struct {
		Sessions []struct {
			InputTokens       int64   `json:"inputTokens"`
			OutputTokens      int64   `json:"outputTokens"`
			CachedInputTokens int64   `json:"cachedInputTokens"`
			CostUSD           float64 `json:"costUSD"`
			LastActivity      string  `json:"lastActivity"`
			Models            map[string]struct {
				TotalTokens int64 `json:"totalTokens"`
			} `json:"models"`
		} `json:"sessions"`
	}
	if json.Unmarshal(data, &d) != nil {
		return nil
	}
	out := make([]SessStat, 0, len(d.Sessions))
	for _, s := range d.Sessions {
		model := ""
		var best int64 = -1
		for name, m := range s.Models {
			if m.TotalTokens > best {
				best, model = m.TotalTokens, name
			}
		}
		out = append(out, SessStat{
			Source: "codex", Project: "—", Model: shortModel(model),
			Date: ccDate(s.LastActivity), TokIn: s.InputTokens,
			TokCache: s.CachedInputTokens, TokOut: s.OutputTokens, Cost: s.CostUSD,
		})
	}
	return out
}

// ccDate 把 ccusage 的时间字段归一为本地 YYYY-MM-DD。
func ccDate(s string) string {
	if len(s) < 10 {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format("2006-01-02")
	}
	return s[:10]
}

// cleanProject 把 ccusage 的 projectPath（home 下用 - 编码的目录名）转可读。
func cleanProject(p string) string {
	if p == "" {
		return "—"
	}
	home, _ := os.UserHomeDir()
	p = strings.TrimPrefix(p, strings.ReplaceAll(home, "/", "-")+"-")
	p = strings.TrimPrefix(p, "-")
	if p == "" {
		return "—"
	}
	return p
}

// shortModel 去掉模型名末尾的日期版本号，便于展示。
func shortModel(m string) string {
	if m == "" {
		return "—"
	}
	if i := strings.LastIndex(m, "-"); i > 0 && len(m)-i == 9 {
		if _, err := strconv.Atoi(m[i+1:]); err == nil {
			return m[:i] // claude-haiku-4-5-20251001 → claude-haiku-4-5
		}
	}
	return m
}

// statsHandler 返回所有会话的用量聚合，前端按日/月/年自行汇总。
func statsHandler(w http.ResponseWriter, r *http.Request) {
	statsMu.Lock()
	list, at, computing, errMsg := statsList, statsAt, statsComputing, statsErr
	statsMu.Unlock()
	if list == nil {
		list = []SessStat{}
	}
	writeJSON(w, map[string]any{
		"sessions": list, "computedAt": at, "computing": computing, "error": errMsg,
	})
}

// recomputeHandler 触发后台重新统计用量。
func recomputeHandler(w http.ResponseWriter, r *http.Request) {
	statsMu.Lock()
	busy := statsComputing
	statsMu.Unlock()
	if !busy {
		go computeStats()
	}
	writeJSON(w, map[string]any{"ok": true, "computing": true})
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
	Comments  int     `json:"comments"` // 评论数，用于检测新增评论
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
		"--json", "number,title,labels,updatedAt,url,comments"}
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
		Number    int               `json:"number"`
		Title     string            `json:"title"`
		URL       string            `json:"url"`
		UpdatedAt string            `json:"updatedAt"`
		Labels    []Label           `json:"labels"`
		Comments  []json.RawMessage `json:"comments"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	list := make([]Issue, 0, len(raw))
	for _, r := range raw {
		list = append(list, Issue{
			Repo: repo, Number: r.Number, Title: r.Title, URL: r.URL,
			Labels: r.Labels, UpdatedAt: r.UpdatedAt, Comments: len(r.Comments),
		})
	}
	return list, nil
}

// refreshIssues 遍历关注的仓库，刷新内存里的 issue 缓存。
func refreshIssues() {
	cfg := liveConfig()
	var all []Issue
	var errs []string
	for _, rm := range watchedRepos() {
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

// refreshAll 串行刷新 issue 与 PR 缓存（refreshMu 避免并发 gh 调用）。
func refreshAll() {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	refreshIssues()
	refreshPRs()
}

// ── Pull Request ───────────────────────────────────────────

// PR 是聚合后的单条 pull request。
type PR struct {
	Repo      string  `json:"repo"`
	Number    int     `json:"number"`
	Title     string  `json:"title"`
	URL       string  `json:"url"`
	Labels    []Label `json:"labels"`
	UpdatedAt string  `json:"updatedAt"`
	Author    string  `json:"author"`
	IsDraft   bool    `json:"isDraft"`
	Reason    string  `json:"reason"`   // author（我创建）| review（待我 review）
	Comments  int     `json:"comments"` // 评论数，用于检测新增评论
}

var (
	prsMu  sync.Mutex
	prs    []PR
	prsAt  time.Time
	prsErr string
)

// ghPRList 调用 gh pr list 拉取某仓库的 open PR；
// author / search 非空时分别作为 --author / --search 条件。
func ghPRList(repo, author, search string, limit int) ([]PR, error) {
	args := []string{"pr", "list", "--repo", repo, "--state", "open",
		"--json", "number,title,labels,updatedAt,url,author,isDraft,comments"}
	if author != "" {
		args = append(args, "--author", author)
	}
	if search != "" {
		args = append(args, "--search", search)
	}
	if limit > 0 {
		args = append(args, "--limit", strconv.Itoa(limit))
	}
	cmd := exec.Command("/bin/zsh", "-lc", "gh "+shellJoin(args))
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
		Number    int               `json:"number"`
		Title     string            `json:"title"`
		URL       string            `json:"url"`
		UpdatedAt string            `json:"updatedAt"`
		IsDraft   bool              `json:"isDraft"`
		Labels    []Label           `json:"labels"`
		Comments  []json.RawMessage `json:"comments"`
		Author    struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	list := make([]PR, 0, len(raw))
	for _, r := range raw {
		list = append(list, PR{
			Repo: repo, Number: r.Number, Title: r.Title, URL: r.URL,
			Labels: r.Labels, UpdatedAt: r.UpdatedAt,
			Author: r.Author.Login, IsDraft: r.IsDraft, Comments: len(r.Comments),
		})
	}
	return list, nil
}

// ghPRs 拉取某仓库「我相关」的 PR：我创建的 + 待我 review 的，按编号去重。
func ghPRs(repo string, limit int) ([]PR, error) {
	mine, e1 := ghPRList(repo, "@me", "", limit)
	review, e2 := ghPRList(repo, "", "review-requested:@me", limit)
	if e1 != nil && e2 != nil {
		return nil, e1
	}
	seen := map[int]bool{}
	out := make([]PR, 0, len(mine)+len(review))
	for _, p := range mine {
		p.Reason = "author"
		seen[p.Number] = true
		out = append(out, p)
	}
	for _, p := range review {
		if seen[p.Number] {
			continue
		}
		p.Reason = "review"
		out = append(out, p)
	}
	return out, nil
}

// refreshPRs 遍历关注的仓库，刷新内存里的 PR 缓存。
func refreshPRs() {
	limit := liveConfig().Issue.Limit
	var all []PR
	var errs []string
	for _, rm := range watchedRepos() {
		got, err := ghPRs(rm.Repo, limit)
		if err != nil {
			errs = append(errs, rm.Repo+": "+err.Error())
			continue
		}
		all = append(all, got...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].UpdatedAt > all[j].UpdatedAt })

	prsMu.Lock()
	prs = all
	prsAt = time.Now()
	prsErr = strings.Join(errs, " · ")
	prsMu.Unlock()
}

// ── GitHub 模块页面 API ─────────────────────────────────────
// 与菜单栏缓存（issues / prs）相互独立：页面按用户选择的 state / 筛选「实时」
// 查询，这样「已关闭 / 已合并」的历史记录无需常驻后台轮询。

// GHItem 是 GitHub 页面用的 issue / PR 统一条目。
type GHItem struct {
	Repo      string  `json:"repo"`
	Number    int     `json:"number"`
	Title     string  `json:"title"`
	URL       string  `json:"url"`
	Labels    []Label `json:"labels"`
	UpdatedAt string  `json:"updatedAt"`
	State     string  `json:"state"`   // OPEN / CLOSED / MERGED
	Author    string  `json:"author"`
	IsDraft   bool    `json:"isDraft"`  // 仅 PR
	Reason    string  `json:"reason"`   // 仅 PR：author（我创建）/ review（待我 review）
	Comments  int     `json:"comments"`
}

// ghJSON 经登录 shell 跑一条 gh 命令并返回 stdout（取得完整 PATH）。
func ghJSON(args []string) ([]byte, error) {
	cmd := exec.Command("/bin/zsh", "-lc", "gh "+shellJoin(args))
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
	return out, nil
}

// repoAllowed 判断 repo 是否在配置里（防命令注入 / 越权查询）。
func repoAllowed(repo string) bool {
	if strings.TrimSpace(repo) == "" {
		return false
	}
	for _, rm := range liveConfig().Repos {
		if rm.Repo == repo {
			return true
		}
	}
	return false
}

// ghQueryIssues 按 state / assignee 实时拉取某仓库 issue。
func ghQueryIssues(repo, state, assignee string, limit int) ([]GHItem, error) {
	if state == "" {
		state = "open"
	}
	args := []string{"issue", "list", "--repo", repo, "--state", state,
		"--json", "number,title,labels,updatedAt,url,comments,state,author"}
	if strings.TrimSpace(assignee) != "" {
		args = append(args, "--assignee", assignee)
	}
	if limit <= 0 {
		limit = 50
	}
	args = append(args, "--limit", strconv.Itoa(limit))
	out, err := ghJSON(args)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Number    int               `json:"number"`
		Title     string            `json:"title"`
		URL       string            `json:"url"`
		UpdatedAt string            `json:"updatedAt"`
		State     string            `json:"state"`
		Labels    []Label           `json:"labels"`
		Comments  []json.RawMessage `json:"comments"`
		Author    struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	list := make([]GHItem, 0, len(raw))
	for _, r := range raw {
		list = append(list, GHItem{
			Repo: repo, Number: r.Number, Title: r.Title, URL: r.URL,
			Labels: r.Labels, UpdatedAt: r.UpdatedAt, State: strings.ToUpper(r.State),
			Author: r.Author.Login, Comments: len(r.Comments),
		})
	}
	return list, nil
}

// ghQueryPRItems 调 gh pr list 拉取某仓库 PR（附带 state 字段）。
func ghQueryPRItems(repo, author, search, state string, limit int) ([]GHItem, error) {
	if state == "" {
		state = "open"
	}
	args := []string{"pr", "list", "--repo", repo, "--state", state,
		"--json", "number,title,labels,updatedAt,url,author,isDraft,comments,state"}
	if author != "" {
		args = append(args, "--author", author)
	}
	if search != "" {
		args = append(args, "--search", search)
	}
	if limit <= 0 {
		limit = 50
	}
	args = append(args, "--limit", strconv.Itoa(limit))
	out, err := ghJSON(args)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Number    int               `json:"number"`
		Title     string            `json:"title"`
		URL       string            `json:"url"`
		UpdatedAt string            `json:"updatedAt"`
		State     string            `json:"state"`
		IsDraft   bool              `json:"isDraft"`
		Labels    []Label           `json:"labels"`
		Comments  []json.RawMessage `json:"comments"`
		Author    struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	list := make([]GHItem, 0, len(raw))
	for _, r := range raw {
		list = append(list, GHItem{
			Repo: repo, Number: r.Number, Title: r.Title, URL: r.URL,
			Labels: r.Labels, UpdatedAt: r.UpdatedAt, State: strings.ToUpper(r.State),
			Author: r.Author.Login, IsDraft: r.IsDraft, Comments: len(r.Comments),
		})
	}
	return list, nil
}

// ghQueryPRs 拉取某仓库「我相关」的 PR：open 时合并「我创建 + 待我 review」两路；
// merged / closed / all 时聚焦「我创建的」（review-requested 对历史 PR 无意义）。
func ghQueryPRs(repo, state string, limit int) ([]GHItem, error) {
	if state == "" || state == "open" {
		mine, e1 := ghQueryPRItems(repo, "@me", "", "open", limit)
		rev, e2 := ghQueryPRItems(repo, "", "review-requested:@me", "open", limit)
		if e1 != nil && e2 != nil {
			return nil, e1
		}
		seen := map[int]bool{}
		out := make([]GHItem, 0, len(mine)+len(rev))
		for _, p := range mine {
			p.Reason = "author"
			seen[p.Number] = true
			out = append(out, p)
		}
		for _, p := range rev {
			if seen[p.Number] {
				continue
			}
			p.Reason = "review"
			out = append(out, p)
		}
		return out, nil
	}
	items, err := ghQueryPRItems(repo, "@me", "", state, limit)
	for i := range items {
		items[i].Reason = "author"
	}
	return items, err
}

// ── GitHub 页面查询缓存 ─────────────────────────────────────
// 命中即返回（哪怕过期），过期则后台异步刷新供下次取用，实现「先给旧数据
// 秒开、后台补新」；只有首次无缓存时才同步等待 gh，避免每次切页签干等。

type ghCacheEntry struct {
	items  []GHItem
	repos  []string
	errStr string
	at     time.Time
}

var (
	ghCacheMu      sync.Mutex
	ghCacheData    = map[string]ghCacheEntry{}
	ghCacheRunning = map[string]bool{}
)

const ghCacheTTL = 60 * time.Second

// ghServe 统一「缓存优先」逻辑：compute 真正执行 gh 查询。
func ghServe(w http.ResponseWriter, key string, force bool, compute func() ([]GHItem, []string, string)) {
	if force {
		ghCacheMu.Lock()
		delete(ghCacheData, key)
		ghCacheMu.Unlock()
	}
	ghCacheMu.Lock()
	ent, ok := ghCacheData[key]
	ghCacheMu.Unlock()
	if ok {
		stale := time.Since(ent.at) >= ghCacheTTL
		writeJSON(w, map[string]any{"items": ent.items, "repos": ent.repos,
			"error": ent.errStr, "cachedAt": ent.at.UnixMilli(), "stale": stale})
		if stale {
			ghRefreshAsync(key, compute)
		}
		return
	}
	items, repos, errStr := compute()
	now := time.Now()
	ghCacheMu.Lock()
	ghCacheData[key] = ghCacheEntry{items: items, repos: repos, errStr: errStr, at: now}
	ghCacheMu.Unlock()
	writeJSON(w, map[string]any{"items": items, "repos": repos, "error": errStr,
		"cachedAt": now.UnixMilli(), "stale": false})
}

// ghRefreshAsync 后台刷新某 key 缓存（同 key 只跑一个，避免并发 gh 风暴）。
func ghRefreshAsync(key string, compute func() ([]GHItem, []string, string)) {
	ghCacheMu.Lock()
	if ghCacheRunning[key] {
		ghCacheMu.Unlock()
		return
	}
	ghCacheRunning[key] = true
	ghCacheMu.Unlock()
	go func() {
		items, repos, errStr := compute()
		ghCacheMu.Lock()
		ghCacheData[key] = ghCacheEntry{items: items, repos: repos, errStr: errStr, at: time.Now()}
		ghCacheRunning[key] = false
		ghCacheMu.Unlock()
	}()
}

// ghPageIssuesHandler 遍历关注仓库拉取 issue，?state=open|closed|all、?mine=1、?refresh=1。
func ghPageIssuesHandler(w http.ResponseWriter, r *http.Request) {
	state := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("state")))
	switch state {
	case "open", "closed", "all":
	default:
		state = "open"
	}
	assignee := ""
	if r.URL.Query().Get("mine") != "" {
		assignee = "@me"
	}
	limit := liveConfig().Issue.Limit
	compute := func() ([]GHItem, []string, string) {
		items := []GHItem{}
		repos := []string{}
		var errs []string
		for _, rm := range watchedRepos() {
			repos = append(repos, rm.Repo)
			got, err := ghQueryIssues(rm.Repo, state, assignee, limit)
			if err != nil {
				errs = append(errs, rm.Repo+": "+err.Error())
				continue
			}
			items = append(items, got...)
		}
		sort.Slice(items, func(i, j int) bool { return items[i].UpdatedAt > items[j].UpdatedAt })
		return items, repos, strings.Join(errs, " · ")
	}
	ghServe(w, "i|"+state+"|"+assignee, r.URL.Query().Get("refresh") != "", compute)
}

// ghPagePRsHandler 遍历关注仓库拉取 PR，?state=open|merged|closed|all、?refresh=1。
func ghPagePRsHandler(w http.ResponseWriter, r *http.Request) {
	state := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("state")))
	switch state {
	case "open", "merged", "closed", "all":
	default:
		state = "open"
	}
	limit := liveConfig().Issue.Limit
	compute := func() ([]GHItem, []string, string) {
		items := []GHItem{}
		repos := []string{}
		var errs []string
		for _, rm := range watchedRepos() {
			repos = append(repos, rm.Repo)
			got, err := ghQueryPRs(rm.Repo, state, limit)
			if err != nil {
				errs = append(errs, rm.Repo+": "+err.Error())
				continue
			}
			items = append(items, got...)
		}
		sort.Slice(items, func(i, j int) bool { return items[i].UpdatedAt > items[j].UpdatedAt })
		return items, repos, strings.Join(errs, " · ")
	}
	ghServe(w, "p|"+state, r.URL.Query().Get("refresh") != "", compute)
}

// ghDetailHandler 返回单条 issue / PR 的正文与评论（?type=issue|pr&repo=&number=）。
func ghDetailHandler(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	number := strings.TrimSpace(r.URL.Query().Get("number"))
	if !repoAllowed(repo) {
		writeJSON(w, map[string]any{"ok": false, "error": "未知仓库"})
		return
	}
	if _, err := strconv.Atoi(number); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "编号非法"})
		return
	}
	sub := "issue"
	if r.URL.Query().Get("type") == "pr" {
		sub = "pr"
	}
	out, err := ghJSON([]string{sub, "view", number, "--repo", repo, "--json", "body,comments"})
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	var raw struct {
		Body     string `json:"body"`
		Comments []struct {
			Body      string `json:"body"`
			CreatedAt string `json:"createdAt"`
			Author    struct {
				Login string `json:"login"`
			} `json:"author"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	cs := make([]map[string]any, 0, len(raw.Comments))
	for _, c := range raw.Comments {
		cs = append(cs, map[string]any{"author": c.Author.Login, "body": c.Body, "createdAt": c.CreatedAt})
	}
	writeJSON(w, map[string]any{"ok": true, "body": raw.Body, "comments": cs})
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
		go refreshAll()
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	writeJSON(w, liveConfig())
}

// issuesHandler 返回缓存的 issue 列表；?refresh=1 强制同步刷新。
func issuesHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") != "" {
		refreshAll()
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

// MenuSession 是菜单栏「活跃会话」标签页的单条会话。
type MenuSession struct {
	ID      string `json:"id"`
	Source  string `json:"source"`
	Title   string `json:"title"`
	Project string `json:"project"`
	Mtime   int64  `json:"mtime"`
}

// activeSessions 返回文件 mtime 在 60 秒内的会话，按 mtime 倒序。
func activeSessions() []MenuSession {
	cutoff := time.Now().UnixMilli() - 60*1000
	mu.Lock()
	out := make([]MenuSession, 0, 8)
	for _, st := range states {
		if st.sum.Mtime < cutoff {
			continue
		}
		title := st.sum.Title
		if title == "" {
			title = st.sum.Preview
		}
		project := ""
		if st.sum.Cwd != "" {
			project = filepath.Base(st.sum.Cwd)
		}
		out = append(out, MenuSession{
			ID: st.sum.ID, Source: st.sum.Source, Title: title,
			Project: project, Mtime: st.sum.Mtime,
		})
	}
	mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Mtime > out[j].Mtime })
	return out
}

// menubarHandler 一次返回菜单栏三个标签页（issue / PR / 活跃会话）所需的全部数据。
func menubarHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") != "" {
		refreshAll()
	}
	ic := liveConfig().Issue
	issuesMu.Lock()
	is, isAt, isErr := issues, issuesAt, issuesErr
	issuesMu.Unlock()
	prsMu.Lock()
	pl, plAt, plErr := prs, prsAt, prsErr
	prsMu.Unlock()
	if is == nil {
		is = []Issue{}
	}
	if pl == nil {
		pl = []PR{}
	}
	writeJSON(w, map[string]any{
		"showInMenu":    ic.ShowInMenu,
		"menuMax":       ic.MenuMax,
		"issues":        is,
		"issuesUpdated": isAt.UnixMilli(),
		"issuesError":   isErr,
		"prs":           pl,
		"prsUpdated":    plAt.UnixMilli(),
		"prsError":      plErr,
		"sessions":      activeSessions(),
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

// ghReposHandler 列出当前 gh 登录账号下的全部仓库（前端仓库设置下拉用）
// 个人仓库 + 所属组织仓库一并返回，按 nameWithOwner 去重
// 不要求 gh 登录态：未登录时返回 ok:false，前端降级到原文本输入
func ghReposHandler(w http.ResponseWriter, r *http.Request) {
	const fields = "nameWithOwner,description,isPrivate"
	// listRepos 拉某个 owner（空 = 个人账号）下的仓库
	listRepos := func(owner string) ([]map[string]any, error) {
		args := []string{"repo", "list"}
		if owner != "" {
			args = append(args, owner)
		}
		args = append(args, "--json", fields, "--limit", "300")
		out, err := exec.Command("gh", args...).Output()
		if err != nil {
			return nil, err
		}
		var arr []map[string]any
		if jerr := json.Unmarshal(out, &arr); jerr != nil {
			return nil, jerr
		}
		return arr, nil
	}

	personal, err := listRepos("")
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "gh repo list 失败：" + err.Error()})
		return
	}
	seen := map[string]bool{}
	merged := make([]map[string]any, 0, len(personal))
	add := func(arr []map[string]any) {
		for _, rm := range arr {
			name, _ := rm["nameWithOwner"].(string)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			merged = append(merged, rm)
		}
	}
	add(personal)

	// 枚举所属组织，逐个拉其仓库；失败不致命（无权限/网络），尽力而为
	if out, oerr := exec.Command("gh", "api", "user/orgs", "--jq", ".[].login").Output(); oerr == nil {
		for _, org := range strings.Fields(string(out)) {
			if arr, lerr := listRepos(org); lerr == nil {
				add(arr)
			}
		}
	}
	writeJSON(w, map[string]any{"ok": true, "repos": merged})
}

// openInEditorHandler 用 VSCode 打开指定文件（macOS 走 `open -a "Visual Studio Code"`）。
// 仅放行 ~/.codex/sessions 与 ~/.claude/projects 下的 .jsonl 文件，避免被当成任意启动器。
func openInEditorHandler(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimSpace(r.URL.Query().Get("path"))
	if p == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "缺少 path"})
		return
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	home, _ := os.UserHomeDir()
	allowed := false
	for _, root := range []string{
		filepath.Join(home, ".codex", "sessions"),
		filepath.Join(home, ".claude", "projects"),
	} {
		if strings.HasPrefix(abs, root+string(os.PathSeparator)) {
			allowed = true
			break
		}
	}
	if !allowed || !strings.HasSuffix(strings.ToLower(abs), ".jsonl") {
		writeJSON(w, map[string]any{"ok": false, "error": "路径不在允许范围"})
		return
	}
	if _, err := os.Stat(abs); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := exec.Command("open", "-a", "Visual Studio Code", abs).Start(); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "path": abs})
}

// folderPickHandler 调起 macOS 原生「选择文件夹」对话框，返回 POSIX 路径
// osascript 在用户取消时 exit 非零；当作 cancelled=true 而非错误
func folderPickHandler(w http.ResponseWriter, r *http.Request) {
	out, err := exec.Command("osascript", "-e",
		`POSIX path of (choose folder with prompt "选择本地路径")`).Output()
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "cancelled": true})
		return
	}
	path := strings.TrimRight(strings.TrimSpace(string(out)), "/")
	writeJSON(w, map[string]any{"ok": true, "path": path})
}

// ── 局域网 / 文件系统 / CLI 调度 ────────────────────────────

// lanURLHandler 返回所有可达的 LAN URL（含 token），供前端"分享链接"按钮使用。
// 端口从请求 Host 反查 —— 不依赖闭包，跟实际监听端口完全一致。
func lanURLHandler(w http.ResponseWriter, r *http.Request) {
	port := 0
	if _, p, err := net.SplitHostPort(r.Host); err == nil {
		port, _ = strconv.Atoi(p)
	}
	writeJSON(w, map[string]any{
		"urls":  lanURLs(port),
		"token": serverToken,
		"port":  port,
	})
}

// fsHomeHandler 返回 $HOME 与一些常用快捷目录，用作目录选择器默认入口。
func fsHomeHandler(w http.ResponseWriter, r *http.Request) {
	home, _ := os.UserHomeDir()
	shortcuts := []string{home}
	for _, sub := range []string{"work", "Code", "code", "Projects", "projects", "Desktop", "Documents"} {
		p := filepath.Join(home, sub)
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			shortcuts = append(shortcuts, p)
		}
	}
	writeJSON(w, map[string]any{
		"home":      home,
		"shortcuts": shortcuts,
		"recent":    liveConfig().Recent.Cwds,
	})
}

// fsLsHandler 列出指定路径下的子目录。
//   path=...       绝对路径；默认 $HOME
//   showHidden=1   是否返回点开头目录
//
// 过滤掉常见的噪音目录（node_modules / .git 等），上限 500 条避免大目录爆量。
func fsLsHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := strings.TrimSpace(q.Get("path"))
	if p == "" {
		p, _ = os.UserHomeDir()
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	fi, err := os.Stat(p)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error(), "path": p})
		return
	}
	if !fi.IsDir() {
		writeJSON(w, map[string]any{"ok": false, "error": "不是目录", "path": p})
		return
	}
	showHidden := q.Get("showHidden") == "1"
	ents, err := os.ReadDir(p)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error(), "path": p})
		return
	}
	noisy := map[string]bool{
		"node_modules": true, "__pycache__": true, ".git": true,
		"venv": true, ".venv": true, "vendor": true, "target": true,
	}
	type entry struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	dirs := make([]entry, 0, len(ents))
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		if noisy[name] {
			continue
		}
		dirs = append(dirs, entry{Name: name, Path: filepath.Join(p, name)})
		if len(dirs) >= 500 {
			break
		}
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name)
	})
	parent := filepath.Dir(p)
	if parent == p {
		parent = ""
	}
	writeJSON(w, map[string]any{
		"ok":     true,
		"path":   p,
		"parent": parent,
		"dirs":   dirs,
	})
}

// RunReq 是 /api/run 的请求体；agent ∈ {claude,codex}，mode ∈ {new,resume}。
type RunReq struct {
	Agent  string `json:"agent"`
	Mode   string `json:"mode"`
	Cwd    string `json:"cwd"`
	Sid    string `json:"sid"`
	Prompt string `json:"prompt"`
}

// runHandler 后台 spawn CLI；输出全部丢弃，依赖会话扫描器把新会话自然拉出。
// 命令参数全部用 exec.Command 的 argv 形式传，避免任何 shell 拼接 → 天然防注入。
func runHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req RunReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": "请求体解析失败：" + err.Error()})
		return
	}
	req.Agent = strings.TrimSpace(req.Agent)
	req.Mode = strings.TrimSpace(req.Mode)
	req.Cwd = strings.TrimSpace(req.Cwd)
	req.Sid = strings.TrimSpace(req.Sid)
	req.Prompt = strings.TrimSpace(req.Prompt)

	if req.Agent != "claude" && req.Agent != "codex" {
		writeJSON(w, map[string]any{"ok": false, "error": "agent 必须是 claude 或 codex"})
		return
	}
	if req.Mode != "new" && req.Mode != "resume" {
		writeJSON(w, map[string]any{"ok": false, "error": "mode 必须是 new 或 resume"})
		return
	}
	if req.Prompt == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "prompt 不能为空"})
		return
	}
	if req.Mode == "resume" && req.Sid == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "resume 模式需要 sid"})
		return
	}
	if req.Cwd == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "cwd 不能为空"})
		return
	}
	if fi, err := os.Stat(req.Cwd); err != nil || !fi.IsDir() {
		writeJSON(w, map[string]any{"ok": false, "error": "cwd 不存在或不是目录：" + req.Cwd})
		return
	}

	args := buildRunArgs(req)
	bin := args[0]
	// 显式 LookPath：报错时给出比 "executable not found in $PATH" 更友好的提示。
	resolved, err := exec.LookPath(bin)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false,
			"error": "找不到 " + bin + "；当前 PATH=" + os.Getenv("PATH"),
			"cmd":   shellJoin(args)})
		return
	}
	cmd := exec.Command(resolved, args[1:]...)
	cmd.Dir = req.Cwd
	// 不接管输出 —— 后台跑到结束即可
	if err := cmd.Start(); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error(), "cmd": shellJoin(args)})
		return
	}
	pid := cmd.Process.Pid
	// resume 模式有 sid，登记到进程表 → 前端可见 + 可终止；new 模式 sid 未知，先不登记。
	if req.Mode == "resume" && req.Sid != "" {
		registerProc(&runProc{
			Sid: req.Sid, Agent: req.Agent, Cwd: req.Cwd, Prompt: req.Prompt,
			Pid: pid, StartedAt: time.Now().UnixMilli(), cmd: cmd,
		})
		go func() { cmd.Wait(); unregisterProc(req.Sid, pid) }()
	} else {
		go cmd.Wait() // 回收僵尸
	}
	pushRecentCwd(req.Cwd)
	writeJSON(w, map[string]any{"ok": true, "pid": pid, "cmd": shellJoin(args)})
}

// buildRunArgs 把请求映射成 argv。
// claude  : claude [--resume sid] -p <prompt> --permission-mode bypassPermissions --allow-dangerously-skip-permissions
// codex   : codex exec [resume sid] <prompt>
func buildRunArgs(req RunReq) []string {
	switch req.Agent {
	case "claude":
		args := []string{"claude"}
		if req.Mode == "resume" {
			args = append(args, "--resume", req.Sid)
		}
		args = append(args, "-p", req.Prompt,
			"--permission-mode", "bypassPermissions",
			"--allow-dangerously-skip-permissions")
		return args
	case "codex":
		args := []string{"codex", "exec"}
		if req.Mode == "resume" {
			args = append(args, "resume", req.Sid)
		}
		args = append(args, req.Prompt)
		return args
	}
	return nil
}

// enrichProcessPath 把 login shell 的 PATH 写回当前进程，让 exec.LookPath
// 能找到 claude/codex/gh 等用户安装的命令。
// Dock/launchd 启动时 PATH 极简（往往只剩 /usr/bin:/bin:/usr/sbin:/sbin），
// 而 brew / pipx / claude installer 都装到非默认前缀，故必须 enrich 一次。
//
// 兜底：常见路径无脑加进 PATH，保证即使 zsh 没装/失败也能找到 brew 的命令。
func enrichProcessPath() {
	cur := os.Getenv("PATH")
	parts := strings.Split(cur, ":")
	seen := map[string]bool{}
	for _, p := range parts {
		if p != "" {
			seen[p] = true
		}
	}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		parts = append(parts, p)
		seen[p] = true
	}
	// 从 login shell 抓一次
	if out, err := exec.Command("/bin/zsh", "-lc", "echo -n $PATH").Output(); err == nil {
		for _, p := range strings.Split(strings.TrimSpace(string(out)), ":") {
			add(p)
		}
	}
	// 常见安装位置兜底
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		"/opt/homebrew/bin", "/opt/homebrew/sbin",
		"/usr/local/bin", "/usr/local/sbin",
		filepath.Join(home, ".local/bin"),
		filepath.Join(home, "bin"),
		filepath.Join(home, ".cargo/bin"),
	} {
		add(p)
	}
	os.Setenv("PATH", strings.Join(parts, ":"))
}

// pushRecentCwd 把 cwd 推到最近列表顶端，去重，保留前 10 条。
func pushRecentCwd(cwd string) {
	cfg := loadConfig()
	keep := []string{cwd}
	for _, x := range cfg.Recent.Cwds {
		if x != cwd && len(keep) < 10 {
			keep = append(keep, x)
		}
	}
	cfg.Recent.Cwds = keep
	if err := saveConfig(cfg); err == nil {
		cfgCache.Store(cfg)
	}
}
