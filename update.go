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
	"syscall"
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
		"checkedAt":  u.CheckedAt,
		"current":    version,
		"currentUrl": currentTagURL(),
	}
}

// currentTagURL 返回「当前运行版本」的 GitHub release tag 页面链接。
// 注意要和 latest release 的 url 区分：前者跟着 current 版本走，后者永远是最新版。
// dev 构建没有对应 tag，返回空串。
func currentTagURL() string {
	if version == "dev" || version == "" {
		return ""
	}
	tag := version
	// current 可能形如 "v0.1.7-3-gabcdef-dirty"，tag 取第一个 "-" 之前
	if i := strings.Index(tag, "-"); i >= 0 {
		tag = tag[:i]
	}
	return fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s", githubOwner, githubRepo, tag)
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

// appBundlePath 从当前可执行文件路径向上找含 .app 后缀的祖先目录。
// 非 .app 运行（go run / dev）时返回空串。
func appBundlePath() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	dst := exe
	for dst != "/" && !strings.HasSuffix(dst, ".app") {
		dst = filepath.Dir(dst)
	}
	if strings.HasSuffix(dst, ".app") {
		return dst
	}
	return ""
}

// applyUpdate 退出当前 App 并用下载好的新版替换后重启。
func applyUpdate() error {
	upd.mu.Lock()
	src := upd.ReadyApp
	upd.mu.Unlock()
	if src == "" {
		return fmt.Errorf("尚未下载就绪")
	}
	dst := appBundlePath()
	if dst == "" {
		return fmt.Errorf("当前不在 .app bundle 中运行，无法自动替换")
	}
	return spawnRelaunch(src, dst)
}

// restartApp 不做替换，纯粹退出当前 App 再重启（供「重启」按钮用）。
func restartApp() error {
	dst := appBundlePath()
	if dst == "" {
		return fmt.Errorf("当前不在 .app bundle 中运行，无法重启")
	}
	return spawnRelaunch("", dst)
}

// spawnRelaunch 写出并以「独立 session」spawn 一个一次性 bash 助手，
// 然后让本 Go 进程退出。助手负责：等 Go 退出 → 优雅退 Swift 外壳 →
// （可选）原地替换 .app → 重启。newApp 为空表示纯重启不替换。
//
// 进程结构：Swift App（NSApplication 持 .app）→ 内嵌 Go binary（本进程）。
// 真正占住 .app bundle 的是 Swift App，所以必须连 Swift 一起退。
//
// 关键：助手必须脱离 Swift 的进程组/会话，否则 Swift 退出时会被一起带走。
// macOS 没有 setsid 命令，过去用 `env setsid` 会静默失败（env 启动成功但
// 找不到 setsid 立即退 127，bash 根本没跑）——这是更新/重启一直不生效的根因。
// 现改用 Go 原生 SysProcAttr{Setsid:true}，由内核在 fork 后建新会话。
func spawnRelaunch(newApp, dst string) error {
	swiftPID := os.Getppid() // Swift App 的 PID — 真正的"父进程"
	goPID := os.Getpid()

	dir, err := updateDir()
	if err != nil {
		return err
	}
	script := filepath.Join(dir, "apply.sh")
	body := fmt.Sprintf(`#!/bin/bash
# Codex Viewer 重启/更新助手 —— 在 Swift App 退出后（可选替换）并重启
# 由 Go 后端以独立 session spawn，独立于 Swift 进程组
set -u

GO_PID=%d
SWIFT_PID=%d
NEW_APP=%q
DST_APP=%q
LOG=%q

exec >>"$LOG" 2>&1
echo "=== $(date '+%%F %%T') 助手启动（NEW_APP='$NEW_APP'）==="
echo "Go PID=$GO_PID  Swift PID=$SWIFT_PID"

# 1) Go 子进程会自己 os.Exit；等它消失（最多 5s）
for i in $(seq 1 50); do
  kill -0 "$GO_PID" 2>/dev/null || break
  sleep 0.1
done

# 2) 让 Swift App 优雅退出（触发 applicationWillTerminate，清理 portFile 等）
#    用 bundle id 比 display name 稳定
osascript -e 'tell application id "com.local.codexviewer" to quit' 2>/dev/null || true

# 3) 等 Swift 真死（最多 10 秒），死透前不能动 .app bundle
for i in $(seq 1 100); do
  kill -0 "$SWIFT_PID" 2>/dev/null || break
  sleep 0.1
done
# 顽固：强杀
if kill -0 "$SWIFT_PID" 2>/dev/null; then
  echo "Swift 没在 10s 内退，强杀"
  kill -KILL "$SWIFT_PID" 2>/dev/null || true
  sleep 0.5
fi

# 4) 仅当有新版时才替换；纯重启跳过这一段
if [ -n "$NEW_APP" ]; then
  TRASH_DIR="$(dirname "$DST_APP")/.codex-viewer-old-$$"
  if ! mv "$DST_APP" "$TRASH_DIR" 2>/dev/null; then
    # 可能是权限不足；退路：rm -rf
    rm -rf "$DST_APP" 2>/dev/null || true
  fi
  if ! mv "$NEW_APP" "$DST_APP" 2>/dev/null; then
    # 跨卷 mv 会失败，退路用 ditto 复制（保留 bundle 元数据）
    if ! ditto "$NEW_APP" "$DST_APP" 2>/dev/null; then
      echo "✗ 替换失败：$NEW_APP → $DST_APP"
      # 还原旧 app（如果还在 trash 里）
      [ -d "$TRASH_DIR" ] && mv "$TRASH_DIR" "$DST_APP" 2>/dev/null || true
      open "$DST_APP" 2>/dev/null || true
      exit 1
    fi
  fi
  xattr -cr "$DST_APP" 2>/dev/null || true
  rm -rf "$TRASH_DIR" 2>/dev/null || true
  # 刷新 Launch Services 缓存（避免 Finder 还认旧版本签名）
  /System/Library/Frameworks/CoreServices.framework/Versions/A/Frameworks/LaunchServices.framework/Versions/A/Support/lsregister -f "$DST_APP" 2>/dev/null || true
fi

# 5) 起新 app（用 -n 确保拉起全新实例）
open -n "$DST_APP"
echo "=== $(date '+%%F %%T') 助手完成 ==="
`, goPID, swiftPID, newApp, dst, filepath.Join(dir, "apply.log"))
	if err := os.WriteFile(script, []byte(body), 0755); err != nil {
		return err
	}
	// 以独立 session spawn，脱离 Swift 进程组；std* 全部置空避免持有句柄。
	cmd := exec.Command("/bin/bash", script)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// 给前端 SSE 一点时间送达，再退；Swift 后续由 osascript 收到 AppleEvent 退出
	go func() {
		time.Sleep(500 * time.Millisecond)
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

// restartHandler 退出当前 App 并重启（不做版本替换）。
func restartHandler(w http.ResponseWriter, r *http.Request) {
	if err := restartApp(); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
