// CodexViewer.swift — Codex Viewer 的原生 macOS 外壳。
// 真实 NSWindow（系统红绿灯 / Dock 图标 / Cmd+Q），内容由 WKWebView 承载，
// 内置的 Go 服务作为子进程提供数据与界面。
import Cocoa
import ServiceManagement
import UserNotifications
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

// 菜单栏的三个标签页。
enum MenuTab: Int { case issues = 0, prs = 1, sessions = 2 }

// /api/menubar 返回的单条 issue。
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
    let comments: Int?
}
// /api/menubar 返回的单条 pull request。
struct PRItem: Decodable {
    let repo: String
    let number: Int
    let title: String
    let url: String
    let labels: [IssueLabel]?
    let author: String?
    let isDraft: Bool?
    let reason: String?   // author（我创建）| review（待我 review）
    let comments: Int?
}
// /api/menubar 返回的单条活跃会话。
struct SessionItem: Decodable {
    let id: String
    let source: String
    let title: String
    let project: String?
    let mtime: Int64
}
// /api/menubar 的完整响应 —— 三个标签页所需的全部数据。
struct MenubarResponse: Decodable {
    let showInMenu: Bool?
    let menuMax: Int?
    let issues: [IssueItem]?
    let issuesUpdated: Int64?
    let issuesError: String?
    let prs: [PRItem]?
    let prsUpdated: Int64?
    let prsError: String?
    let sessions: [SessionItem]?
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
    let notifyOnNewItems: Bool?
}

// 一条待发通知的素材（issue 或 PR 归一化后）。
struct NotifyEntry {
    let key: String          // "i:repo#n" / "p:repo#n"，评论数基线的键
    let comments: Int
    let newTitle: String     // 作为「新条目」时的通知标题
    let commentTitle: String // 作为「新评论」时的通知标题
    let body: String
    let url: String
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

// ellipsizeStr 把过长字符串截断并加省略号。
func ellipsizeStr(_ s: String, _ n: Int) -> String {
    s.count <= n ? s : String(s.prefix(n)) + "…"
}

// 菜单行的公共基类：悬停高亮 + 状态点 + 指针进出重绘。
class HoverRowView: NSView {
    weak var owner: AppDelegate?

    init(width: CGFloat, owner: AppDelegate) {
        self.owner = owner
        super.init(frame: NSRect(x: 0, y: 0, width: width, height: 26))
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) 未实现") }

    var isHot: Bool { enclosingMenuItem?.isHighlighted ?? false }

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
    override func mouseDown(with event: NSEvent) {}

    // 绘制高亮背景；子类在 super.draw(_:) 之后绘制内容。
    override func draw(_ dirtyRect: NSRect) {
        if isHot {
            NSColor.selectedContentBackgroundColor.setFill()
            NSBezierPath(roundedRect: bounds.insetBy(dx: 5, dy: 1),
                         xRadius: 5, yRadius: 5).fill()
        }
    }
    // 在左侧画一个状态点。
    func drawDot(_ color: NSColor) {
        color.setFill()
        NSBezierPath(ovalIn: NSRect(x: 15, y: (bounds.height - 8) / 2,
                                    width: 8, height: 8)).fill()
    }
}

// 菜单里的单条 issue 行：左侧标题（点击打开 GitHub 页面），
// 右侧「详情」按钮（点击在 iTerm2 里跑 /issue info）。
final class IssueRowView: HoverRowView {
    private let issue: IssueItem

    init(issue: IssueItem, width: CGFloat, owner: AppDelegate) {
        self.issue = issue
        super.init(width: width, owner: owner)
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) 未实现") }

    // 「详情」按钮的矩形区域（右对齐）。
    private var detailRect: NSRect {
        NSRect(x: bounds.width - 14 - 48, y: (bounds.height - 17) / 2,
               width: 48, height: 17)
    }

    override func draw(_ dirtyRect: NSRect) {
        super.draw(dirtyRect)
        let hot = isHot
        drawDot(owner?.labelColor(issue.labels ?? []) ?? .gray)
        if let title = owner?.rowTitle(number: issue.number, repo: issue.repo,
                                       text: issue.title, highlighted: hot) {
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

// 菜单里的单条 PR 行：左侧标题，右侧来源标签（作者 / 待审）。点击打开 GitHub 页面。
final class PRRowView: HoverRowView {
    private let pr: PRItem

    init(pr: PRItem, width: CGFloat, owner: AppDelegate) {
        self.pr = pr
        super.init(width: width, owner: owner)
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) 未实现") }

    private var tagText: String { pr.reason == "review" ? "待审" : "作者" }
    private var tagColor: NSColor {
        pr.reason == "review" ? (NSColor(hex: "E8A33D") ?? .systemOrange)
                              : (NSColor(hex: "5B9DFF") ?? .systemBlue)
    }
    private var tagRect: NSRect {
        NSRect(x: bounds.width - 14 - 40, y: (bounds.height - 17) / 2, width: 40, height: 17)
    }

    override func draw(_ dirtyRect: NSRect) {
        super.draw(dirtyRect)
        let hot = isHot
        drawDot(owner?.labelColor(pr.labels ?? []) ?? .gray)
        let text = pr.isDraft == true ? "[草稿] " + pr.title : pr.title
        if let title = owner?.rowTitle(number: pr.number, repo: pr.repo,
                                       text: text, highlighted: hot) {
            title.draw(in: NSRect(x: 32, y: (bounds.height - 17) / 2,
                                  width: tagRect.minX - 32 - 10, height: 17))
        }
        // 来源标签
        let t = tagRect
        (hot ? NSColor.white.withAlphaComponent(0.16)
             : NSColor.white.withAlphaComponent(0.07)).setFill()
        NSBezierPath(roundedRect: t, xRadius: 4, yRadius: 4).fill()
        let label = NSAttributedString(string: tagText, attributes: [
            .font: NSFont.systemFont(ofSize: 10, weight: .medium),
            .foregroundColor: hot ? NSColor.white : tagColor])
        let sz = label.size()
        label.draw(at: NSPoint(x: t.midX - sz.width / 2, y: t.midY - sz.height / 2))
    }

    override func mouseUp(with event: NSEvent) {
        enclosingMenuItem?.menu?.cancelTracking()
        let url = pr.url
        DispatchQueue.main.async { [weak owner] in owner?.openIssueLink(url) }
    }
}

// 菜单里的单条活跃会话行：来源圆点 + 标题 + 项目名。点击唤起主窗口并定位。
final class SessionRowView: HoverRowView {
    private let session: SessionItem

    init(session: SessionItem, width: CGFloat, owner: AppDelegate) {
        self.session = session
        super.init(width: width, owner: owner)
    }
    required init?(coder: NSCoder) { fatalError("init(coder:) 未实现") }

    override func draw(_ dirtyRect: NSRect) {
        super.draw(dirtyRect)
        let hot = isHot
        // 来源圆点：codex 琥珀，claude 紫。
        let dot = session.source == "claude" ? (NSColor(hex: "9B8AFB") ?? .systemPurple)
                                             : (NSColor(hex: "E8A33D") ?? .systemOrange)
        drawDot(dot)
        let para = NSMutableParagraphStyle()
        para.lineBreakMode = .byTruncatingTail
        // 右侧次要信息：项目名
        let proj = session.project ?? ""
        let metaColor = hot ? NSColor.white.withAlphaComponent(0.62) : NSColor.tertiaryLabelColor
        let meta = NSAttributedString(string: proj, attributes: [
            .foregroundColor: metaColor, .paragraphStyle: para,
            .font: NSFont.monospacedSystemFont(ofSize: 10, weight: .medium)])
        let metaW = proj.isEmpty ? 0 : min(meta.size().width, 130)
        let metaX = bounds.width - 14 - metaW
        if metaW > 0 {
            meta.draw(in: NSRect(x: metaX, y: (bounds.height - 16) / 2, width: metaW, height: 16))
        }
        // 标题
        let titleColor = hot ? NSColor.white : NSColor.labelColor
        let raw = session.title.isEmpty ? "（无标题）" : session.title
        let title = NSAttributedString(string: ellipsizeStr(raw, 44), attributes: [
            .foregroundColor: titleColor, .paragraphStyle: para,
            .font: NSFont.menuFont(ofSize: 13)])
        title.draw(in: NSRect(x: 32, y: (bounds.height - 17) / 2,
                              width: metaX - 32 - 10, height: 17))
    }

    override func mouseUp(with event: NSEvent) {
        enclosingMenuItem?.menu?.cancelTracking()
        let id = session.id
        DispatchQueue.main.async { [weak owner] in owner?.openSessionInWindow(id) }
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate, WKNavigationDelegate, WKUIDelegate,
                         NSMenuDelegate, NSWindowDelegate, UNUserNotificationCenterDelegate {
    var window: NSWindow!
    var webView: WKWebView!
    var statusItem: NSStatusItem!
    var port = 7800
    let bg = NSColor(red: 0.027, green: 0.027, blue: 0.031, alpha: 1)
    var selectedTab: MenuTab = .issues   // 当前菜单标签页，跨次开合保留
    var menuData: MenubarResponse?       // 上次拉取的菜单数据，切标签复用
    var pollTimer: Timer?                // 后台轮询 issue，驱动菜单栏红点
    var hasNewIssues = false             // 当前是否存在未读新 issue（红点开关）
    // 已看过的 issue（"<repo>#<number>"），持久化；打开菜单看过列表即视为已读。
    var seenIssueKeys: Set<String> =
        Set(UserDefaults.standard.stringArray(forKey: "SeenIssueKeys") ?? [])
    // ── 系统通知状态 ──
    var notifyEnabled = true              // 设置开关：有新 issue/PR 时发系统通知（由 config 刷新）
    var notifyAuthorized = false          // 是否已拿到系统通知授权
    // 各侧首轮拉取仅建立基线（静默），避免启动把存量条目刷屏；两侧独立，
    // 一侧 gh 报错不影响另一侧。
    var issueBaselineDone = false
    var prBaselineDone = false
    // 每个 issue/PR 的评论数基线（key："i:repo#n" / "p:repo#n"），不持久化。
    // 基线没有的 key = 新条目；评论数比基线大 = 新评论。每轮重建以淘汰已关闭项。
    var commentBaseline = [String: Int]()

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
        statusItem.button?.toolTip = "Codex Viewer — 我的 Issue"
        updateStatusIcon()
        let menu = NSMenu()
        menu.delegate = self
        statusItem.menu = menu
        setupNotifications()
        startIssuePolling()
    }

    // 申请系统通知授权，并接管通知中心回调（前台展示 / 点击打开链接）。
    func setupNotifications() {
        let center = UNUserNotificationCenter.current()
        center.delegate = self
        center.requestAuthorization(options: [.alert, .sound]) { [weak self] granted, _ in
            DispatchQueue.main.async { self?.notifyAuthorized = granted }
        }
    }

    // 根据 hasNewIssues 重绘菜单栏图标：无红点用模板符号（随菜单栏明暗自适应）；
    // 有红点则合成「符号本体 + 右上角红点」的彩色图。
    func updateStatusIcon() {
        guard let btn = statusItem?.button else { return }
        guard let base = NSImage(systemSymbolName: "smallcircle.filled.circle",
                                 accessibilityDescription: "我的 Issue") else {
            btn.title = hasNewIssues ? "◆ ●" : "◆"
            return
        }
        if !hasNewIssues {
            base.isTemplate = true
            btn.image = base
            return
        }
        let size = NSSize(width: 20, height: 16)
        let img = NSImage(size: size)
        img.lockFocus()
        // 在菜单栏当前外观下绘制，使 labelColor 解析为正确的明/暗色
        btn.effectiveAppearance.performAsCurrentDrawingAppearance {
            // 符号本体：先按模板画出剪影，再用 labelColor 染色（浅色菜单栏→黑、深色→白）
            let iconRect = NSRect(x: 0, y: 1, width: 14, height: 14)
            base.isTemplate = true
            base.draw(in: iconRect)
            NSColor.labelColor.set()
            iconRect.fill(using: .sourceAtop)
            // 右上角红点
            NSColor.systemRed.setFill()
            NSBezierPath(ovalIn: NSRect(x: size.width - 6, y: size.height - 6,
                                        width: 6, height: 6)).fill()
        }
        img.unlockFocus()
        img.isTemplate = false   // 含红色，不能当模板（否则会被系统抹成单色）
        btn.image = img
    }

    // 启动后台轮询：立刻拉一次，之后每 60s 拉一次 /api/menubar，重算红点。
    func startIssuePolling() {
        pollIssuesOnce()
        pollTimer = Timer.scheduledTimer(withTimeInterval: 60, repeats: true) { [weak self] _ in
            self?.pollIssuesOnce()
        }
    }

    // 后台拉取菜单数据并刷新红点（不弹菜单，仅更新图标）。
    func pollIssuesOnce() {
        DispatchQueue.global().async { [weak self] in
            let data = self?.fetchMenubar()
            DispatchQueue.main.async {
                guard let self = self, let data = data else { return }
                self.menuData = data
                self.recomputeBadge()
                self.maybeNotifyNewItems()
            }
        }
    }

    // 比对当前 issue 与「已看过」集合：存在未见过的编号则亮红点。
    func recomputeBadge() {
        let cur = menuData?.issues ?? []
        let isNew = cur.contains { !seenIssueKeys.contains("\($0.repo)#\($0.number)") }
        if isNew != hasNewIssues {
            hasNewIssues = isNew
            updateStatusIcon()
        }
    }

    // 比对最新数据与评论数基线，发系统通知：基线里没有的条目 → 「新 issue/PR」；
    // 评论数比基线大 → 「新评论」。issue 与 PR 两侧各自独立处理（某侧 gh 报错时跳过该侧）。
    func maybeNotifyNewItems() {
        if (menuData?.issuesError ?? "").isEmpty, let issues = menuData?.issues {
            let entries = issues.map { it in
                NotifyEntry(key: "i:\(it.repo)#\(it.number)", comments: it.comments ?? 0,
                            newTitle: "新 issue · \(it.repo)",
                            commentTitle: "issue 新评论 · \(it.repo)",
                            body: "#\(it.number) \(it.title)", url: it.url)
            }
            processNotifySide(prefix: "i:", silent: !issueBaselineDone, entries: entries)
            issueBaselineDone = true
        }
        if (menuData?.prsError ?? "").isEmpty, let prs = menuData?.prs {
            let entries = prs.map { pr -> NotifyEntry in
                let review = (pr.reason == "review")
                return NotifyEntry(key: "p:\(pr.repo)#\(pr.number)", comments: pr.comments ?? 0,
                            newTitle: review ? "待你 review 的 PR · \(pr.repo)" : "新 PR · \(pr.repo)",
                            commentTitle: "PR 新评论 · \(pr.repo)",
                            body: "#\(pr.number) \(pr.title)", url: pr.url)
            }
            processNotifySide(prefix: "p:", silent: !prBaselineDone, entries: entries)
            prBaselineDone = true
        }
    }

    // 重建某一前缀的评论数基线，并按需发通知。muted（首轮 / 开关关 / 未授权）时
    // 只更新基线、不发通知 —— 这样开关重开后不会把存量条目当成新的刷屏。
    func processNotifySide(prefix: String, silent: Bool, entries: [NotifyEntry]) {
        let muted = silent || !notifyEnabled || !notifyAuthorized
        var rebuilt = commentBaseline.filter { !$0.key.hasPrefix(prefix) }
        for e in entries {
            let prev = commentBaseline[e.key]
            rebuilt[e.key] = e.comments
            if muted { continue }
            if prev == nil {
                postNotify(id: "\(e.key):new", title: e.newTitle, body: e.body, url: e.url)
            } else if e.comments > prev! {
                postNotify(id: "\(e.key):c\(e.comments)", title: e.commentTitle, body: e.body, url: e.url)
            }
        }
        commentBaseline = rebuilt
    }

    // 投递一条系统通知；userInfo 带 url，点击时打开对应 GitHub 页面。
    func postNotify(id: String, title: String, body: String, url: String) {
        let content = UNMutableNotificationContent()
        content.title = title
        content.body = body
        content.sound = .default
        if !url.isEmpty { content.userInfo = ["url": url] }
        let req = UNNotificationRequest(identifier: id, content: content, trigger: nil)
        UNUserNotificationCenter.current().add(req, withCompletionHandler: nil)
    }

    // 前台运行时也展示通知横幅（默认前台会被系统吞掉）。
    func userNotificationCenter(_ center: UNUserNotificationCenter,
                                willPresent notification: UNNotification,
                                withCompletionHandler completionHandler:
                                    @escaping (UNNotificationPresentationOptions) -> Void) {
        if #available(macOS 11.0, *) { completionHandler([.banner, .sound]) }
        else { completionHandler([.alert, .sound]) }
    }

    // 点击通知 → 打开 userInfo 里的 GitHub 链接。
    func userNotificationCenter(_ center: UNUserNotificationCenter,
                                didReceive response: UNNotificationResponse,
                                withCompletionHandler completionHandler: @escaping () -> Void) {
        if let url = response.notification.request.content.userInfo["url"] as? String {
            DispatchQueue.main.async { [weak self] in self?.openIssueLink(url) }
        }
        completionHandler()
    }

    // 用户打开菜单看过 issue 列表后：把当前全部 issue 记为已读，红点熄灭。
    // seen 直接重置为「当前 open 全集」，已关闭的编号自动淘汰，集合不膨胀。
    func markIssuesSeen() {
        // 注意：后端无错误时发的是空字符串 ""（非 nil），不能用 == nil 判断，
        // 否则守卫永远失败、issue 永不标记已读、红点永不消失。
        guard (menuData?.issuesError ?? "").isEmpty, let issues = menuData?.issues else { return }
        seenIssueKeys = Set(issues.map { "\($0.repo)#\($0.number)" })
        UserDefaults.standard.set(Array(seenIssueKeys), forKey: "SeenIssueKeys")
        if hasNewIssues {
            hasNewIssues = false
            updateStatusIcon()
        }
    }

    // 菜单行的统一宽度。
    let menuWidth: CGFloat = 470

    // 菜单即将显示前：拉取最新数据（读后端缓存），再按当前标签页构建。
    func menuNeedsUpdate(_ menu: NSMenu) {
        // 顺带按配置同步「开机自启动」（异步，不拖慢菜单弹出）。
        DispatchQueue.global().async { [weak self] in
            let s = self?.fetchConfig()?.startup
            DispatchQueue.main.async { self?.applyStartupConfig(s) }
        }
        menuData = fetchMenubar()
        rebuildMenu(menu)
    }

    // 按 selectedTab 重建整个菜单 —— 切换标签页时也走这里（复用已拉取的 menuData）。
    func rebuildMenu(_ menu: NSMenu) {
        menu.removeAllItems()
        let segItem = NSMenuItem()
        segItem.view = makeTabBar(width: menuWidth)
        menu.addItem(segItem)
        menu.addItem(.separator())

        if menuData == nil {
            addInfoItem(menu, "无法连接到服务")
        } else {
            switch selectedTab {
            case .issues:   buildIssues(menu); markIssuesSeen()
            case .prs:      buildPRs(menu)
            case .sessions: buildSessions(menu)
            }
        }
        addFooter(menu)
    }

    // 顶部分段控件：三个标签页 + 各自数量徽标。
    func makeTabBar(width: CGFloat) -> NSView {
        let v = NSView(frame: NSRect(x: 0, y: 0, width: width, height: 36))
        let seg = NSSegmentedControl(labels: tabLabels(), trackingMode: .selectOne,
                                     target: self, action: #selector(tabChanged(_:)))
        seg.selectedSegment = selectedTab.rawValue
        let segW = (width - 28) / 3
        for i in 0..<3 { seg.setWidth(segW, forSegment: i) }
        seg.frame = NSRect(x: 14, y: 4, width: width - 28, height: 28)
        seg.autoresizingMask = [.width]
        v.addSubview(seg)
        return v
    }

    // 各标签页的文字 —— 有数据时带计数。
    func tabLabels() -> [String] {
        let ic = menuData?.issues?.count ?? 0
        let pc = menuData?.prs?.count ?? 0
        let sc = menuData?.sessions?.count ?? 0
        return [ic > 0 ? "Issue \(ic)" : "Issue",
                pc > 0 ? "PR \(pc)" : "PR",
                sc > 0 ? "会话 \(sc)" : "会话"]
    }

    @objc func tabChanged(_ sender: NSSegmentedControl) {
        selectedTab = MenuTab(rawValue: sender.selectedSegment) ?? .issues
        if let menu = statusItem.menu { rebuildMenu(menu) }
    }

    // issue 标签页内容。
    func buildIssues(_ menu: NSMenu) {
        guard let d = menuData else { return }
        if !(d.showInMenu ?? true) {
            addInfoItem(menu, "issue 菜单显示已关闭 —— 可在设置开启")
            return
        }
        if let ms = d.issuesUpdated, ms > 0 {
            addInfoItem(menu, relTime(ms))
            menu.addItem(.separator())
        }
        let list = d.issues ?? []
        if list.isEmpty {
            let err = d.issuesError ?? ""
            addInfoItem(menu, err.isEmpty ? "暂无分配给你的 issue —— 点设置关注仓库"
                                          : "issue 拉取失败 —— 点设置检查")
            return
        }
        for it in list.prefix(d.menuMax ?? 20) {
            let item = NSMenuItem()
            item.view = IssueRowView(issue: it, width: menuWidth, owner: self)
            menu.addItem(item)
        }
    }

    // PR 标签页内容。
    func buildPRs(_ menu: NSMenu) {
        guard let d = menuData else { return }
        if let ms = d.prsUpdated, ms > 0 {
            addInfoItem(menu, relTime(ms))
            menu.addItem(.separator())
        }
        let list = d.prs ?? []
        if list.isEmpty {
            let err = d.prsError ?? ""
            addInfoItem(menu, err.isEmpty ? "暂无与你相关的 PR —— 点设置关注仓库"
                                          : "PR 拉取失败 —— 点设置检查")
            return
        }
        for it in list.prefix(d.menuMax ?? 20) {
            let item = NSMenuItem()
            item.view = PRRowView(pr: it, width: menuWidth, owner: self)
            menu.addItem(item)
        }
    }

    // 活跃会话标签页内容。
    func buildSessions(_ menu: NSMenu) {
        guard let d = menuData else { return }
        let list = d.sessions ?? []
        if list.isEmpty {
            addInfoItem(menu, "最近 1 分钟内没有活跃会话")
            return
        }
        for it in list.prefix(d.menuMax ?? 20) {
            let item = NSMenuItem()
            item.view = SessionRowView(session: it, width: menuWidth, owner: self)
            menu.addItem(item)
        }
    }

    // 菜单底部固定操作项。
    func addFooter(_ menu: NSMenu) {
        menu.addItem(.separator())
        let refresh = NSMenuItem(title: "刷新", action: #selector(refreshMenuNow),
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

    // 「刷新」→ 强制后端重新拉取 issue 与 PR，完成后重新弹出菜单显示新数据。
    @objc func refreshMenuNow() {
        DispatchQueue.global().async {
            if let url = URL(string: "http://127.0.0.1:\(self.port)/api/menubar?refresh=1") {
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

    // 唤起主窗口并定位到指定会话（供菜单栏「活跃会话」点击）。
    func openSessionInWindow(_ id: String) {
        showMainWindow()
        let esc = id.replacingOccurrences(of: "\\", with: "\\\\")
                    .replacingOccurrences(of: "'", with: "\\'")
        webView.evaluateJavaScript("window.openSession && window.openSession('\(esc)')",
                                   completionHandler: nil)
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
        guard let s = s else { return }
        // 通知开关随配置刷新（默认开）；这一步不依赖系统版本，放在登录项之前。
        notifyEnabled = s.notifyOnNewItems ?? true
        guard #available(macOS 13.0, *) else { return }
        let want = s.launchAtLogin ?? false
        let svc = SMAppService.mainApp
        let isOn = svc.status == .enabled
        if want && !isOn {
            try? svc.register()
        } else if !want && isOn {
            try? svc.unregister()
        }
    }

    // 同步拉取 /api/menubar（读 Go 后端缓存，毫秒级）；返回 nil 表示请求失败。
    func fetchMenubar() -> MenubarResponse? {
        guard let url = URL(string: "http://127.0.0.1:\(port)/api/menubar") else { return nil }
        var req = URLRequest(url: url)
        req.timeoutInterval = 2
        var result: MenubarResponse? = nil
        let sem = DispatchSemaphore(value: 0)
        URLSession.shared.dataTask(with: req) { data, _, _ in
            defer { sem.signal() }
            if let data = data {
                result = try? JSONDecoder().decode(MenubarResponse.self, from: data)
            }
        }.resume()
        _ = sem.wait(timeout: .now() + 3)
        return result
    }

    // 菜单行标题：#编号（暗）+ 仓库短名（更暗）+ 标题；高亮时改用亮色。
    func rowTitle(number: Int, repo: String, text: String,
                  highlighted hot: Bool) -> NSAttributedString {
        let para = NSMutableParagraphStyle()
        para.lineBreakMode = .byTruncatingTail
        let numColor   = hot ? NSColor.white.withAlphaComponent(0.92) : NSColor.secondaryLabelColor
        let repoColor  = hot ? NSColor.white.withAlphaComponent(0.62) : NSColor.tertiaryLabelColor
        let titleColor = hot ? NSColor.white : NSColor.labelColor
        let r = NSMutableAttributedString()
        r.append(NSAttributedString(string: "#\(number)  ", attributes: [
            .foregroundColor: numColor, .paragraphStyle: para,
            .font: NSFont.monospacedDigitSystemFont(ofSize: 11, weight: .semibold)]))
        let short = repo.split(separator: "/").last.map(String.init) ?? repo
        r.append(NSAttributedString(string: short + "  ", attributes: [
            .foregroundColor: repoColor, .paragraphStyle: para,
            .font: NSFont.monospacedSystemFont(ofSize: 9, weight: .medium)]))
        r.append(NSAttributedString(string: ellipsize(text, 44), attributes: [
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

    // ── JS 原生对话框 ──────────────────────────────────────────
    // WKWebView 默认不弹 alert/confirm/prompt：必须实现这些回调，否则
    // confirm() 直接返回 false、alert() 静默丢弃。后果是前端所有
    // `if(!confirm())return` 的按钮（重启 / 重启应用 / 终止会话）点了毫无反应。
    // 用 runModal（应用级模态）保证在窗口任意可见状态下都能弹出。

    func webView(_ webView: WKWebView,
                 runJavaScriptAlertPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo,
                 completionHandler: @escaping () -> Void) {
        let a = NSAlert()
        a.messageText = "Codex Viewer"
        a.informativeText = message
        a.addButton(withTitle: "好")
        a.runModal()
        completionHandler()
    }

    func webView(_ webView: WKWebView,
                 runJavaScriptConfirmPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo,
                 completionHandler: @escaping (Bool) -> Void) {
        let a = NSAlert()
        a.messageText = "Codex Viewer"
        a.informativeText = message
        a.addButton(withTitle: "确定")
        a.addButton(withTitle: "取消")
        completionHandler(a.runModal() == .alertFirstButtonReturn)
    }

    func webView(_ webView: WKWebView,
                 runJavaScriptTextInputPanelWithPrompt prompt: String,
                 defaultText: String?,
                 initiatedByFrame frame: WKFrameInfo,
                 completionHandler: @escaping (String?) -> Void) {
        let a = NSAlert()
        a.messageText = prompt
        let tf = NSTextField(frame: NSRect(x: 0, y: 0, width: 240, height: 24))
        tf.stringValue = defaultText ?? ""
        a.accessoryView = tf
        a.addButton(withTitle: "确定")
        a.addButton(withTitle: "取消")
        completionHandler(a.runModal() == .alertFirstButtonReturn ? tf.stringValue : nil)
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
