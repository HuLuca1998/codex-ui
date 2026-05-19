// makeicon.swift — 渲染 Codex Viewer 的 1024×1024 应用图标。
// 用法: makeicon <输出png路径>
import Cocoa

let size = 1024
let rep = NSBitmapImageRep(
    bitmapDataPlanes: nil, pixelsWide: size, pixelsHigh: size,
    bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
    colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!

NSGraphicsContext.saveGraphicsState()
NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
let ctx = NSGraphicsContext.current!.cgContext
let S = CGFloat(size)

// 圆角方形底（macOS 风格 squircle 近似）
let inset: CGFloat = 88
let rect = CGRect(x: inset, y: inset, width: S - 2*inset, height: S - 2*inset)
let corner = (S - 2*inset) * 0.225
let bg = CGPath(roundedRect: rect, cornerWidth: corner, cornerHeight: corner, transform: nil)
ctx.addPath(bg)
ctx.setFillColor(NSColor(red: 0.082, green: 0.082, blue: 0.098, alpha: 1).cgColor)
ctx.fillPath()
ctx.addPath(bg)
ctx.setStrokeColor(NSColor(white: 1, alpha: 0.06).cgColor)
ctx.setLineWidth(3)
ctx.strokePath()

// 三条胶囊条 —— 抽象「对话时间线」标记
let accent = NSColor(red: 0.604, green: 0.627, blue: 0.910, alpha: 1)
let cx = S / 2
let barH: CGFloat = 88
let bars: [(w: CGFloat, dy: CGFloat, a: CGFloat)] = [
    (392,  128, 1.0),
    (300,    0, 0.62),
    (212, -128, 0.36),
]
for b in bars {
    let r = CGRect(x: cx - b.w/2, y: S/2 + b.dy - barH/2, width: b.w, height: barH)
    ctx.addPath(CGPath(roundedRect: r, cornerWidth: barH/2, cornerHeight: barH/2, transform: nil))
    ctx.setFillColor(accent.withAlphaComponent(b.a).cgColor)
    ctx.fillPath()
}

NSGraphicsContext.restoreGraphicsState()
let out = CommandLine.arguments.count > 1 ? CommandLine.arguments[1] : "icon.png"
try! rep.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: out))
