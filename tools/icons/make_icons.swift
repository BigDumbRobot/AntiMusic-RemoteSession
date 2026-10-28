// Generates the Anti-Music icons from the logo:
//   assets/icons/plugin-icon.png  stencil of the bass clef with glasses and
//                                 mohawk, "ANTI-MUSIC" underneath (all plugins)
//   assets/icons/app-icon.png     the striped sun, "ANTI-MUSIC" underneath
// Run: swiftc -O -o build/make_icons tools/icons/make_icons.swift && build/make_icons
import AppKit
import CoreGraphics

let size = 1024
let ink = CGColor(red: 0.95, green: 0.93, blue: 0.96, alpha: 1) // stencil colour
let pink = CGColor(red: 1.0, green: 0.36, blue: 0.62, alpha: 1)
let yellow = CGColor(red: 1.0, green: 0.8, blue: 0.45, alpha: 1)

func context() -> CGContext {
    CGContext(data: nil, width: size, height: size, bitsPerComponent: 8, bytesPerRow: 0,
              space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!
}

func save(_ ctx: CGContext, _ path: String) {
    let rep = NSBitmapImageRep(cgImage: ctx.makeImage()!)
    try! rep.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: path))
    print("wrote \(path)")
}

/// macOS-style rounded square with a vertical gradient.
func background(_ ctx: CGContext, top: CGColor, bottom: CGColor) {
    let inset: CGFloat = 100, r = CGRect(x: inset, y: inset, width: CGFloat(size) - 2 * inset, height: CGFloat(size) - 2 * inset)
    let path = CGPath(roundedRect: r, cornerWidth: 185, cornerHeight: 185, transform: nil)
    ctx.saveGState()
    ctx.addPath(path)
    ctx.clip()
    let g = CGGradient(colorsSpace: CGColorSpace(name: CGColorSpace.sRGB)!, colors: [top, bottom] as CFArray, locations: [0, 1])!
    ctx.drawLinearGradient(g, start: CGPoint(x: 0, y: r.maxY), end: CGPoint(x: 0, y: r.minY), options: [])
    ctx.restoreGState()
}

/// "ANTI-MUSIC" with a stencil break cut through the letters.
func lettering(_ ctx: CGContext, centerY: CGFloat, height: CGFloat, color: CGColor) {
    let font = NSFont(name: "Futura-CondensedExtraBold", size: height) ?? NSFont.boldSystemFont(ofSize: height)
    let text = NSAttributedString(string: "ANTI-MUSIC", attributes: [.font: font, .kern: height * 0.04,
                                                                      .foregroundColor: NSColor(cgColor: color)!])
    let line = CTLineCreateWithAttributedString(text)
    let bounds = CTLineGetImageBounds(line, ctx)
    // Draw into a layer so the break only removes lettering.
    let layer = CGLayer(ctx, size: CGSize(width: size, height: size), auxiliaryInfo: nil)!
    let lc = layer.context!
    let x = (CGFloat(size) - bounds.width) / 2 - bounds.minX
    let y = centerY - bounds.height / 2 - bounds.minY
    lc.textPosition = CGPoint(x: x, y: y)
    CTLineDraw(line, lc)
    lc.setBlendMode(.clear)
    lc.fill(CGRect(x: 0, y: y + bounds.minY + bounds.height * 0.47, width: CGFloat(size), height: max(3, height * 0.055)))
    ctx.draw(layer, at: .zero)
}

// MARK: - Plugin icon: stencil from the logo

func stencilMask(logo path: String) -> CGImage {
    let src = NSImage(contentsOfFile: path)!.cgImage(forProposedRect: nil, context: nil, hints: nil)!
    // The clef, glasses and mohawk sit in this part of the 1408x768 logo.
    let crop = src.cropping(to: CGRect(x: 520, y: 40, width: 380, height: 510))!
    let w = crop.width, h = crop.height
    var px = [UInt8](repeating: 0, count: w * h * 4)
    let c = CGContext(data: &px, width: w, height: h, bitsPerComponent: 8, bytesPerRow: w * 4,
                      space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!
    c.draw(crop, in: CGRect(x: 0, y: 0, width: w, height: h))
    var on = [Bool](repeating: false, count: w * h)
    for i in 0..<(w * h) {
        let r = Int(px[i * 4]), g = Int(px[i * 4 + 1]), b = Int(px[i * 4 + 2])
        let luma = (299 * r + 587 * g + 114 * b) / 1000
        let isPink = r > 140 && r - g > 70 && b > 60
        on[i] = luma < 75 || isPink
    }
    // Keep only large connected shapes: drops grime and spray specks.
    var label = [Int](repeating: -1, count: w * h)
    var sizes: [Int] = []
    for start in 0..<(w * h) where on[start] && label[start] < 0 {
        let id = sizes.count
        var stack = [start], count = 0
        label[start] = id
        while let p = stack.popLast() {
            count += 1
            let x = p % w, y = p / w
            for (dx, dy) in [(1, 0), (-1, 0), (0, 1), (0, -1)] {
                let nx = x + dx, ny = y + dy
                guard nx >= 0, ny >= 0, nx < w, ny < h else { continue }
                let q = ny * w + nx
                if on[q] && label[q] < 0 { label[q] = id; stack.append(q) }
            }
        }
        sizes.append(count)
    }
    var mask = [UInt8](repeating: 0, count: w * h)
    for i in 0..<(w * h) where label[i] >= 0 && sizes[label[i]] > 1000 /* clef dots are ~1600 px, grime under 800 */ { mask[i] = 255 }
    let provider = CGDataProvider(data: Data(mask) as CFData)!
    return CGImage(width: w, height: h, bitsPerComponent: 8, bitsPerPixel: 8, bytesPerRow: w,
                   space: CGColorSpaceCreateDeviceGray(), bitmapInfo: [], provider: provider,
                   decode: nil, shouldInterpolate: true, intent: .defaultIntent)!
}

let root = CommandLine.arguments.count > 1 ? CommandLine.arguments[1] : "."
let mask = stencilMask(logo: root + "/Anti-Music Logo.jpeg")

let plugin = context()
background(plugin, top: CGColor(red: 0.13, green: 0.11, blue: 0.16, alpha: 1), bottom: CGColor(red: 0.06, green: 0.05, blue: 0.08, alpha: 1))
let mh: CGFloat = 520, mw = mh * CGFloat(mask.width) / CGFloat(mask.height)
plugin.saveGState()
plugin.clip(to: CGRect(x: (CGFloat(size) - mw) / 2, y: 335, width: mw, height: mh), mask: mask)
plugin.setFillColor(ink)
plugin.fill(CGRect(x: 0, y: 0, width: size, height: size))
plugin.restoreGState()
lettering(plugin, centerY: 230, height: 130, color: ink)
save(plugin, root + "/assets/icons/plugin-icon.png")

// MARK: - App icon: the sun

let app = context()
background(app, top: CGColor(red: 0.2, green: 0.12, blue: 0.27, alpha: 1), bottom: CGColor(red: 0.08, green: 0.06, blue: 0.12, alpha: 1))
let sun = CGRect(x: 512 - 230, y: 330, width: 460, height: 460)
app.saveGState()
app.beginTransparencyLayer(auxiliaryInfo: nil)
app.addEllipse(in: sun)
app.clip()
let sunGrad = CGGradient(colorsSpace: CGColorSpace(name: CGColorSpace.sRGB)!, colors: [yellow, pink] as CFArray, locations: [0, 1])!
app.drawLinearGradient(sunGrad, start: CGPoint(x: 0, y: sun.maxY), end: CGPoint(x: 0, y: sun.minY), options: [])
app.setBlendMode(.clear)
var y = sun.midY, gap: CGFloat = 14
while y > sun.minY { // stripes thicken toward the bottom
    app.fill(CGRect(x: sun.minX, y: y - gap, width: sun.width, height: gap))
    y -= gap + 40
    gap += 9
}
app.endTransparencyLayer()
app.restoreGState()
lettering(app, centerY: 230, height: 130, color: ink)
save(app, root + "/assets/icons/app-icon.png")
