package main

// procs.go — 后端 spawn 的 CLI 子进程（claude / codex）注册表。
// 目的：让前端能看到「这个会话当前是不是在我们后端跑」并提供「终止」按钮，
// 避免 claude --resume -p 卡在网络/重试时悄悄堆积、吃 API 配额、连环 529。

import (
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// runProc 是一条进入注册表的子进程。键是 sid（一个会话一条）。
type runProc struct {
	Sid       string `json:"sid"`
	Agent     string `json:"agent"`
	Cwd       string `json:"cwd"`
	Prompt    string `json:"prompt"`
	Pid       int    `json:"pid"`
	StartedAt int64  `json:"startedAt"` // unix ms
	cmd       *exec.Cmd
}

var (
	procsMu sync.RWMutex
	procs   = map[string]*runProc{}
)

// registerProc 把刚 Start 的进程加入注册表并广播 SSE。
// 若同一 sid 已有旧进程，先 SIGKILL 旧的避免堆积。
func registerProc(p *runProc) {
	if p == nil || p.Sid == "" {
		return
	}
	procsMu.Lock()
	if old, ok := procs[p.Sid]; ok && old.cmd != nil && old.cmd.Process != nil {
		old.cmd.Process.Kill()
	}
	procs[p.Sid] = p
	procsMu.Unlock()
	broadcastProc(p, true)
}

// unregisterProc 在 cmd.Wait() 完成后调用。
// 只有当当前注册的 pid 等于 expectedPid 才删除，避免新进程刚 register 就被旧 Wait 删掉。
func unregisterProc(sid string, expectedPid int) {
	if sid == "" {
		return
	}
	procsMu.Lock()
	p, ok := procs[sid]
	if !ok || p.Pid != expectedPid {
		procsMu.Unlock()
		return
	}
	delete(procs, sid)
	procsMu.Unlock()
	broadcastProc(p, false)
}

func listProcs() []*runProc {
	procsMu.RLock()
	defer procsMu.RUnlock()
	out := make([]*runProc, 0, len(procs))
	for _, p := range procs {
		out = append(out, p)
	}
	return out
}

// killProcBySid 先 SIGTERM 给个机会善后，2s 后兜底 SIGKILL。
func killProcBySid(sid string) (int, bool) {
	procsMu.RLock()
	p, ok := procs[sid]
	procsMu.RUnlock()
	if !ok || p.cmd == nil || p.cmd.Process == nil {
		return 0, false
	}
	pid := p.Pid
	p.cmd.Process.Signal(syscall.SIGTERM)
	go func(cmd *exec.Cmd) {
		time.Sleep(2 * time.Second)
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
		}
	}(p.cmd)
	return pid, true
}

// killAllProcs 在 Go 后端收到 SIGTERM/SIGINT 时调用：硬 kill 所有子进程。
// 不广播 SSE —— 此时客户端要么已断、要么马上断。
func killAllProcs() {
	procsMu.Lock()
	defer procsMu.Unlock()
	for sid, p := range procs {
		if p.cmd != nil && p.cmd.Process != nil {
			p.cmd.Process.Kill()
		}
		delete(procs, sid)
	}
}

func broadcastProc(p *runProc, on bool) {
	sendSSE(map[string]any{
		"t":         "running",
		"sid":       p.Sid,
		"on":        on,
		"pid":       p.Pid,
		"agent":     p.Agent,
		"cwd":       p.Cwd,
		"prompt":    p.Prompt,
		"startedAt": p.StartedAt,
	})
}

// runListHandler 返回当前所有后端运行中的子进程，前端 boot 时调一次做初始化。
func runListHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "procs": listProcs()})
}

// runKillHandler 终止指定 sid 对应的子进程。
func runKillHandler(w http.ResponseWriter, r *http.Request) {
	sid := strings.TrimSpace(r.URL.Query().Get("sid"))
	if sid == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "缺少 sid"})
		return
	}
	pid, ok := killProcBySid(sid)
	if !ok {
		writeJSON(w, map[string]any{"ok": false, "error": "未找到运行中的进程"})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "pid": pid})
}
