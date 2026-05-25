// 自动更新机制：30 min 轮询 GitHub Releases；用户手动触发下载；spawn 助手脚本原地替换。
//
// 状态机：
//
//	idle              ── 启动后初始
//	checking          ── 正在请求 GitHub API
//	up-to-date        ── 检查后无新版
//	available         ── 发现新版（尚未下载）
//	downloading       ── 下载中（progress 字段表 0-100）
//	ready             ── 已下载 + 解压，可应用
//	error             ── 任一阶段失败（msg 带原因）
//
// 所有状态变更通过 SSE {t:"update", ...} 广播给前端。
package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	githubOwner   = "HuLuca1998"
	githubRepo    = "codex-ui"
	updatePollDur = 30 * time.Minute
)

// updateState 当前更新状态（线程安全：用 mu 保护读写）
type updateState struct {
	mu       sync.Mutex
	Status   string `json:"status"`             // idle|checking|up-to-date|available|downloading|ready|error
	Latest   string `json:"latest,omitempty"`   // 远端最新 tag（如 v0.1.0）
	URL      string `json:"url,omitempty"`      // release 页面 URL
	AssetURL string `json:"assetUrl,omitempty"` // zip 下载直链
	Progress int    `json:"progress,omitempty"` // 0-100
	Message  string `json:"message,omitempty"`  // error 时的错误信息
	ReadyApp string `json:"-"`                  // 解压后的 .app 路径（内部用）
	CheckedAt int64 `json:"checkedAt,omitempty"` // 最近一次检查的 unix ms
}

var upd updateState

// 防止重复并发下载
var downloading atomic.Bool

func (u *updateState) snapshot() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	return map[string]any{
		"status":    u.Status,
		"latest":    u.Latest,
		"url":       u.URL,
		"assetUrl":  u.AssetURL,
		"progress":  u.Progress,
		"message":   u.Message,
		"checkedAt": u.CheckedAt,
		"current":   version,
	}
}

func (u *updateState) set(fn func(*updateState)) {
	u.mu.Lock()
	fn(u)
	u.mu.Unlock()
	// 状态变更后立刻 SSE 广播（snapshot 自带二次锁）
	m := u.snapshot()
	m["t"] = "update"
	sendSSE(m)
}

// startUpdater 在 main 启动末尾调用：dev 版跳过；release 版每 updatePollDur 拉一次。
func startUpdater() {
	if version == "dev" {
		upd.set(func(u *updateState) { u.Status = "idle" })
		return
	}
	upd.set(func(u *updateState) { u.Status = "idle" })
	go func() {
		// 启动后延迟 30s 再首次查（避免影响启动速度）
		time.Sleep(30 * time.Second)
		for {
			_ = checkUpdate()
			time.Sleep(updatePollDur)
		}
	}()
}

// checkUpdate 拉一次 GitHub Releases /latest，更新状态机；非阻塞错误，不抛 panic
func checkUpdate() error {
	upd.set(func(u *updateState) {
		u.Status = "checking"
		u.Message = ""
	})
	api := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", githubOwner, githubRepo)
	req, _ := http.NewRequest("GET", api, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "codex-ui/"+version)
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		upd.set(func(u *updateState) {
			u.Status = "error"
			u.Message = "网络请求失败：" + err.Error()
			u.CheckedAt = time.Now().UnixMilli()
		})
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		upd.set(func(u *updateState) {
			u.Status = "error"
			u.Message = fmt.Sprintf("GitHub 返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			u.CheckedAt = time.Now().UnixMilli()
		})
		return fmt.Errorf("github http %d", resp.StatusCode)
	}
	var rel struct {
		TagName    string `json:"tag_name"`
		HTMLURL    string `json:"html_url"`
		Prerelease bool   `json:"prerelease"`
		Draft      bool   `json:"draft"`
		Assets     []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		upd.set(func(u *updateState) {
			u.Status = "error"
			u.Message = "解析 GitHub 响应失败：" + err.Error()
			u.CheckedAt = time.Now().UnixMilli()
		})
		return err
	}
	// 跳过 prerelease / draft
	if rel.Draft || rel.Prerelease {
		upd.set(func(u *updateState) {
			u.Status = "up-to-date"
			u.CheckedAt = time.Now().UnixMilli()
		})
		return nil
	}
	// 找当前架构对应的 asset：Codex-Viewer-<ver>-arm64.zip
	wantArch := runtime.GOARCH                  // arm64 | amd64
	wantSuffix := "-" + wantArch + ".zip"       // -arm64.zip
	if wantArch == "amd64" {
		wantSuffix = "-x86_64.zip"             // intel 包按 macOS 习惯命名
	}
	var assetURL string
	for _, a := range rel.Assets {
		lname := strings.ToLower(a.Name)
		if strings.HasSuffix(lname, wantSuffix) {
			assetURL = a.URL
			break
		}
	}
	// 没匹配到当前架构 asset：本平台不支持自动更新
	if assetURL == "" {
		upd.set(func(u *updateState) {
			u.Latest = rel.TagName
			u.URL = rel.HTMLURL
			u.Status = "error"
			u.Message = "未找到 " + wantArch + " 架构的发布包"
			u.CheckedAt = time.Now().UnixMilli()
		})
		return nil
	}
	// 比较版本
	if !versionNewer(rel.TagName, version) {
		upd.set(func(u *updateState) {
			u.Latest = rel.TagName
			u.URL = rel.HTMLURL
			u.Status = "up-to-date"
			u.CheckedAt = time.Now().UnixMilli()
		})
		return nil
	}
	upd.set(func(u *updateState) {
		u.Latest = rel.TagName
		u.URL = rel.HTMLURL
		u.AssetURL = assetURL
		u.Status = "available"
		u.CheckedAt = time.Now().UnixMilli()
	})
	return nil
}

// versionNewer 朴素的 semver 比较：去掉 v 前缀，按 . 分段取数字比较
// 任何无法解析的段当成 0，避免 panic。
func versionNewer(remote, current string) bool {
	r := strings.TrimPrefix(strings.TrimPrefix(remote, "v"), "V")
	c := strings.TrimPrefix(strings.TrimPrefix(current, "v"), "V")
	// current 形如 "0.1.0-3-gabcdef-dirty"，只比第一个 - 之前
	if i := strings.Index(c, "-"); i >= 0 {
		c = c[:i]
	}
	rp := strings.Split(r, ".")
	cp := strings.Split(c, ".")
	for i := 0; i < 4; i++ {
		ra := segNum(rp, i)
		ca := segNum(cp, i)
		if ra != ca {
			return ra > ca
		}
	}
	return false
}

func segNum(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	// 截断到第一个非数字位
	s := parts[i]
	for j, ch := range s {
		if ch < '0' || ch > '9' {
			s = s[:j]
			break
		}
	}
	n, _ := strconv.Atoi(s)
	return n
}

// updateDir 下载/解压的目标目录
func updateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(home, "Library", "Application Support", "Codex Viewer", "updates")
	if err := os.MkdirAll(d, 0755); err != nil {
		return "", err
	}
	return d, nil
}

// downloadUpdate 把 zip 下到本地，解压出 .app
// 调用前应当 upd.Status == "available"
func downloadUpdate() error {
	if !downloading.CompareAndSwap(false, true) {
		return fmt.Errorf("已有下载任务在进行")
	}
	defer downloading.Store(false)

	upd.mu.Lock()
	url := upd.AssetURL
	latest := upd.Latest
	upd.mu.Unlock()
	if url == "" {
		return fmt.Errorf("无可下载的 asset")
	}

	upd.set(func(u *updateState) { u.Status = "downloading"; u.Progress = 0; u.Message = "" })

	dir, err := updateDir()
	if err != nil {
		upd.set(func(u *updateState) { u.Status = "error"; u.Message = err.Error() })
		return err
	}
	// 清空旧的（同一版本多次下载场景）
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || p == dir {
			return nil
		}
		return os.RemoveAll(p)
	})

	zipPath := filepath.Join(dir, "update.zip")
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "codex-ui/"+version)
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		upd.set(func(u *updateState) { u.Status = "error"; u.Message = "下载失败：" + err.Error() })
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		upd.set(func(u *updateState) { u.Status = "error"; u.Message = fmt.Sprintf("下载失败：HTTP %d", resp.StatusCode) })
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	out, err := os.Create(zipPath)
	if err != nil {
		upd.set(func(u *updateState) { u.Status = "error"; u.Message = err.Error() })
		return err
	}
	// 边写边发 progress（防客户端等空响应）
	total := resp.ContentLength
	var written int64
	buf := make([]byte, 64*1024)
	var lastPct int = -1
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				upd.set(func(u *updateState) { u.Status = "error"; u.Message = werr.Error() })
				return werr
			}
			written += int64(n)
			if total > 0 {
				pct := int(written * 100 / total)
				if pct != lastPct && pct%5 == 0 { // 每 5% 推一次，避免 SSE 洪水
					lastPct = pct
					upd.set(func(u *updateState) { u.Progress = pct })
				}
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			out.Close()
			upd.set(func(u *updateState) { u.Status = "error"; u.Message = "下载中断：" + rerr.Error() })
			return rerr
		}
	}
	out.Close()
	upd.set(func(u *updateState) { u.Progress = 100 })

	// 解压
	extractDir := filepath.Join(dir, "extracted")
	_ = os.RemoveAll(extractDir)
	if err := unzipTo(zipPath, extractDir); err != nil {
		upd.set(func(u *updateState) { u.Status = "error"; u.Message = "解压失败：" + err.Error() })
		return err
	}
	// 找 .app 路径
	var appPath string
	_ = filepath.Walk(extractDir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || appPath != "" {
			return nil
		}
		if fi.IsDir() && strings.HasSuffix(fi.Name(), ".app") {
			appPath = p
			return filepath.SkipDir
		}
		return nil
	})
	if appPath == "" {
		upd.set(func(u *updateState) { u.Status = "error"; u.Message = "zip 中未找到 .app" })
		return fmt.Errorf("no .app")
	}
	// 清掉下载隔离属性，避免 Gatekeeper 拦截
	_ = exec.Command("xattr", "-cr", appPath).Run()

	upd.set(func(u *updateState) {
		u.Status = "ready"
		u.ReadyApp = appPath
		u.Latest = latest
	})
	return nil
}

// unzipTo 解压 zip 到 dest（支持 macOS app bundle 的符号链接 / 权限位）
func unzipTo(zipPath, dest string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := os.MkdirAll(dest, 0755); err != nil {
		return err
	}
	for _, f := range r.File {
		// 跳过 macOS 元数据
		if strings.HasPrefix(filepath.Base(f.Name), "._") || strings.Contains(f.Name, "__MACOSX") {
			continue
		}
		// 防 zip-slip
		fp := filepath.Join(dest, f.Name)
		if !strings.HasPrefix(fp, filepath.Clean(dest)+string(os.PathSeparator)) && fp != dest {
			return fmt.Errorf("非法路径：%s", f.Name)
		}
		mode := f.Mode()
		if mode&os.ModeSymlink != 0 {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			b, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return err
			}
			_ = os.MkdirAll(filepath.Dir(fp), 0755)
			_ = os.Remove(fp)
			if err := os.Symlink(string(b), fp); err != nil {
				return err
			}
			continue
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(fp, mode.Perm()); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(fp), 0755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(fp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode.Perm())
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// applyUpdate spawn 助手脚本然后退出当前进程，让脚本完成 mv/open。
func applyUpdate() error {
	upd.mu.Lock()
	src := upd.ReadyApp
	upd.mu.Unlock()
	if src == "" {
		return fmt.Errorf("尚未下载就绪")
	}
	// 当前正在运行的 app 路径：可执行文件在 .app/Contents/MacOS/CodexViewer
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// 找 .app 根目录（向上找含 .app 后缀的祖先）
	dst := exe
	for dst != "/" && !strings.HasSuffix(dst, ".app") {
		dst = filepath.Dir(dst)
	}
	if !strings.HasSuffix(dst, ".app") {
		// 不在 .app bundle 里（go run / debug 模式）— 拒绝原地替换
		return fmt.Errorf("当前不在 .app bundle 中运行，无法自动替换")
	}
	pid := os.Getpid()

	// 写一次性脚本到 updates 目录，由 Go spawn 后立刻退出
	dir, err := updateDir()
	if err != nil {
		return err
	}
	script := filepath.Join(dir, "apply.sh")
	body := fmt.Sprintf(`#!/usr/bin/env bash
# 等待父进程退出，把新版 .app 原地搬上去，重新打开
set -e
PARENT_PID=%d
NEW_APP=%q
DST_APP=%q

# 等待最多 10 秒让父进程退出
for i in $(seq 1 100); do
  if ! kill -0 "$PARENT_PID" 2>/dev/null; then break; fi
  sleep 0.1
done

# 替换：先移到 trash 名字，再 mv 新的过去（避免读写中断）
TRASH_DIR=$(dirname "$DST_APP")/.codex-viewer-old-$$
mv "$DST_APP" "$TRASH_DIR" 2>/dev/null || true
mv "$NEW_APP" "$DST_APP"
xattr -cr "$DST_APP" 2>/dev/null || true
rm -rf "$TRASH_DIR" 2>/dev/null || true

# 启动新 app
open "$DST_APP"
`, pid, src, dst)
	if err := os.WriteFile(script, []byte(body), 0755); err != nil {
		return err
	}
	cmd := exec.Command("/bin/bash", script)
	// 完全脱离：让脚本在我们退出后继续跑
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return err
	}
	// 不等待，让父进程退出后脚本接管
	go func() {
		time.Sleep(300 * time.Millisecond) // 给前端 SSE 一点时间送达
		os.Exit(0)
	}()
	return nil
}

// ── HTTP Handlers ──

func versionHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"version": version, "state": upd.snapshot()})
}

func updateCheckHandler(w http.ResponseWriter, r *http.Request) {
	if version == "dev" {
		writeJSON(w, map[string]any{"ok": false, "error": "dev 构建跳过更新检查"})
		return
	}
	go checkUpdate()
	writeJSON(w, map[string]any{"ok": true})
}

func updateDownloadHandler(w http.ResponseWriter, r *http.Request) {
	upd.mu.Lock()
	st := upd.Status
	upd.mu.Unlock()
	if st != "available" && st != "error" {
		writeJSON(w, map[string]any{"ok": false, "error": "当前状态：" + st + "，不能下载"})
		return
	}
	go func() { _ = downloadUpdate() }()
	writeJSON(w, map[string]any{"ok": true})
}

func updateApplyHandler(w http.ResponseWriter, r *http.Request) {
	if err := applyUpdate(); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
