// CodexViewer.swift — Codex Viewer 的原生 macOS 外壳。
// 真实 NSWindow（系统红绿灯 / Dock 图标 / Cmd+Q），内容由 WKWebView 承载，
// 内置的 Go 服务作为子进程提供数据与界面。
import Cocoa
import ServiceManagement
import WebKit

// 端口文件：Go 服务启动后写入实际端口，供本程序读取。
let portFile = NSTemporaryDirectory() + "codex-viewer-\(getpid()).port"
var serverProcess: Process?

func startServer() {
    let res = Bundle.main.resourcePath ?? "."
    let proc = Process()
    proc.executableURL = URL(fileURLWithPath: res + "/codex-ui")
    var env = ProcessInfo.processInfo.environment
    env["NO_OPEN"] = "1"
    env["CODEXUI_PORTFILE"] = portFile
    proc.environment = env
    try? proc.run()
    serverProcess = proc
}

// 轮询端口文件，最多等待约 15 秒。
func waitForPort() -> Int {
    for _ in 0..<150 {
        if let s = try? String(contentsOfFile: portFile, encoding: .utf8),
           let n = Int(s.trimmingCharacters(in: .whitespacesAndNewlines)) {
            return n
        }
        Thread.sleep(forTimeInterval: 0.1)
    }
    return 7800
}

// 透明原生视图：承载 macOS 真实窗口拖动与双击标题栏行为。
// （WKWebView 不支持 CSS -webkit-app-region，故拖动必须由原生层实现。）
final class DragView: NSView {
    override var mouseDownCanMoveWindow: Bool { false }
    override func mouseDown(with event: NSEvent) {
        if event.clickCount == 2 {
            // 双击标题栏：遵循系统偏好设置
            switch UserDefaults.standard.string(forKey: "AppleActionOnDoubleClick") {
            case "Minimize": window?.miniaturize(nil)
            case "None":     break
            default:         window?.zoom(nil)
            }
            return
        }
        window?.performDrag(with: event)   // 系统级窗口拖动
    }
}

// /api/issues 返回的单条 issue（仅取菜单栏需要的字段）。
struct IssueLabel: Decodable {
    let name: String
    let color: String
}
struct IssueItem: Decodable {
    let repo: String
    let number: Int
    let title: String
    let url: String
    let labels: [IssueLabel]?
}
struct IssuesResponse: Decodable {
    let issues: [IssueItem]?
    let error: String?
    let updated: Int64?
    let menuMax: Int?
    let showInMenu: Bool?
}

// /api/config 的子集 —— 仅菜单栏外壳关心的部分。
struct GeneralCfg: Decodable {
    let browser: String?
    let browserPath: String?
}
struct StartupCfg: Decodable {
    let launchAtLogin: Bool?
    let openWindowOnLaunch: Bool?
    let onWindowClose: String?
}
struct AppConfig: Decodable {
    let general: GeneralCfg?
    let startup: StartupCfg?
}

extension NSColor {
    // 从 GitHub 标签的 6 位十六进制色（无 #）构造颜色。
    convenience init?(hex: String) {
        let s = hex.trimmingCharacters(in: CharacterSet(charactersIn: "# "))
        guard s.count == 6, let v = Int(s, radix: 16) else { return nil }
        self.init(srgbRed: CGFloat((v >> 16) & 0xff) / 255,
                  green: CGFloat((v >> 8) & 0xff) / 255,
                  blue: CGFloat(v & 0xff) / 255, alpha: 1)
    }
}

// 菜单里的单条 issue 行：左侧标题（点击打开 GitHub 页面），
// 右侧「详情」按钮（点击在 iTerm2 里跑 /issue info）。
final class IssueRowView: NSView {
    private let issue: IssueItem
    private weak var owner: AppDelegate?

    init(issue: IssueItem, width: CGFloat, owner: AppDelegate) {
        self.issue = issue
        self.owner = owner
        super.init(frame: NSRect(x: 0, y: 0, width: width, height: 26))
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) 未实现") }

    // 「详情」按钮的矩形区域（右对齐）。
    private var detailRect: NSRect {
        NSRect(x: bounds.width - 14 - 48, y: (bounds.height - 17) / 2,
               width: 48, height: 17)
    }
    private var isHot: Bool { enclosingMenuItem?.isHighlighted ?? false }

    // 菜单高亮随指针移动变化，进出时刷新重绘。
    override func updateTrackingAreas() {
        super.updateTrackingAreas()
        trackingAreas.forEach(removeTrackingArea)
        addTrackingArea(NSTrackingArea(
            rect: bounds, options: [.mouseEnteredAndExited, .activeAlways, .inVisibleRect],
            owner: self, userInfo: nil))
    }
    override func mouseEntered(with event: NSEvent) { needsDisplay = true }
    override func mouseExited(with event: NSEvent) { needsDisplay = true }

    override func draw(_ dirtyRect: NSRect) {
        let hot = isHot
        if hot {
            NSColor.selectedContentBackgroundColor.setFill()
            NSBezierPath(roundedRect: bounds.insetBy(dx: 5, dy: 1),
                         xRadius: 5, yRadius: 5).fill()
        }
        // 状态点
        (owner?.labelColor(issue.labels ?? []) ?? .gray).setFill()
        NSBezierPath(ovalIn: NSRect(x: 15, y: (bounds.height - 8) / 2,
                                    width: 8, height: 8)).fill()
        // 标题
        if let title = owner?.issueMenuTitle(issue, highlighted: hot) {
            title.draw(in: NSRect(x: 32, y: (bounds.height - 17) / 2,
                                  width: detailRect.minX - 32 - 10, height: 17))
        }
        // 「详情」按钮
        let d = detailRect
        (hot ? NSColor.white.withAlphaComponent(0.28)
             : NSColor.white.withAlphaComponent(0.09)).setFill()
        NSBezierPath(roundedRect: d, xRadius: 4, yRadius: 4).fill()
        let label = NSAttributedString(string: "详情", attributes: [
            .font: NSFont.systemFont(ofSize: 11, weight: .medium),
            .foregroundColor: hot ? NSColor.white
                                  : (NSColor(hex: "5B9DFF") ?? .systemBlue)])
        let sz = label.size()
        label.draw(at: NSPoint(x: d.midX - sz.width / 2, y: d.midY - sz.height / 2))
    }

    override func mouseDown(with event: NSEvent) {}

    override func mouseUp(with event: NSEvent) {
        let p = convert(event.locationInWindow, from: nil)
        let onDetail = detailRect.insetBy(dx: -6, dy: -4).contains(p)
        enclosingMenuItem?.menu?.cancelTracking()
        let it = issue
        DispatchQueue.main.async { [weak owner] in
            if onDetail { owner?.runIssueDetail(it) }
            else { owner?.openIssueLink(it.url) }
        }
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate, WKNavigationDelegate, WKUIDelegate,
                         NSMenuDelegate, NSWindowDelegate {
    var window: NSWindow!
    var webView: WKWebView!
    var statusItem: NSStatusItem!
    var port = 7800
    let bg = NSColor(red: 0.027, green: 0.027, blue: 0.031, alpha: 1)

    func applicationDidFinishLaunching(_ note: Notification) {
        startServer()
        let port = waitForPort()
        self.port = port

        window = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 1480, height: 940),
            styleMask: [.titled, .closable, .miniaturizable, .resizable, .fullSizeContentView],
            backing: .buffered, defer: false)
        window.titlebarAppearsTransparent = true   // 内容延伸到标题栏，红绿灯浮于界面上
        window.titleVisibility = .hidden
        window.title = "Codex Viewer"
        window.appearance = NSAppearance(named: .darkAqua)
        window.backgroundColor = bg
        window.minSize = NSSize(width: 960, height: 620)
        window.center()
        window.setFrameAutosaveName("CodexViewerWindow")  // 记住窗口位置/尺寸
        window.isReleasedWhenClosed = false               // 关闭仅隐藏，可从菜单栏重开
        window.delegate = self

        let cv = window.contentView!
        let cfg = WKWebViewConfiguration()
        cfg.websiteDataStore = .nonPersistent()
        webView = WKWebView(frame: cv.bounds, configuration: cfg)
        webView.autoresizingMask = [.width, .height]
        webView.navigationDelegate = self
        webView.uiDelegate = self
        webView.setValue(false, forKey: "drawsBackground")  // 避免加载时白屏闪烁
        if #available(macOS 12.0, *) { webView.underPageBackgroundColor = bg }
        cv.addSubview(webView)

        // 透明拖拽区覆盖顶部 52px 标题栏；系统红绿灯浮于其上，仍可点击。
        let titlebarH: CGFloat = 52
        let drag = DragView(frame: NSRect(x: 0, y: cv.bounds.height - titlebarH,
                                          width: cv.bounds.width, height: titlebarH))
        drag.autoresizingMask = [.width, .minYMargin]
        cv.addSubview(drag)

        let url = URL(string: "http://127.0.0.1:\(port)/?app=1")!
        webView.load(URLRequest(url: url))

        // 读取启动配置：决定是否开窗、是否登录自启。
        let appCfg = fetchConfig()
        applyStartupConfig(appCfg?.startup)
        if appCfg?.startup?.openWindowOnLaunch ?? true {
            window.makeKeyAndOrderFront(nil)
            NSApp.activate(ignoringOtherApps: true)
        } else {
            NSApp.setActivationPolicy(.accessory)  // 仅驻留菜单栏后台
        }

        setupStatusItem()
    }

    @objc func reload() { webView.reload() }

    // ── 菜单栏 Issue 菜单 ───────────────────────────────────────

    func setupStatusItem() {
        statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        if let btn = statusItem.button {
            if let img = NSImage(systemSymbolName: "smallcircle.filled.circle",
                                 accessibilityDescription: "我的 Issue") {
                img.isTemplate = true
                btn.image = img
            } else {
                btn.title = "◆"
            }
            btn.toolTip = "Codex Viewer — 我的 Issue"
        }
        let menu = NSMenu()
        menu.delegate = self
        statusItem.menu = menu
    }

    // 菜单即将显示前重建：拉取最新 issue 列表（读后端缓存）。
    func menuNeedsUpdate(_ menu: NSMenu) {
        menu.removeAllItems()
        // 顺带按配置同步「开机自启动」（异步，不拖慢菜单弹出）。
        DispatchQueue.global().async { [weak self] in
            let s = self?.fetchConfig()?.startup
            DispatchQueue.main.async { self?.applyStartupConfig(s) }
        }
        let resp = fetchIssues()
        if resp == nil {
            addInfoItem(menu, "无法连接到服务")
        } else if !(resp?.showInMenu ?? true) {
            addInfoItem(menu, "issue 菜单显示已关闭 —— 可在设置开启")
        } else {
            if let ms = resp?.updated {
                addInfoItem(menu, relTime(ms))
                menu.addItem(.separator())
            }
            let list = resp?.issues ?? []
            if list.isEmpty {
                let err = resp?.error ?? ""
                addInfoItem(menu, err.isEmpty ? "暂无分配给你的 issue —— 点设置添加仓库"
                                              : "issue 拉取失败 —— 点设置检查")
            } else {
                for it in list.prefix(resp?.menuMax ?? 20) {
                    let item = NSMenuItem()
                    item.view = IssueRowView(issue: it, width: 470, owner: self)
                    menu.addItem(item)
                }
            }
        }
        menu.addItem(.separator())
        let refresh = NSMenuItem(title: "刷新 Issue", action: #selector(refreshIssuesNow),
                                 keyEquivalent: "")
        refresh.target = self
        if let img = NSImage(systemSymbolName: "arrow.clockwise", accessibilityDescription: nil) {
            refresh.image = img
        }
        menu.addItem(refresh)
        let settings = NSMenuItem(title: "设置…", action: #selector(openSettings),
                                  keyEquivalent: ",")
        settings.target = self
        menu.addItem(settings)
        let open = NSMenuItem(title: "打开 Codex Viewer",
                              action: #selector(openMainWindow), keyEquivalent: "")
        open.target = self
        menu.addItem(open)
        menu.addItem(NSMenuItem(title: "退出 Codex Viewer",
                                action: #selector(NSApplication.terminate(_:)), keyEquivalent: ""))
    }

    func addInfoItem(_ menu: NSMenu, _ text: String) {
        let item = NSMenuItem(title: text, action: nil, keyEquivalent: "")
        item.isEnabled = false
        menu.addItem(item)
    }

    // 点 issue 标题 → 浏览器打开 issue 链接。
    func openIssueLink(_ s: String) {
        if let url = URL(string: s) { openExternal(url) }
    }

    // 点「详情」→ 让 Go 后端在该仓库的本地映射目录用 iTerm2 跑 /issue info。
    func runIssueDetail(_ issue: IssueItem) {
        let repo = issue.repo.addingPercentEncoding(
            withAllowedCharacters: .alphanumerics) ?? issue.repo
        guard let url = URL(string:
            "http://127.0.0.1:\(port)/api/issue-run?repo=\(repo)&number=\(issue.number)")
        else { return }
        var req = URLRequest(url: url)
        req.timeoutInterval = 6
        URLSession.shared.dataTask(with: req) { data, _, _ in
            var ok = false
            var errMsg = "启动失败"
            if let data = data,
               let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any] {
                ok = obj["ok"] as? Bool ?? false
                if let e = obj["error"] as? String, !e.isEmpty { errMsg = e }
            }
            if !ok {
                DispatchQueue.main.async {
                    let a = NSAlert()
                    a.alertStyle = .warning
                    a.messageText = "无法打开 issue 详情"
                    a.informativeText = errMsg
                    a.runModal()
                }
            }
        }.resume()
    }

    // 「设置…」→ 唤起主窗口并打开设置页。
    @objc func openSettings() {
        showMainWindow()
        webView.evaluateJavaScript("window.openSettings && window.openSettings()",
                                   completionHandler: nil)
    }

    // 「刷新 Issue」→ 强制后端重新拉取，完成后重新弹出菜单显示新数据。
    @objc func refreshIssuesNow() {
        DispatchQueue.global().async {
            if let url = URL(string: "http://127.0.0.1:\(self.port)/api/issues?refresh=1") {
                var req = URLRequest(url: url)
                req.timeoutInterval = 30
                let sem = DispatchSemaphore(value: 0)
                URLSession.shared.dataTask(with: req) { _, _, _ in sem.signal() }.resume()
                _ = sem.wait(timeout: .now() + 30)
            }
            DispatchQueue.main.async {
                self.statusItem.button?.performClick(nil) // 重新弹出菜单
            }
        }
    }

    // 把毫秒时间戳格式化成「更新于 X 前」。
    func relTime(_ ms: Int64) -> String {
        if ms <= 0 { return "尚未更新" }
        let secs = Int(Date().timeIntervalSince1970) - Int(ms / 1000)
        if secs < 60 { return "刚刚更新" }
        if secs < 3600 { return "更新于 \(secs / 60) 分钟前" }
        if secs < 86400 { return "更新于 \(secs / 3600) 小时前" }
        return "更新于 \(secs / 86400) 天前"
    }

    @objc func openMainWindow() { showMainWindow() }

    func showMainWindow() {
        NSApp.setActivationPolicy(.regular)               // 恢复 Dock 图标与 App 菜单
        if window.isMiniaturized { window.deminiaturize(nil) }
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    // 关闭窗口：按配置决定退出 App 或切到「菜单栏后台」。
    func windowWillClose(_ notification: Notification) {
        if fetchConfig()?.startup?.onWindowClose == "quit" {
            DispatchQueue.main.async { NSApp.terminate(nil) }
            return
        }
        DispatchQueue.main.async { NSApp.setActivationPolicy(.accessory) }
    }

    // 同步拉取 /api/config 的外壳子集（读 Go 后端，毫秒级）。
    func fetchConfig() -> AppConfig? {
        guard let url = URL(string: "http://127.0.0.1:\(port)/api/config") else { return nil }
        var req = URLRequest(url: url)
        req.timeoutInterval = 2
        var result: AppConfig? = nil
        let sem = DispatchSemaphore(value: 0)
        URLSession.shared.dataTask(with: req) { data, _, _ in
            defer { sem.signal() }
            if let data = data {
                result = try? JSONDecoder().decode(AppConfig.self, from: data)
            }
        }.resume()
        _ = sem.wait(timeout: .now() + 3)
        return result
    }

    // 按配置同步「开机自启动」登录项（macOS 13+）。
    func applyStartupConfig(_ s: StartupCfg?) {
        guard let s = s, #available(macOS 13.0, *) else { return }
        let want = s.launchAtLogin ?? false
        let svc = SMAppService.mainApp
        let isOn = svc.status == .enabled
        if want && !isOn {
            try? svc.register()
        } else if !want && isOn {
            try? svc.unregister()
        }
    }

    // 同步拉取 /api/issues（读 Go 后端缓存，毫秒级）；返回 nil 表示请求失败。
    func fetchIssues() -> IssuesResponse? {
        guard let url = URL(string: "http://127.0.0.1:\(port)/api/issues") else { return nil }
        var req = URLRequest(url: url)
        req.timeoutInterval = 2
        var result: IssuesResponse? = nil
        let sem = DispatchSemaphore(value: 0)
        URLSession.shared.dataTask(with: req) { data, _, _ in
            defer { sem.signal() }
            if let data = data {
                result = try? JSONDecoder().decode(IssuesResponse.self, from: data)
            }
        }.resume()
        _ = sem.wait(timeout: .now() + 3)
        return result
    }

    // 菜单行标题：#编号（暗）+ 仓库短名（更暗）+ issue 标题；高亮时改用亮色。
    func issueMenuTitle(_ it: IssueItem, highlighted hot: Bool) -> NSAttributedString {
        let para = NSMutableParagraphStyle()
        para.lineBreakMode = .byTruncatingTail
        let numColor   = hot ? NSColor.white.withAlphaComponent(0.92) : NSColor.secondaryLabelColor
        let repoColor  = hot ? NSColor.white.withAlphaComponent(0.62) : NSColor.tertiaryLabelColor
        let titleColor = hot ? NSColor.white : NSColor.labelColor
        let r = NSMutableAttributedString()
        r.append(NSAttributedString(string: "#\(it.number)  ", attributes: [
            .foregroundColor: numColor, .paragraphStyle: para,
            .font: NSFont.monospacedDigitSystemFont(ofSize: 11, weight: .semibold)]))
        let short = it.repo.split(separator: "/").last.map(String.init) ?? it.repo
        r.append(NSAttributedString(string: short + "  ", attributes: [
            .foregroundColor: repoColor, .paragraphStyle: para,
            .font: NSFont.monospacedSystemFont(ofSize: 9, weight: .medium)]))
        r.append(NSAttributedString(string: ellipsize(it.title, 44), attributes: [
            .foregroundColor: titleColor, .paragraphStyle: para,
            .font: NSFont.menuFont(ofSize: 13)]))
        return r
    }

    // issue 状态点：取第一个标签的颜色，无标签则灰色。
    func labelColor(_ labels: [IssueLabel]) -> NSColor {
        if let hex = labels.first?.color, let c = NSColor(hex: hex) { return c }
        return .tertiaryLabelColor
    }

    func ellipsize(_ s: String, _ n: Int) -> String {
        s.count <= n ? s : String(s.prefix(n)) + "…"
    }

    // 外部 http/https 链接：取消 WKWebView 内跳转，改用浏览器打开
    func webView(_ webView: WKWebView, decidePolicyFor navigationAction: WKNavigationAction,
                 decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        if let url = navigationAction.request.url, isExternal(url) {
            decisionHandler(.cancel)
            openExternal(url)
            return
        }
        decisionHandler(.allow)
    }

    // target="_blank" / window.open 也走外部打开
    func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration,
                 for navigationAction: WKNavigationAction,
                 windowFeatures: WKWindowFeatures) -> WKWebView? {
        if let url = navigationAction.request.url { openExternal(url) }
        return nil
    }

    private func isExternal(_ url: URL) -> Bool {
        let scheme = url.scheme?.lowercased() ?? ""
        guard scheme == "http" || scheme == "https" else { return false }
        let host = url.host ?? ""
        return host != "127.0.0.1" && host != "localhost"
    }

    // 优先 Google Chrome，未安装则回退系统默认浏览器
    private func openExternal(_ url: URL) {
        let g = fetchConfig()?.general
        var appPath: String?
        switch g?.browser ?? "chrome" {
        case "safari":
            appPath = "/Applications/Safari.app"
        case "default":
            appPath = nil
        case "custom":
            let p = g?.browserPath ?? ""
            appPath = p.isEmpty ? nil : p
        default:
            appPath = "/Applications/Google Chrome.app"
        }
        if let ap = appPath, FileManager.default.fileExists(atPath: ap) {
            NSWorkspace.shared.open([url], withApplicationAt: URL(fileURLWithPath: ap),
                                    configuration: NSWorkspace.OpenConfiguration()) { _, err in
                if err != nil { NSWorkspace.shared.open(url) }
            }
        } else {
            NSWorkspace.shared.open(url)
        }
    }

    // 关闭窗口不退出 App —— 菜单栏图标常驻，可随时重新打开。
    func applicationShouldTerminateAfterLastWindowClosed(_ s: NSApplication) -> Bool { false }

    // 点 Dock 图标（窗口已关闭时）重新打开主窗口。
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        if !flag { showMainWindow() }
        return true
    }

    func applicationWillTerminate(_ note: Notification) {
        serverProcess?.terminate()
        try? FileManager.default.removeItem(atPath: portFile)
    }
}

// 标准菜单：让 Cmd+Q / Cmd+W / 复制粘贴 / Cmd+R 正常工作。
func buildMenu(_ delegate: AppDelegate) {
    let main = NSMenu()

    let appItem = NSMenuItem(); main.addItem(appItem)
    let appMenu = NSMenu()
    appMenu.addItem(withTitle: "关于 Codex Viewer",
                    action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)), keyEquivalent: "")
    appMenu.addItem(.separator())
    appMenu.addItem(withTitle: "隐藏", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
    appMenu.addItem(withTitle: "退出 Codex Viewer",
                    action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
    appItem.submenu = appMenu

    let editItem = NSMenuItem(); main.addItem(editItem)
    let editMenu = NSMenu(title: "编辑")
    editMenu.addItem(withTitle: "撤销", action: Selector(("undo:")), keyEquivalent: "z")
    editMenu.addItem(withTitle: "重做", action: Selector(("redo:")), keyEquivalent: "Z")
    editMenu.addItem(.separator())
    editMenu.addItem(withTitle: "剪切", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
    editMenu.addItem(withTitle: "拷贝", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
    editMenu.addItem(withTitle: "粘贴", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
    editMenu.addItem(withTitle: "全选", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
    editItem.submenu = editMenu

    let viewItem = NSMenuItem(); main.addItem(viewItem)
    let viewMenu = NSMenu(title: "显示")
    let reload = NSMenuItem(title: "重新载入", action: #selector(AppDelegate.reload), keyEquivalent: "r")
    reload.target = delegate
    viewMenu.addItem(reload)
    viewItem.submenu = viewMenu

    let winItem = NSMenuItem(); main.addItem(winItem)
    let winMenu = NSMenu(title: "窗口")
    winMenu.addItem(withTitle: "最小化",
                    action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m")
    winMenu.addItem(withTitle: "缩放", action: #selector(NSWindow.performZoom(_:)), keyEquivalent: "")
    winItem.submenu = winMenu
    NSApp.windowsMenu = winMenu

    NSApp.mainMenu = main
}

let app = NSApplication.shared
app.setActivationPolicy(.regular)
let delegate = AppDelegate()
app.delegate = delegate
buildMenu(delegate)
app.run()
