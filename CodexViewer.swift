// CodexViewer.swift — Codex Viewer 的原生 macOS 外壳。
// 真实 NSWindow（系统红绿灯 / Dock 图标 / Cmd+Q），内容由 WKWebView 承载，
// 内置的 Go 服务作为子进程提供数据与界面。
import Cocoa
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

final class AppDelegate: NSObject, NSApplicationDelegate, WKNavigationDelegate, WKUIDelegate {
    var window: NSWindow!
    var webView: WKWebView!
    let bg = NSColor(red: 0.027, green: 0.027, blue: 0.031, alpha: 1)

    func applicationDidFinishLaunching(_ note: Notification) {
        startServer()
        let port = waitForPort()

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

        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    @objc func reload() { webView.reload() }

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
        let chrome = "/Applications/Google Chrome.app"
        if FileManager.default.fileExists(atPath: chrome) {
            NSWorkspace.shared.open([url], withApplicationAt: URL(fileURLWithPath: chrome),
                                    configuration: NSWorkspace.OpenConfiguration()) { _, err in
                if err != nil { NSWorkspace.shared.open(url) }
            }
        } else {
            NSWorkspace.shared.open(url)
        }
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ s: NSApplication) -> Bool { true }

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
