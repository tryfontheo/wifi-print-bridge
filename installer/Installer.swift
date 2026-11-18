import AppKit
import Foundation

private enum InstallResult {
    case installed(String)
    case cancelled
    case failed(String)
}

private struct DiscoveredPrinter: Decodable, Equatable {
    let name: String
    let uri: String
    let address: String
    let interface: String
    let airPrint: Bool

    enum CodingKeys: String, CodingKey {
        case name, uri, address, interface
        case airPrint = "air_print"
    }
}

private enum PrinterChoice: Equatable {
    case placeholder, installed, manual, discovered(DiscoveredPrinter)
}

private func discoverPrinters(at script: URL) -> [DiscoveredPrinter]? {
    let process = Process()
    let output = Pipe()
    process.executableURL = URL(fileURLWithPath: "/bin/bash")
    process.arguments = [script.path, "discover"]
    process.standardOutput = output
    process.standardError = FileHandle.nullDevice
    do {
        try process.run()
    } catch {
        return nil
    }
    let data = output.fileHandleForReading.readDataToEndOfFile()
    process.waitUntilExit()
    guard process.terminationStatus == 0 else { return nil }
    return try? JSONDecoder().decode([DiscoveredPrinter].self, from: data)
}

private func manualIPPAddress(_ entry: String) -> String? {
    let address = entry.trimmingCharacters(in: .whitespacesAndNewlines)
    let input = address.contains("://") ? address : "ipp://" + address
    guard var url = URLComponents(string: input), url.scheme?.lowercased() == "ipp",
          url.user == nil, url.password == nil, url.query == nil, url.fragment == nil,
          let host = url.host, !host.isEmpty else { return nil }
    let octets = host.split(separator: ".", omittingEmptySubsequences: false)
    guard octets.count == 4, octets.allSatisfy({
        guard let number = UInt8($0) else { return false }
        return $0 == String(number)
    }) else { return nil }
    url.scheme = "ipp"
    if url.port == nil { url.port = 631 }
    if let port = url.port, !(1...65535).contains(port) { return nil }
    if url.path.isEmpty { url.path = "/ipp/print" }
    return url.string
}

private func shellQuote(_ value: String) -> String {
    "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
}

private func runInstaller(at script: URL, options: [String]) -> InstallResult {
    let exitMarker = "__WIFI_PRINT_BRIDGE_EXIT__="
    let errorMarker = "__WIFI_PRINT_BRIDGE_AUTH__="
    let arguments = ["/bin/bash", script.path, "install"] + options
    let command = arguments.map(shellQuote).joined(separator: " ") +
        " 2>&1; result=$?; /usr/bin/printf '\\n\(exitMarker)%s\\n' \"$result\""
    let appleScript = """
        on run arguments
            try
                return do shell script (item 1 of arguments) with administrator privileges without altering line endings
            on error errorMessage number errorNumber
                return "\(errorMarker)" & errorNumber & linefeed & errorMessage
            end try
        end run
        """

    let process = Process()
    let outputPipe = Pipe()
    process.executableURL = URL(fileURLWithPath: "/usr/bin/osascript")
    process.arguments = ["-e", appleScript, "--", command]
    process.standardOutput = outputPipe
    process.standardError = outputPipe
    do {
        try process.run()
    } catch {
        return .failed(error.localizedDescription)
    }

    let data = outputPipe.fileHandleForReading.readDataToEndOfFile()
    process.waitUntilExit()
    let output = String(decoding: data, as: UTF8.self)
        .trimmingCharacters(in: .whitespacesAndNewlines)

    if output.hasPrefix(errorMarker) {
        let parts = output.dropFirst(errorMarker.count).split(separator: "\n", maxSplits: 1)
        if parts.first == "-128" {
            return .cancelled
        }
        let message = parts.dropFirst().first.map(String.init) ?? output
        return .failed(message)
    }

    let lines = output.split(separator: "\n", omittingEmptySubsequences: false)
    guard process.terminationStatus == 0,
          let last = lines.last, last.hasPrefix(exitMarker),
          let exitStatus = Int(last.dropFirst(exitMarker.count)) else {
        return .failed(output.isEmpty ? "The macOS installer could not start." : output)
    }

    let log = lines.dropLast().joined(separator: "\n").trimmingCharacters(in: .whitespacesAndNewlines)
    return exitStatus == 0 ? .installed(log) : .failed(log)
}

@MainActor
private final class InstallerWindow: NSObject, NSApplicationDelegate, NSWindowDelegate, NSTextFieldDelegate {
    private let width: CGFloat = 560
    private let window = NSWindow(
        contentRect: NSRect(x: 0, y: 0, width: 560, height: 276),
        styleMask: [.titled, .closable, .miniaturizable],
        backing: .buffered,
        defer: false
    )
    private let layout = NSStackView()
    private let heading = NSTextField(labelWithString: "Wi-Fi Print Bridge")
    private let descriptionLabel = NSTextField(wrappingLabelWithString:
        "Share an Ethernet printer with devices on Wi-Fi. The bridge starts automatically at login."
    )
    private let status = NSTextField(wrappingLabelWithString: "macOS will ask for an administrator password.")
    private let printerSettings = NSStackView()
    private let printerPicker = NSPopUpButton(frame: .zero, pullsDown: false)
    private let rescan = NSButton(title: "Rescan", target: nil, action: nil)
    private let discoveryStatus = NSTextField(wrappingLabelWithString: "")
    private let manualPrinterRow = NSStackView()
    private let printerAddress = NSTextField(string: "")
    private let windowsLAN = NSButton(
        checkboxWithTitle: "Allow Windows printing on this Wi-Fi network", target: nil, action: nil
    )
    private let settings = NSStackView()
    private let wifiAddress = NSTextField(string: "")
    private let ethernetAddress = NSTextField(string: "")
    private let routing = NSPopUpButton(frame: .zero, pullsDown: false)
    private let routeAddress = NSTextField(string: "")
    private let routeLabel = NSTextField(labelWithString: "")
    private let routeHelp = NSTextField(wrappingLabelWithString: "")
    private let routeFields = NSStackView()
    private let log = NSTextView()
    private let logScroll = NSScrollView()
    private let progress = NSProgressIndicator()
    private let settingsButton = NSButton(title: "Network Settings…", target: nil, action: nil)
    private let installButton = NSButton(title: "Install Bridge", target: nil, action: nil)

    private var busy = false
    private var installed = false
    private var scanning = false

    private var installerScript: URL? {
        guard let script = Bundle.main.resourceURL?.appendingPathComponent("bridge/install.sh"),
              FileManager.default.isReadableFile(atPath: script.path) else { return nil }
        return script
    }

    private var existingConfiguration: Bool {
        FileManager.default.fileExists(atPath: "/Library/Application Support/WiFiPrintBridge/config.sh")
    }

    private var printerChoice: PrinterChoice {
        printerPicker.selectedItem?.representedObject as? PrinterChoice ?? .placeholder
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        setupMenu()
        setupWindow()
        window.center()
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
        startDiscovery()
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        true
    }

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        busy ? .terminateCancel : .terminateNow
    }

    func windowShouldClose(_ sender: NSWindow) -> Bool {
        !busy
    }

    private func setupMenu() {
        let menu = NSMenu()
        let appItem = NSMenuItem()
        let appMenu = NSMenu(title: "Wi-Fi Print Bridge")
        appMenu.addItem(withTitle: "Quit Wi-Fi Print Bridge", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        menu.addItem(appItem)
        appItem.submenu = appMenu
        NSApp.mainMenu = menu
    }

    private func setupWindow() {
        window.title = "Wi-Fi Print Bridge Installer"
        window.delegate = self
        window.isReleasedWhenClosed = false
        window.initialFirstResponder = printerPicker
        guard let content = window.contentView else { return }

        layout.orientation = .vertical
        layout.alignment = .leading
        layout.detachesHiddenViews = true
        layout.spacing = 12
        layout.translatesAutoresizingMaskIntoConstraints = false
        content.addSubview(layout)
        NSLayoutConstraint.activate([
            layout.leadingAnchor.constraint(equalTo: content.leadingAnchor, constant: 26),
            layout.trailingAnchor.constraint(equalTo: content.trailingAnchor, constant: -26),
            layout.topAnchor.constraint(equalTo: content.topAnchor, constant: 22),
        ])

        heading.font = .systemFont(ofSize: 26, weight: .semibold)
        descriptionLabel.font = .systemFont(ofSize: 14)
        status.font = .systemFont(ofSize: 13)
        status.textColor = .secondaryLabelColor
        append(heading)
        append(descriptionLabel)
        layout.setCustomSpacing(17, after: descriptionLabel)
        setupPrinterPicker()
        append(printerSettings)
        windowsLAN.state = .on
        append(windowsLAN)

        setupSettings()
        append(settings)
        settings.isHidden = true

        append(status)
        setupLog()
        append(logScroll)
        logScroll.isHidden = true

        setupButtons()
        resize()
    }

    private func append(_ view: NSView) {
        layout.addArrangedSubview(view)
        view.widthAnchor.constraint(equalTo: layout.widthAnchor).isActive = true
    }

    private func textRow(_ label: NSTextField, field: NSView) -> NSStackView {
        let row = NSStackView(views: [label, field])
        row.orientation = .horizontal
        row.alignment = .centerY
        row.distribution = .fill
        row.spacing = 12
        label.font = .systemFont(ofSize: 13)
        label.widthAnchor.constraint(equalToConstant: 130).isActive = true
        return row
    }

    private func textRow(_ title: String, field: NSTextField) -> NSStackView {
        field.setAccessibilityLabel(title)
        return textRow(NSTextField(labelWithString: title), field: field)
    }

    private func setupPrinterPicker() {
        printerSettings.orientation = .vertical
        printerSettings.alignment = .leading
        printerSettings.spacing = 7
        printerSettings.detachesHiddenViews = true
        printerPicker.target = self
        printerPicker.action = #selector(printerChanged)
        printerPicker.setAccessibilityLabel("Ethernet printer")
        printerPicker.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
        printerPicker.cell?.lineBreakMode = .byTruncatingTail
        rescan.target = self
        rescan.action = #selector(startDiscovery)
        rescan.bezelStyle = .rounded

        let controls = NSStackView(views: [printerPicker, rescan])
        controls.orientation = .horizontal
        controls.alignment = .centerY
        controls.spacing = 8
        let pickerRow = textRow(NSTextField(labelWithString: "Ethernet printer"), field: controls)
        appendChoice("Searching for printers…", .placeholder)

        printerAddress.placeholderString = "IPv4 or ipp://address:631/ipp/print"
        printerAddress.delegate = self
        printerAddress.setAccessibilityLabel("Manual printer IPP address")
        let addressRow = textRow("Printer address", field: printerAddress)
        manualPrinterRow.orientation = .vertical
        manualPrinterRow.addArrangedSubview(addressRow)
        addressRow.widthAnchor.constraint(equalTo: manualPrinterRow.widthAnchor).isActive = true
        manualPrinterRow.isHidden = true

        discoveryStatus.font = .systemFont(ofSize: 12)
        discoveryStatus.textColor = .secondaryLabelColor
        [pickerRow, manualPrinterRow, discoveryStatus].forEach { row in
            printerSettings.addArrangedSubview(row)
            row.widthAnchor.constraint(equalTo: printerSettings.widthAnchor).isActive = true
        }
    }

    private func appendChoice(_ label: String, _ choice: PrinterChoice) {
        printerPicker.addItem(withTitle: label)
        printerPicker.lastItem?.representedObject = choice
    }

    @objc private func startDiscovery() {
        guard !busy, !installed, !scanning else { return }
        guard let script = installerScript else {
            finish(.failed("The installer files are missing. Open a fresh copy of the installer app."))
            return
        }
        let previous = printerChoice
        scanning = true
        discoveryStatus.stringValue = "Searching for Ethernet printers…"
        updateControls()
        DispatchQueue.global(qos: .userInitiated).async {
            let results = discoverPrinters(at: script)
            DispatchQueue.main.async {
                self.showDiscovered(results, preserving: previous)
            }
        }
    }

    private func showDiscovered(_ found: [DiscoveredPrinter]?, preserving previous: PrinterChoice) {
        scanning = false
        var seen = Set<String>()
        let printers = (found ?? [])
            .filter { seen.insert("\($0.interface) \($0.uri)").inserted }
            .sorted { $0.name.localizedStandardCompare($1.name) == .orderedAscending }
        printerPicker.removeAllItems()
        if existingConfiguration {
            appendChoice("Use the installed printer", .installed)
        } else if printers.count > 1 {
            appendChoice("Select a printer", .placeholder)
        }
        for printer in printers {
            let title = "\(printer.name) — \(printer.address)"
            let sameNameAndAddress = printers.filter {
                $0.name == printer.name && $0.address == printer.address
            }
            let sameInterface = sameNameAndAddress.filter { $0.interface == printer.interface }
            let detail = sameInterface.count > 1 ? "\(printer.interface), \(printer.uri)" : printer.interface
            let label = sameNameAndAddress.count > 1 ? "\(title) (\(detail))" : title
            appendChoice(label, .discovered(printer))
        }
        appendChoice("Enter address manually…", .manual)
        let prior = printerPicker.itemArray.first { ($0.representedObject as? PrinterChoice) == previous }
        printerPicker.select(prior ?? printerPicker.itemArray.first)
        updatePrinterChoice(focusManual: false)
        switch printerChoice {
        case .installed, .discovered: return
        case .manual, .placeholder: break
        }
        if found == nil {
            discoveryStatus.stringValue = "Couldn’t search Ethernet. Rescan or enter the printer address."
        } else if printers.isEmpty {
            discoveryStatus.stringValue = "No Ethernet printers found. Rescan or enter the printer address."
        } else if printerChoice == .placeholder {
            discoveryStatus.stringValue = "Select a printer to share on Wi-Fi."
        }
    }

    @objc private func printerChanged() {
        updatePrinterChoice(focusManual: true)
    }

    private func updatePrinterChoice(focusManual: Bool) {
        manualPrinterRow.isHidden = printerChoice != .manual
        switch printerChoice {
        case .placeholder:
            discoveryStatus.stringValue = "Select a printer to share on Wi-Fi."
        case .installed:
            discoveryStatus.stringValue = "Keep the current printer and update its Windows sharing setting."
            settings.isHidden = true
            settingsButton.title = "Network Settings…"
        case .manual:
            discoveryStatus.stringValue = "An IPv4 address uses IPP port 631 and the /ipp/print path."
        case .discovered(let printer):
            discoveryStatus.stringValue = printer.airPrint ? "AirPrint available for iPhone and iPad." :
                "Printer found on Ethernet."
        }
        if focusManual && printerChoice == .manual {
            window.makeFirstResponder(printerAddress)
        }
        updateControls()
        resize()
    }

    func controlTextDidChange(_ obj: Notification) {
        updateControls()
    }

    private func setupSettings() {
        settings.orientation = .vertical
        settings.alignment = .leading
        settings.detachesHiddenViews = true
        settings.spacing = 8
        wifiAddress.placeholderString = "Automatic (Wi-Fi adapter)"
        ethernetAddress.placeholderString = "Automatic (Ethernet adapter)"
        let networkHelp = NSTextField(wrappingLabelWithString:
            "Leave the Mac IPs blank unless you need to select a specific adapter."
        )
        networkHelp.font = .systemFont(ofSize: 12)
        networkHelp.textColor = .secondaryLabelColor
        routing.addItems(withTitles: ["Use current routing", "Use an Ethernet router", "Use a direct Ethernet connection"])
        routing.setAccessibilityLabel("Ethernet routing")
        routing.target = self
        routing.action = #selector(routeChanged)

        routeFields.orientation = .vertical
        routeFields.alignment = .leading
        routeFields.spacing = 7
        let addressRow = textRow(routeLabel, field: routeAddress)
        routeFields.addArrangedSubview(addressRow)
        routeFields.addArrangedSubview(routeHelp)
        addressRow.widthAnchor.constraint(equalTo: routeFields.widthAnchor).isActive = true
        routeHelp.widthAnchor.constraint(equalTo: routeFields.widthAnchor).isActive = true
        routeHelp.font = .systemFont(ofSize: 12)
        routeHelp.textColor = .secondaryLabelColor

        [
            networkHelp,
            textRow("Mac Wi-Fi IPv4", field: wifiAddress),
            textRow("Mac Ethernet IPv4", field: ethernetAddress),
            textRow(NSTextField(labelWithString: "Ethernet routing"), field: routing),
            routeFields,
        ].forEach { item in
            settings.addArrangedSubview(item)
            item.widthAnchor.constraint(equalTo: settings.widthAnchor).isActive = true
        }
        routeFields.isHidden = true
    }

    private func setupLog() {
        log.isEditable = false
        log.isRichText = false
        log.isVerticallyResizable = true
        log.isHorizontallyResizable = false
        log.autoresizingMask = [.width]
        log.font = .monospacedSystemFont(ofSize: 11, weight: .regular)
        log.textContainerInset = NSSize(width: 7, height: 7)
        log.textContainer?.widthTracksTextView = true
        log.setAccessibilityLabel("Installer details")
        logScroll.documentView = log
        logScroll.borderType = .bezelBorder
        logScroll.hasVerticalScroller = true
        logScroll.heightAnchor.constraint(equalToConstant: 118).isActive = true
    }

    private func setupButtons() {
        settingsButton.target = self
        settingsButton.action = #selector(toggleSettings)
        settingsButton.bezelStyle = .rounded
        installButton.target = self
        installButton.action = #selector(beginInstall)
        installButton.bezelStyle = .rounded
        installButton.keyEquivalent = "\r"
        progress.style = .spinning
        progress.controlSize = .small
        progress.isDisplayedWhenStopped = false
        progress.setAccessibilityLabel("Installing Wi-Fi Print Bridge")
        progress.widthAnchor.constraint(equalToConstant: 18).isActive = true
        progress.heightAnchor.constraint(equalToConstant: 18).isActive = true
        let spacer = NSView()
        spacer.setContentHuggingPriority(.defaultLow, for: .horizontal)
        let row = NSStackView(views: [settingsButton, spacer, progress, installButton])
        row.orientation = .horizontal
        row.alignment = .centerY
        row.spacing = 10
        append(row)
    }

    private func resize() {
        let height: CGFloat = (installed ? 195 : 276) +
            (printerSettings.isHidden || manualPrinterRow.isHidden ? 0 : 29) +
            (settings.isHidden ? 0 : 153) +
            (settings.isHidden || routeFields.isHidden ? 0 : 58) +
            (logScroll.isHidden ? 0 : 130)
        let old = window.frame
        let new = window.frameRect(forContentRect: NSRect(x: 0, y: 0, width: width, height: height))
        let visibleBottom = window.screen?.visibleFrame.minY ?? 0
        window.setFrame(
            NSRect(x: old.minX, y: max(visibleBottom, old.maxY - new.height),
                   width: new.width, height: new.height),
            display: true
        )
    }

    @objc private func toggleSettings() {
        settings.isHidden.toggle()
        settingsButton.title = settings.isHidden ? "Network Settings…" : "Hide Network Settings"
        resize()
    }

    @objc private func routeChanged() {
        let index = routing.indexOfSelectedItem
        routeFields.isHidden = index == 0
        if index == 1 {
            routeLabel.stringValue = "Ethernet router IP"
            routeAddress.placeholderString = "Router address on the Mac’s Ethernet network"
            routeHelp.stringValue = "Use the address of the Ethernet-side router that can reach the printer."
        } else if index == 2 {
            routeLabel.stringValue = "Reserved Ethernet IP"
            routeAddress.placeholderString = "Unused address from the printer’s subnet"
            routeHelp.stringValue = "Reserve an address that is unused on both networks."
        }
        routeAddress.stringValue = ""
        routeAddress.setAccessibilityLabel(routeLabel.stringValue)
        resize()
    }

    private func installOptions() -> [String]? {
        var options = ["--windows-lan", windowsLAN.state == .on ? "on" : "off"]
        switch printerChoice {
        case .placeholder:
            status.stringValue = "Select a printer or enter its address."
            status.textColor = .systemRed
            window.makeFirstResponder(printerPicker)
            return nil
        case .installed:
            return options
        case .manual:
            guard let address = manualIPPAddress(printerAddress.stringValue) else {
                status.stringValue = "Enter the printer’s IPv4 address or its plain IPP URL."
                status.textColor = .systemRed
                window.makeFirstResponder(printerAddress)
                return nil
            }
            options.append(contentsOf: ["--backend", address])
        case .discovered(let printer):
            options.append(contentsOf: ["--backend", printer.uri, "--ethernet-interface", printer.interface])
        }
        for (field, flag) in [(wifiAddress, "--wifi-ip"), (ethernetAddress, "--ethernet-ip")] {
            let value = field.stringValue.trimmingCharacters(in: .whitespacesAndNewlines)
            if !value.isEmpty {
                options.append(contentsOf: [flag, value])
            }
        }
        let route = routing.indexOfSelectedItem
        if route > 0 {
            let address = routeAddress.stringValue.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !address.isEmpty else {
                status.stringValue = route == 1 ? "Enter the Ethernet router’s IPv4 address." :
                    "Enter a reserved IPv4 address from the printer’s subnet."
                status.textColor = .systemRed
                if settings.isHidden { toggleSettings() }
                window.makeFirstResponder(routeAddress)
                return nil
            }
            options.append(contentsOf: [route == 1 ? "--gateway" : "--alias", address])
        }
        return options
    }

    @objc private func beginInstall() {
        if installed {
            NSApp.terminate(nil)
            return
        }
        guard !busy, !scanning else { return }
        window.makeFirstResponder(nil)
        guard let options = installOptions() else { return }
        guard let script = installerScript else {
            finish(.failed("The installer files are missing. Open a fresh copy of the installer app."))
            return
        }
        setBusy(true)
        status.stringValue = "Waiting for macOS approval, then checking the printer and installing the bridge…"
        status.textColor = .secondaryLabelColor
        logScroll.isHidden = true
        resize()

        DispatchQueue.global(qos: .userInitiated).async {
            let result = runInstaller(at: script, options: options)
            DispatchQueue.main.async {
                self.finish(result)
            }
        }
    }

    private func setBusy(_ running: Bool) {
        busy = running
        updateControls()
    }

    private func updateControls() {
        let ready = !busy && !scanning && !installed
        [printerPicker, rescan].forEach { $0.isEnabled = ready }
        [windowsLAN, wifiAddress, ethernetAddress, printerAddress, routing, routeAddress]
            .forEach { $0.isEnabled = !busy }
        settingsButton.isEnabled = !busy && printerChoice != .installed
        let manualEmpty = printerAddress.stringValue.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
        installButton.isEnabled = installed ||
            (ready && printerChoice != .placeholder && (printerChoice != .manual || !manualEmpty))
        if busy || scanning {
            progress.setAccessibilityLabel(scanning ? "Searching for Ethernet printers" : "Installing Wi-Fi Print Bridge")
            progress.startAnimation(nil)
        } else {
            progress.stopAnimation(nil)
        }
    }

    private func finish(_ result: InstallResult) {
        setBusy(false)
        switch result {
        case .cancelled:
            status.stringValue = "Installation was canceled. Click Install Bridge to try again."
            status.textColor = .secondaryLabelColor
        case .installed(let details):
            installed = true
            let rawAvailable = details.contains("Windows RAW printing is available")
            var platforms = [String]()
            if details.contains("AirPrint: true") { platforms.append(contentsOf: ["iPhone", "iPad", "Mac"]) }
            if details.contains("Windows IPP: true") || (windowsLAN.state == .on && rawAvailable) {
                platforms.append("Windows")
            }
            status.stringValue = platforms.isEmpty ? "Bridge ready. See the connection details below." :
                "Bridge ready for \(ListFormatter.localizedString(byJoining: platforms))."
            if windowsLAN.state == .on && rawAvailable {
                status.stringValue += " For Windows RAW printing, use the Mac’s Wi-Fi address and port 9100."
            }
            status.textColor = .labelColor
            settings.isHidden = true
            printerSettings.isHidden = true
            windowsLAN.isHidden = true
            log.string = details
            logScroll.isHidden = details.isEmpty
            settingsButton.isHidden = true
            installButton.title = "Done"
            window.makeFirstResponder(installButton)
        case .failed(let details):
            status.stringValue = "Installation could not finish. See the details below."
            status.textColor = .systemRed
            log.string = details.isEmpty ? "The installer did not return any details." : details
            logScroll.isHidden = false
            if printerChoice != .installed {
                settings.isHidden = false
                settingsButton.title = "Hide Network Settings"
            }
        }
        updateControls()
        resize()
    }
}

@main
private enum EntryPoint {
    @MainActor
    static func main() {
        let application = NSApplication.shared
        let installer = InstallerWindow()
        application.delegate = installer
        application.setActivationPolicy(.regular)
        withExtendedLifetime(installer) {
            application.run()
        }
    }
}
