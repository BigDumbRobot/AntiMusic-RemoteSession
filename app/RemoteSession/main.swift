// RemoteSession: a small window over the rsession command-line tool.
// Studio tab: goes online, shows the pairing code to copy and send.
// Send tab: paste a studio's code, pick what to record, record.
import AppKit
import SwiftUI

// MARK: - Running rsession

enum Tool {
    /// rsession and its helpers ship in the app's Resources/bin; RS_BIN
    /// points at a build folder during development.
    static func url(_ name: String) -> URL? {
        var dirs: [URL] = []
        if let r = Bundle.main.resourceURL { dirs.append(r.appendingPathComponent("bin")) }
        if let d = ProcessInfo.processInfo.environment["RS_BIN"] { dirs.append(URL(fileURLWithPath: d)) }
        for d in dirs {
            let u = d.appendingPathComponent(name)
            if FileManager.default.isExecutableFile(atPath: u.path) { return u }
        }
        return nil
    }

    /// Runs rsession to completion and returns its stdout.
    static func output(_ args: [String]) -> String {
        guard let exe = url("rsession") else { return "" }
        let p = Process()
        p.executableURL = exe
        p.arguments = args
        let out = Pipe()
        p.standardOutput = out
        p.standardError = Pipe()
        do { try p.run() } catch { return "" }
        let data = out.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        return String(decoding: data, as: UTF8.self)
    }
}

/// Splits a byte stream into lines.
final class Lines {
    private var buf = Data()
    private let onLine: (String) -> Void
    init(_ onLine: @escaping (String) -> Void) { self.onLine = onLine }
    func attach(_ pipe: Pipe) {
        pipe.fileHandleForReading.readabilityHandler = { [weak self] h in
            let d = h.availableData
            if d.isEmpty { h.readabilityHandler = nil; return }
            self?.feed(d)
        }
    }
    private func feed(_ d: Data) {
        buf.append(d)
        while let i = buf.firstIndex(of: 10) {
            let line = String(decoding: buf[buf.startIndex..<i], as: UTF8.self)
            buf.removeSubrange(buf.startIndex...i)
            onLine(line)
        }
    }
}

/// A long-running rsession process with line callbacks on the main thread.
final class Job {
    let process = Process()
    private var outLines: Lines?
    private var errLines: Lines?
    var stopping = false

    init?(_ args: [String], stdout: @escaping (String) -> Void, stderr: @escaping (String) -> Void,
          exit: @escaping (Int32, Bool) -> Void) {
        guard let exe = Tool.url("rsession") else { return nil }
        process.executableURL = exe
        process.arguments = args
        let o = Pipe(), e = Pipe()
        process.standardOutput = o
        process.standardError = e
        outLines = Lines { l in DispatchQueue.main.async { stdout(l) } }
        errLines = Lines { l in DispatchQueue.main.async { stderr(l) } }
        outLines?.attach(o)
        errLines?.attach(e)
        process.terminationHandler = { [weak self] p in
            let stopped = self?.stopping ?? true
            DispatchQueue.main.async { exit(p.terminationStatus, stopped) }
        }
        do { try process.run() } catch { return nil }
    }

    /// Asks rsession to stop cleanly (it removes the router port, finishes
    /// files) and waits up to `seconds`.
    func stop(waiting seconds: Double = 0) {
        stopping = true
        if process.isRunning { process.interrupt() }
        let deadline = Date().addingTimeInterval(seconds)
        while process.isRunning && Date() < deadline { usleep(50_000) }
    }
}

func copyToClipboard(_ s: String) {
    NSPasteboard.general.clearContents()
    NSPasteboard.general.setString(s, forType: .string)
}

let musicFolder = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Music/RemoteSession")

// MARK: - Studio (receive)

struct Take: Identifiable {
    let id: String
    var state: String
    var detail = ""
    var dir = ""
}

final class Studio: ObservableObject {
    @Published var code: String?
    @Published var reach = "starting"
    @Published var detail = "Getting online…"
    @Published var takes: [Take] = []
    @Published var problem: String?
    @Published var importIntoProTools = true {
        didSet { if oldValue != importIntoProTools { restart() } }
    }
    private var job: Job?
    private var lastError = ""
    let folder = musicFolder.appendingPathComponent("Received")

    func start() {
        guard job == nil else { return }
        problem = nil
        try? FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
        var args = ["receive", "-events", "-deliver", "-dir", folder.path]
        if importIntoProTools { args.append("-import") }
        job = Job(args, stdout: { [weak self] in self?.event($0) },
                  stderr: { [weak self] l in if l.hasPrefix("rsession: ") { self?.lastError = String(l.dropFirst(10)) } },
                  exit: { [weak self] status, stopped in
                      guard let self else { return }
                      self.job = nil
                      if !stopped {
                          self.reach = "offline"
                          self.problem = self.lastError.isEmpty ? "The studio stopped unexpectedly (\(status))." : self.lastError
                      }
                  })
        if job == nil {
            reach = "offline"
            problem = "Could not start rsession. Is the app complete?"
        }
    }

    func stop(waiting seconds: Double = 0) {
        job?.stop(waiting: seconds)
        job = nil
    }

    func restart() {
        stop(waiting: 5)
        code = nil
        reach = "starting"
        detail = "Getting online…"
        start()
    }

    private func event(_ line: String) {
        guard let d = line.data(using: .utf8),
              let ev = try? JSONSerialization.jsonObject(with: d) as? [String: Any] else { return }
        switch ev["type"] as? String {
        case "code":
            code = ev["code"] as? String
            reach = ev["reach"] as? String ?? "internet"
            detail = ev["detail"] as? String ?? ""
        case "take":
            let name = ev["take"] as? String ?? "?"
            var t = takes.first(where: { $0.id == name }) ?? Take(id: name, state: "")
            t.dir = ev["dir"] as? String ?? t.dir
            switch ev["state"] as? String {
            case "receiving":
                t.state = "Receiving…"
                t.detail = ""
            case "done":
                t.state = (ev["imported"] as? Bool ?? false) ? "Imported into Pro Tools" : "Saved"
                t.detail = ""
            default:
                t.state = "Saved, not imported"
                t.detail = ev["error"] as? String ?? ""
            }
            if let i = takes.firstIndex(where: { $0.id == name }) { takes[i] = t } else { takes.insert(t, at: 0) }
        default:
            break
        }
    }

    var invite: String {
        guard let code else { return "" }
        return """
        Join my RemoteSession studio.

        Studio code: \(code)

        1. Open RemoteSession and choose "Send to a studio".
        2. Paste the code.
        3. Pick your track or input and press Record. Takes land in my session automatically.
        """
    }
}

struct StudioView: View {
    @ObservedObject var studio: Studio
    @State private var copied = ""

    var dotColor: Color {
        switch studio.reach {
        case "internet", "relay": return .green
        case "lan": return .orange
        case "offline": return .red
        default: return .gray
        }
    }

    var body: some View {
        VStack(spacing: 16) {
            Text("Your studio code").font(.headline).foregroundColor(.secondary)
            Text(studio.code ?? "Getting online…")
                .font(.system(size: studio.code == nil ? 22 : 40, weight: .bold, design: .monospaced))
                .textSelection(.enabled)
                .padding(.vertical, 18).padding(.horizontal, 28)
                .frame(maxWidth: .infinity)
                .background(RoundedRectangle(cornerRadius: 14).fill(Color.secondary.opacity(0.12)))
            HStack(spacing: 12) {
                Button {
                    copyToClipboard(studio.code ?? ""); copied = "Code copied. Paste it into a message."
                } label: { Label("Copy code", systemImage: "doc.on.doc").frame(minWidth: 130) }
                    .buttonStyle(.borderedProminent).controlSize(.large)
                    .keyboardShortcut("c", modifiers: [.command, .shift])
                Button {
                    copyToClipboard(studio.invite); copied = "Invite copied: code plus instructions."
                } label: { Label("Copy invite", systemImage: "envelope").frame(minWidth: 130) }
                    .controlSize(.large)
            }
            .disabled(studio.code == nil)
            Text(copied).font(.callout).foregroundColor(.secondary).frame(height: 16)

            HStack(alignment: .top, spacing: 8) {
                Circle().fill(dotColor).frame(width: 10, height: 10).padding(.top, 4)
                VStack(alignment: .leading, spacing: 4) {
                    Text(studio.problem ?? statusTitle).font(.callout.weight(.semibold))
                    if studio.problem == nil && !studio.detail.isEmpty {
                        Text(studio.detail).font(.caption).foregroundColor(.secondary).fixedSize(horizontal: false, vertical: true)
                    }
                }
                Spacer()
                if studio.problem != nil { Button("Try again") { studio.restart() } }
            }

            Divider()
            HStack {
                Text("Takes").font(.headline)
                Spacer()
                Button("Show in Finder") { NSWorkspace.shared.open(studio.folder) }
            }
            if studio.takes.isEmpty {
                Text("Takes from musicians appear here as they record.")
                    .font(.callout).foregroundColor(.secondary).frame(maxWidth: .infinity, alignment: .leading)
            } else {
                ScrollView {
                    VStack(alignment: .leading, spacing: 8) {
                        ForEach(studio.takes) { t in
                            VStack(alignment: .leading, spacing: 2) {
                                HStack {
                                    Text(t.id).font(.body.monospaced())
                                    Spacer()
                                    Text(t.state).foregroundColor(t.state.hasPrefix("Receiving") ? .accentColor : .secondary)
                                }
                                if !t.detail.isEmpty { Text(t.detail).font(.caption).foregroundColor(.orange) }
                            }
                        }
                    }
                }.frame(maxHeight: 140)
            }
            Toggle("Import takes into the open Pro Tools session", isOn: $studio.importIntoProTools)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
        .padding(24)
        .onAppear { studio.start() }
    }

    var statusTitle: String {
        switch studio.reach {
        case "internet": return "Online: musicians can reach you from anywhere"
        case "relay": return "Online through a relay"
        case "lan": return "Only reachable on this network"
        case "offline": return "Offline"
        default: return "Starting…"
        }
    }
}

// MARK: - Send

struct Source: Identifiable, Hashable {
    let id: String
    let label: String
    let args: [String]
}

final class Sender: ObservableObject {
    @Published var code = ""
    @Published var sources: [Source] = []
    @Published var source: Source?
    @Published var recording = false
    @Published var finishing = false
    @Published var status = ""
    private var job: Job?
    private var lastLine = ""

    var codeLooksValid: Bool {
        let s = code.uppercased().filter { $0.isLetter || $0.isNumber }
        return s.count == 18 || s.count == 12
    }

    func refresh() {
        var list: [Source] = []
        // Link plugin instances, in any DAW.
        for line in Tool.output(["links"]).split(separator: "\n") {
            let f = line.split(separator: "\t").map(String.init)
            guard f.count >= 6, f[0] == "active" else { continue }
            let track = f[1].trimmingCharacters(in: CharacterSet(charactersIn: "\""))
            let host = f[2]
            list.append(Source(id: "link:" + f[5], label: "Track \"\(track)\" in \(host) (Link plugin)", args: ["-link", f[5]]))
        }
        // Audio inputs, placed with Pro Tools' transport.
        for line in Tool.output(["devices"]).split(separator: "\n") {
            guard let d = line.data(using: .utf8),
                  let o = try? JSONSerialization.jsonObject(with: d) as? [String: Any],
                  let name = o["name"] as? String, let uid = o["uid"] as? String else { continue }
            list.append(Source(id: "dev:" + uid, label: "\(name) (input, Pro Tools transport)", args: ["-device", uid, "-ptsl"]))
        }
        sources = list
        if source == nil || !list.contains(where: { $0 == source }) {
            source = list.first(where: { $0.id.hasPrefix("link:") }) ?? list.first(where: { $0.label.contains("Audio Bridge") }) ?? list.first
        }
    }

    func record() {
        guard let source, codeLooksValid else { return }
        let f = DateFormatter()
        f.dateFormat = "yyyy-MM-dd_HH-mm-ss"
        let dir = musicFolder.appendingPathComponent("Sent").appendingPathComponent("take_" + f.string(from: Date()))
        try? FileManager.default.createDirectory(at: dir.deletingLastPathComponent(), withIntermediateDirectories: true)
        status = "Connecting to the studio…"
        lastLine = ""
        job = Job(["record", "-dir", dir.path, "-code", code] + source.args,
                  stdout: { _ in },
                  stderr: { [weak self] l in self?.line(l) },
                  exit: { [weak self] status, _ in
                      guard let self else { return }
                      self.job = nil
                      self.recording = false
                      self.finishing = false
                      if status == 0 {
                          self.status = "Done. The studio has the take."
                      } else {
                          self.status = self.lastLine.isEmpty ? "Stopped (\(status))." : self.lastLine
                      }
                  })
        recording = job != nil
        if job == nil { status = "Could not start rsession. Is the app complete?" }
    }

    func stop() {
        guard let job else { return }
        finishing = true
        status = "Finishing the upload…"
        job.stopping = true
        job.process.interrupt()
    }

    private func line(_ l: String) {
        let text = l.replacingOccurrences(of: #"^\d\d:\d\d:\d\d "#, with: "", options: .regularExpression)
        if text.hasPrefix("rsession: ") { lastLine = String(text.dropFirst(10)); return }
        if text.contains("send: connected") { status = recording && !finishing ? "Recording. Connected to the studio." : status }
        if text.contains("retrying") { status = "Studio not reachable yet; still recording locally and retrying…" }
        if text.contains("verified copy") { status = "Done. The studio has the take." }
    }
}

struct SendView: View {
    @ObservedObject var sender: Sender

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("Studio code").font(.headline)
            HStack {
                TextField("Paste the code the studio sent you", text: $sender.code)
                    .font(.system(size: 20, weight: .semibold, design: .monospaced))
                    .textFieldStyle(.roundedBorder)
                    .disabled(sender.recording)
                Button("Paste") { sender.code = NSPasteboard.general.string(forType: .string)?.trimmingCharacters(in: .whitespacesAndNewlines) ?? "" }
                    .disabled(sender.recording)
            }
            if !sender.code.isEmpty && !sender.codeLooksValid {
                Text("That doesn't look like a studio code (it looks like 8RC4TK-8WX0K7-GDZCFA).").font(.caption).foregroundColor(.orange)
            }
            Text("Record from").font(.headline)
            HStack {
                Picker("", selection: $sender.source) {
                    ForEach(sender.sources) { s in Text(s.label).tag(Optional(s)) }
                }.labelsHidden().disabled(sender.recording)
                Button("Refresh") { sender.refresh() }.disabled(sender.recording)
            }
            Text("Any DAW: put the RemoteSession Link plugin on the track you want to send. Pro Tools can also send through Audio Bridge.")
                .font(.caption).foregroundColor(.secondary).fixedSize(horizontal: false, vertical: true)
            HStack {
                if sender.recording {
                    Button { sender.stop() } label: { Label(sender.finishing ? "Finishing…" : "Stop", systemImage: "stop.fill").frame(minWidth: 120) }
                        .buttonStyle(.borderedProminent).controlSize(.large).disabled(sender.finishing)
                } else {
                    Button { sender.record() } label: { Label("Record", systemImage: "record.circle").frame(minWidth: 120) }
                        .buttonStyle(.borderedProminent).tint(.red).controlSize(.large)
                        .disabled(!sender.codeLooksValid || sender.source == nil)
                }
                Spacer()
            }
            Text(sender.status).font(.callout).foregroundColor(.secondary).fixedSize(horizontal: false, vertical: true)
            Spacer()
        }
        .padding(24)
        .onAppear { sender.refresh() }
    }
}

// MARK: - App

final class AppDelegate: NSObject, NSApplicationDelegate {
    var onQuit: (() -> Void)?
    func applicationWillTerminate(_ notification: Notification) { onQuit?() }
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { true }
}

@main
struct RemoteSessionApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) var delegate
    @StateObject private var studio = Studio()
    @StateObject private var sender = Sender()

    var body: some Scene {
        WindowGroup("RemoteSession") {
            TabView {
                StudioView(studio: studio).tabItem { Text("Studio") }
                SendView(sender: sender).tabItem { Text("Send to a studio") }
            }
            .frame(minWidth: 560, minHeight: 560)
            .onAppear {
                delegate.onQuit = { [studio, sender] in
                    sender.stop()
                    studio.stop(waiting: 5) // closes the router port
                }
            }
        }
        .windowResizability(.contentMinSize)
    }
}
