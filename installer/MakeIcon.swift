import AppKit

let file = CommandLine.arguments[1]
let canvas = NSSize(width: 1024, height: 1024)
let artwork = NSImage(size: canvas, flipped: false) { _ in
    let background = NSBezierPath(
        roundedRect: NSRect(x: 92, y: 92, width: 840, height: 840),
        xRadius: 190, yRadius: 190
    )
    let gradient = NSGradient(
        starting: NSColor(srgbRed: 0.22, green: 0.39, blue: 0.86, alpha: 1),
        ending: NSColor(srgbRed: 0.09, green: 0.19, blue: 0.54, alpha: 1)
    )
    gradient?.draw(in: background, angle: -70)

    // Tint the glyphs offscreen so the tint covers just the symbol.
    func whiteGlyph(_ name: String, pointSize: CGFloat) -> NSImage? {
        guard let symbol = NSImage(systemSymbolName: name, accessibilityDescription: nil)?
            .withSymbolConfiguration(.init(pointSize: pointSize, weight: .semibold)) else { return nil }
        return NSImage(size: symbol.size, flipped: false) { rect in
            symbol.draw(in: rect)
            NSColor.white.setFill()
            rect.fill(using: .sourceAtop)
            return true
        }
    }
    func place(_ image: NSImage?, at center: NSPoint) {
        guard let image else { return }
        let size = image.size
        image.draw(in: NSRect(x: center.x - size.width / 2, y: center.y - size.height / 2, width: size.width, height: size.height))
    }
    place(whiteGlyph("printer.fill", pointSize: 386), at: NSPoint(x: 501, y: 535))

    let badge = NSBezierPath(ovalIn: NSRect(x: 646, y: 219, width: 220, height: 220))
    NSColor(srgbRed: 0.01, green: 0.46, blue: 0.48, alpha: 1).setFill()
    badge.fill()
    NSColor(srgbRed: 0.11, green: 0.25, blue: 0.67, alpha: 1).setStroke()
    badge.lineWidth = 16
    badge.stroke()
    place(whiteGlyph("arrow.down", pointSize: 112), at: NSPoint(x: 756, y: 329))
    return true
}
var rect = NSRect(origin: .zero, size: canvas)
guard let raster = artwork.cgImage(forProposedRect: &rect, context: nil, hints: nil),
      let png = NSBitmapImageRep(cgImage: raster).representation(using: .png, properties: [:]) else {
    fatalError("Could not render the installer icon")
}
try png.write(to: URL(fileURLWithPath: file))
