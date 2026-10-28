// RemoteSession: a window over the rsession command-line tool.
// Studio: host attended sessions (code, chat, live listen, stems in, prints out).
// Join: join a studio (code, listen, chat, send stems, record live, prints back).
import AppKit
import SwiftUI
import UniformTypeIdentifiers

// MARK: - Running rsession

enum Tool {
    /// rsession and its helpers ship in Resources/bin; RS_BIN points at a
    /// build folder during development.
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

    static func devices() -> [(name: String, uid: String)] {
        Tool.output(["devices"]).split(separator: "\n").compactMap { line in
            guard let d = line.data(using: .utf8),
                  let o = try? JSONSerialization.jsonObject(with: d) as? [String: Any],
                  let name = o["name"] as? String, let uid = o["uid"] as? String else { return nil }
            return (name, uid)
        }
    }
}

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

/// A long-running rsession process: JSON events in, JSON commands out.
final class Job {
    let process = Process()
    private let stdin = Pipe()
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
        process.standardInput = stdin
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

    func send(_ cmd: [String: Any]) {
        guard process.isRunning, var d = try? JSONSerialization.data(withJSONObject: cmd) else { return }
        d.append(10)
        try? stdin.fileHandleForWriting.write(contentsOf: d)
    }

    func stop(waiting seconds: Double = 0) {
        stopping = true
        if process.isRunning { process.interrupt() }
        let deadline = Date().addingTimeInterval(seconds)
        while process.isRunning && Date() < deadline { usleep(50_000) }
    }
}

func parse(_ line: String) -> [String: Any]? {
    guard let d = line.data(using: .utf8) else { return nil }
    return try? JSONSerialization.jsonObject(with: d) as? [String: Any]
}

func copyToClipboard(_ s: String) {
    NSPasteboard.general.clearContents()
    NSPasteboard.general.setString(s, forType: .string)
}

func pickWAVs(_ prompt: String) -> [String] {
    let p = NSOpenPanel()
    p.allowsMultipleSelection = true
    p.canChooseDirectories = false
    p.allowedContentTypes = [.wav]
    p.prompt = prompt
    return p.runModal() == .OK ? p.urls.map(\.path) : []
}

let musicFolder = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Music/RemoteSession")

struct ChatLine: Identifiable {
    let id = UUID()
    let mine: Bool
    let text: String
}

// MARK: - Studio (engineer)

struct Take: Identifiable {
    let id: String
    var state: String
    var detail = ""
}

final class Studio: ObservableObject {
    @Published var code: String?
    @Published var reach = "starting"
    @Published var detail = "Getting online…"
    @Published var takes: [Take] = []
    @Published var problem: String?
    @Published var client: String?
    @Published var chat: [ChatLine] = []
    @Published var printsStatus = ""
    @Published var devices: [(name: String, uid: String)] = []
    @Published var listenDevice: String = UserDefaults.standard.string(forKey: "listenDevice") ?? "" {
        didSet {
            UserDefaults.standard.set(listenDevice, forKey: "listenDevice")
            if oldValue != listenDevice { restart() }
        }
    }
    @Published var importIntoProTools = true {
        didSet { if oldValue != importIntoProTools { restart() } }
    }
    @Published var timerRunning = false
    @Published var elapsed: TimeInterval = 0
    private var timerStart: Date?
    private var ticker: Timer?
    private var job: Job?
    private var lastError = ""
    let folder = musicFolder.appendingPathComponent("Received")

    func start() {
        guard job == nil else { return }
        problem = nil
        if devices.isEmpty { devices = Tool.devices() }
        if listenDevice.isEmpty, let b = devices.first(where: { $0.name.contains("Audio Bridge 2") }) { listenDevice = b.name }
        try? FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
        var args = ["receive", "-events", "-deliver", "-dir", folder.path, "-name", Host.current().localizedName ?? "Studio"]
        if importIntoProTools { args.append("-import") }
        if !listenDevice.isEmpty { args += ["-listen-device", listenDevice] }
        job = Job(args, stdout: { [weak self] in self?.event($0) },
                  stderr: { [weak self] l in if l.hasPrefix("rsession: ") { self?.lastError = String(l.dropFirst(10)) } },
                  exit: { [weak self] status, stopped in
                      guard let self else { return }
                      self.job = nil
                      self.client = nil
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
        guard job != nil || problem != nil else { return }
        stop(waiting: 5)
        code = nil
        reach = "starting"
        detail = "Getting online…"
        start()
    }

    func say(_ text: String) {
        let t = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !t.isEmpty else { return }
        job?.send(["cmd": "chat", "text": t])
    }

    func sendPrints() {
        let files = pickWAVs("Send prints")
        guard !files.isEmpty else { return }
        printsStatus = "Packing \(files.count) print(s)…"
        job?.send(["cmd": "send_prints", "files": files])
    }

    func toggleTimer() {
        if timerRunning {
            ticker?.invalidate()
            timerRunning = false
        } else {
            timerStart = Date().addingTimeInterval(-elapsed)
            ticker = Timer.scheduledTimer(withTimeInterval: 1, repeats: true) { [weak self] _ in
                guard let self, let s = self.timerStart else { return }
                self.elapsed = Date().timeIntervalSince(s)
            }
            timerRunning = true
        }
    }

    private func event(_ line: String) {
        guard let ev = parse(line) else { return }
        switch ev["type"] as? String {
        case "code":
            code = ev["code"] as? String
            reach = ev["reach"] as? String ?? "internet"
            detail = ev["detail"] as? String ?? ""
        case "client":
            client = (ev["connected"] as? Bool ?? false) ? (ev["name"] as? String ?? "Client") : nil
        case "chat":
            chat.append(ChatLine(mine: (ev["from"] as? String) == "engineer", text: ev["text"] as? String ?? ""))
        case "prints":
            switch ev["state"] as? String {
            case "sending": printsStatus = "Sending prints…"
            case "delivered": printsStatus = "Prints delivered to the client."
            default: printsStatus = "Prints not sent: " + (ev["error"] as? String ?? "unknown error")
            }
        case "take":
            let name = ev["take"] as? String ?? "?"
            var t = takes.first(where: { $0.id == name }) ?? Take(id: name, state: "")
            switch ev["state"] as? String {
            case "receiving": t.state = "Receiving…"; t.detail = ""
            case "done": t.state = (ev["imported"] as? Bool ?? false) ? "In Pro Tools" : "Saved"; t.detail = ""
            default: t.state = "Saved, not imported"; t.detail = ev["error"] as? String ?? ""
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

        1. Open RemoteSession and choose "Join a studio".
        2. Paste the code and press Join.
        3. Listen in, send your stems, and get prints back in your session.
        """
    }
}

// MARK: - Join (client)

struct Source: Identifiable, Hashable {
    let id: String
    let label: String
    let args: [String]
}

final class Client: ObservableObject {
    @Published var code = UserDefaults.standard.string(forKey: "lastCode") ?? ""
    @Published var joined = false
    @Published var studio: String?
    @Published var status = ""
    @Published var listening = false
    @Published var chat: [ChatLine] = []
    @Published var prints: [Take] = []
    @Published var stemsStatus = ""
    @Published var importIntoProTools = true
    // Live recording
    @Published var sources: [Source] = []
    @Published var source: Source?
    @Published var recording = false
    @Published var recordStatus = ""
    private var job: Job?
    private var rec: Job?
    private var lastError = ""
    let folder = musicFolder.appendingPathComponent("Prints")

    var codeLooksValid: Bool {
        let s = code.uppercased().filter { $0.isLetter || $0.isNumber }
        return s.count == 18 || s.count == 12
    }

    func join() {
        guard codeLooksValid, job == nil else { return }
        UserDefaults.standard.set(code, forKey: "lastCode")
        try? FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
        status = "Connecting…"
        lastError = ""
        var args = ["join", "-events", "-code", code, "-dir", folder.path, "-name", NSFullUserName()]
        if importIntoProTools { args.append("-import") }
        job = Job(args, stdout: { [weak self] in self?.event($0) },
                  stderr: { [weak self] l in if l.hasPrefix("rsession: ") { self?.lastError = String(l.dropFirst(10)) } },
                  exit: { [weak self] _, stopped in
                      guard let self else { return }
                      self.job = nil
                      self.joined = false
                      self.studio = nil
                      self.listening = false
                      if !stopped { self.status = self.lastError.isEmpty ? "Disconnected." : self.lastError }
                  })
        joined = job != nil
        if job == nil { status = "Could not start rsession. Is the app complete?" }
    }

    func leave() {
        job?.stop(waiting: 3)
        job = nil
        joined = false
        studio = nil
        listening = false
        status = "Left the session."
    }

    func setListening(_ on: Bool) {
        listening = on
        job?.send(["cmd": "listen", "on": on])
    }

    func say(_ text: String) {
        let t = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !t.isEmpty else { return }
        job?.send(["cmd": "chat", "text": t])
    }

    func sendStems() {
        let files = pickWAVs("Send stems")
        guard !files.isEmpty else { return }
        stemsStatus = "Sending \(files.count) stem(s)…"
        job?.send(["cmd": "send_stems", "files": files])
    }

    private func event(_ line: String) {
        guard let ev = parse(line) else { return }
        switch ev["type"] as? String {
        case "studio":
            if ev["connected"] as? Bool ?? false {
                studio = ev["name"] as? String ?? "Studio"
                status = ""
            } else {
                studio = nil
                status = ev["error"] as? String ?? "Reconnecting…"
            }
        case "chat":
            chat.append(ChatLine(mine: (ev["from"] as? String) == "client", text: ev["text"] as? String ?? ""))
        case "listen":
            if (ev["state"] as? String) == "playing" { listening = true }
            if let e = ev["error"] as? String, !e.isEmpty { status = "Listen: " + e }
        case "stems":
            switch ev["state"] as? String {
            case "sending": stemsStatus = "Sending stems…"
            case "delivered": stemsStatus = "Stems delivered to the studio."
            default: stemsStatus = "Stems not sent: " + (ev["error"] as? String ?? "unknown error")
            }
        case "prints":
            let name = ev["take"] as? String ?? ""
            guard !name.isEmpty else { return }
            var t = prints.first(where: { $0.id == name }) ?? Take(id: name, state: "")
            switch ev["state"] as? String {
            case "receiving": t.state = "Receiving…"
            case "received":
                if ev["imported"] as? Bool ?? false { t.state = "In your session"; t.detail = "" }
                else if let e = ev["error"] as? String { t.state = "Saved, not imported"; t.detail = e }
                else { t.state = "Saved" }
            default: break
            }
            if let i = prints.firstIndex(where: { $0.id == name }) { prints[i] = t } else { prints.insert(t, at: 0) }
        default:
            break
        }
    }

    // Live recording (any DAW via the Link plugin, or an input with Pro Tools transport).
    func refreshSources() {
        var list: [Source] = []
        for line in Tool.output(["links"]).split(separator: "\n") {
            let f = line.split(separator: "\t").map(String.init)
            guard f.count >= 6, f[0] == "active" else { continue }
            let track = f[1].trimmingCharacters(in: CharacterSet(charactersIn: "\""))
            list.append(Source(id: "link:" + f[5], label: "Track \"\(track)\" in \(f[2]) (Link plugin)", args: ["-link", f[5]]))
        }
        for d in Tool.devices() {
            list.append(Source(id: "dev:" + d.uid, label: "\(d.name) (Pro Tools transport)", args: ["-device", d.uid, "-ptsl"]))
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
        recordStatus = "Recording…"
        var lastLine = ""
        rec = Job(["record", "-dir", dir.path, "-code", code] + source.args,
                  stdout: { _ in },
                  stderr: { [weak self] l in
                      if l.hasPrefix("rsession: ") { lastLine = String(l.dropFirst(10)) }
                      if l.contains("retrying") { self?.recordStatus = "Studio not reachable; recording locally and retrying…" }
                  },
                  exit: { [weak self] status, _ in
                      guard let self else { return }
                      self.rec = nil
                      self.recording = false
                      self.recordStatus = status == 0 ? "Take delivered to the studio." : (lastLine.isEmpty ? "Stopped (\(status))." : lastLine)
                  })
        recording = rec != nil
    }

    func stopRecording() {
        guard let rec else { return }
        recordStatus = "Finishing the upload…"
        rec.stopping = true
        rec.process.interrupt()
    }
}

// MARK: - Look: calm and dark, with a nod to vaporwave

enum Neon {
    static let pink = Color(red: 1.0, green: 0.36, blue: 0.62)
    static let cyan = Color(red: 0.35, green: 0.85, blue: 0.92)
    static let purple = Color(red: 0.62, green: 0.5, blue: 0.95)
    static let yellow = Color(red: 1.0, green: 0.8, blue: 0.45)
    static let ice = Color(red: 0.92, green: 0.9, blue: 0.97)
    static let dim = Color(red: 0.62, green: 0.58, blue: 0.7)
    static let panel = Color(red: 0.12, green: 0.1, blue: 0.16)
    static let line = Color.white.opacity(0.08)
    static let sunset = LinearGradient(colors: [pink, purple, cyan], startPoint: .leading, endPoint: .trailing)
}

/// The striped retro sun, small, as the app's mark.
struct SunView: View {
    var body: some View {
        Canvas { ctx, size in
            let r = CGRect(origin: .zero, size: size)
            ctx.clip(to: Path(ellipseIn: r))
            ctx.fill(Path(r), with: .linearGradient(Gradient(colors: [Neon.yellow, Neon.pink]),
                                                     startPoint: .zero, endPoint: CGPoint(x: 0, y: size.height)))
            var y = size.height * 0.5
            var gap = size.height * 0.04
            while y < size.height {
                ctx.blendMode = .destinationOut
                ctx.fill(Path(CGRect(x: 0, y: y, width: size.width, height: gap)), with: .color(.black))
                y += gap + size.height * 0.1
                gap += size.height * 0.02
            }
        }
        .compositingGroup()
    }
}

/// A faint perspective grid fading in at the bottom of the window.
struct GridView: View {
    var body: some View {
        Canvas { ctx, size in
            let horizon = size.height * 0.72
            var lines = Path()
            for i in 1...10 {
                let y = horizon + (size.height - horizon) * pow(CGFloat(i) / 10, 2.2)
                lines.move(to: CGPoint(x: 0, y: y))
                lines.addLine(to: CGPoint(x: size.width, y: y))
            }
            let vx = size.width / 2
            for i in -10...10 {
                lines.move(to: CGPoint(x: vx + CGFloat(i) * 5, y: horizon))
                lines.addLine(to: CGPoint(x: vx + CGFloat(i) * size.width / 8, y: size.height))
            }
            ctx.stroke(lines, with: .linearGradient(Gradient(colors: [Neon.pink.opacity(0), Neon.pink.opacity(0.16)]),
                                                     startPoint: CGPoint(x: 0, y: horizon), endPoint: CGPoint(x: 0, y: size.height)),
                       lineWidth: 1)
        }
        .allowsHitTesting(false)
    }
}

struct VaporBackground: View {
    var body: some View {
        ZStack {
            LinearGradient(colors: [Color(red: 0.07, green: 0.06, blue: 0.1), Color(red: 0.1, green: 0.07, blue: 0.14)],
                           startPoint: .top, endPoint: .bottom)
            GridView()
        }
        .ignoresSafeArea()
    }
}

struct ChromeTitle: View {
    let text: String
    var body: some View {
        VStack(spacing: 6) {
            HStack(spacing: 10) {
                SunView().frame(width: 26, height: 26)
                Text(text).font(.system(size: 24, weight: .semibold)).foregroundColor(Neon.ice)
            }
            Rectangle().fill(Neon.sunset).frame(width: 160, height: 2).cornerRadius(1)
        }
    }
}

struct NeonButton: ButtonStyle {
    var color: Color = Neon.cyan
    var filled = false
    func makeBody(configuration: Configuration) -> some View {
        NeonButtonBody(configuration: configuration, color: color, filled: filled)
    }
}

struct NeonButtonBody: View {
    let configuration: ButtonStyle.Configuration
    let color: Color
    let filled: Bool
    @Environment(\.isEnabled) private var enabled
    var body: some View {
        configuration.label
            .font(.system(size: 13, weight: .semibold))
            .padding(.vertical, 7).padding(.horizontal, 14)
            .foregroundColor(filled ? Color(red: 0.1, green: 0.05, blue: 0.12) : color)
            .background(RoundedRectangle(cornerRadius: 7).fill(filled ? color : Color.white.opacity(0.04)))
            .overlay(RoundedRectangle(cornerRadius: 7).stroke(filled ? .clear : color.opacity(0.55), lineWidth: 1))
            .opacity(configuration.isPressed ? 0.75 : (enabled ? 1 : 0.35))
    }
}

struct Panel<Content: View>: View {
    let title: String
    var accent: Color = Neon.cyan
    @ViewBuilder var content: Content
    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text(title.capitalized)
                .font(.system(size: 12, weight: .semibold))
                .foregroundColor(Neon.dim)
            content
        }
        .padding(16)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 12).fill(Neon.panel))
        .overlay(RoundedRectangle(cornerRadius: 12).stroke(Neon.line, lineWidth: 1))
    }
}

struct GlowText: View {
    let text: String
    var color: Color = Neon.ice
    var size: CGFloat = 13
    var body: some View {
        Text(text).font(.system(size: size))
            .foregroundColor(color).fixedSize(horizontal: false, vertical: true)
    }
}

struct NeonField: View {
    let placeholder: String
    @Binding var text: String
    var size: CGFloat = 14
    var mono = false
    var onSubmit: () -> Void = {}
    var body: some View {
        TextField("", text: $text, prompt: Text(placeholder).foregroundColor(Neon.dim.opacity(0.7)))
            .textFieldStyle(.plain)
            .font(.system(size: size, weight: mono ? .medium : .regular, design: mono ? .monospaced : .default))
            .foregroundColor(Neon.ice)
            .padding(9)
            .background(RoundedRectangle(cornerRadius: 7).fill(Color.black.opacity(0.3)))
            .overlay(RoundedRectangle(cornerRadius: 7).stroke(Neon.line, lineWidth: 1))
            .onSubmit(onSubmit)
    }
}

struct ChatBox: View {
    let lines: [ChatLine]
    let enabled: Bool
    let send: (String) -> Void
    @State private var draft = ""
    var body: some View {
        VStack(spacing: 8) {
            ScrollViewReader { proxy in
                ScrollView {
                    VStack(alignment: .leading, spacing: 6) {
                        ForEach(lines) { l in
                            HStack {
                                if l.mine { Spacer(minLength: 40) }
                                Text(l.text)
                                    .font(.system(size: 13))
                                    .foregroundColor(l.mine ? Color(red: 0.1, green: 0.05, blue: 0.12) : Neon.ice)
                                    .padding(.vertical, 6).padding(.horizontal, 10)
                                    .background(RoundedRectangle(cornerRadius: 10).fill(l.mine ? Neon.cyan.opacity(0.9) : Color.white.opacity(0.08)))
                                if !l.mine { Spacer(minLength: 40) }
                            }.id(l.id)
                        }
                    }
                }
                .frame(height: 110)
                .onChange(of: lines.count) { _ in if let last = lines.last { proxy.scrollTo(last.id) } }
            }
            HStack {
                NeonField(placeholder: enabled ? "Message" : "Chat opens when someone is connected", text: $draft) { submit() }
                    .disabled(!enabled)
                Button("Send") { submit() }.buttonStyle(NeonButton(color: Neon.cyan)).disabled(!enabled || draft.isEmpty)
            }
        }
    }
    private func submit() { send(draft); draft = "" }
}

struct StatusDot: View {
    let color: Color
    var body: some View {
        Circle().fill(color).frame(width: 8, height: 8)
    }
}

// MARK: - Studio view

struct StudioView: View {
    @ObservedObject var studio: Studio
    @State private var copied = ""

    var dot: Color {
        switch studio.reach {
        case "internet", "relay": return Neon.cyan
        case "lan": return Neon.yellow
        case "offline": return Neon.pink
        default: return .gray
        }
    }
    var statusTitle: String {
        switch studio.reach {
        case "internet": return "Online · musicians can reach you from anywhere"
        case "relay": return "Online through a relay"
        case "lan": return "Only reachable on this network"
        case "offline": return "Offline"
        default: return "Starting…"
        }
    }

    var body: some View {
        VStack(spacing: 14) {
            VStack(spacing: 12) {
                Text("Your studio code")
                    .font(.system(size: 13, weight: .medium)).foregroundColor(Neon.dim)
                Text(studio.code ?? "Getting online…")
                    .font(.system(size: studio.code == nil ? 20 : 36, weight: .semibold, design: .monospaced))
                    .foregroundColor(Neon.ice)
                    .textSelection(.enabled)
                    .padding(.vertical, 18)
                    .frame(maxWidth: .infinity)
                    .background(RoundedRectangle(cornerRadius: 12).fill(Neon.panel))
                    .overlay(RoundedRectangle(cornerRadius: 12).stroke(Neon.sunset, lineWidth: 1.5))
                HStack(spacing: 14) {
                    Button { copyToClipboard(studio.code ?? ""); copied = "Code copied · paste it into a message" } label: {
                        Label("Copy code", systemImage: "doc.on.doc")
                    }.buttonStyle(NeonButton(color: Neon.pink, filled: true))
                    Button { copyToClipboard(studio.invite); copied = "Invite copied · code plus instructions" } label: {
                        Label("Copy invite", systemImage: "envelope")
                    }.buttonStyle(NeonButton(color: Neon.cyan))
                }.disabled(studio.code == nil)
                GlowText(text: copied, color: Neon.dim, size: 12).frame(height: 14)
                HStack(alignment: .top, spacing: 8) {
                    StatusDot(color: dot).padding(.top, 3)
                    VStack(alignment: .leading, spacing: 3) {
                        GlowText(text: studio.problem ?? statusTitle, color: .white)
                        if studio.problem == nil && !studio.detail.isEmpty { GlowText(text: studio.detail, color: Neon.dim, size: 11) }
                    }
                    Spacer()
                    if studio.problem != nil { Button("Try again") { studio.restart() }.buttonStyle(NeonButton(color: Neon.yellow)) }
                }
            }

            Panel(title: "SESSION", accent: Neon.pink) {
                HStack {
                    StatusDot(color: studio.client == nil ? .gray : Neon.cyan)
                    GlowText(text: studio.client.map { "\($0) is in your session" } ?? "Waiting for a client to join…", color: .white)
                    Spacer()
                    Text(timeString(studio.elapsed))
                        .font(.system(size: 16, weight: .medium, design: .monospaced)).foregroundColor(Neon.ice)
                    Button(studio.timerRunning ? "Pause" : "Start clock") { studio.toggleTimer() }
                        .buttonStyle(NeonButton(color: Neon.yellow))
                }
                HStack {
                    GlowText(text: "Client hears:", color: Neon.ice.opacity(0.8), size: 12)
                    Picker("", selection: $studio.listenDevice) {
                        Text("Nothing (listening off)").tag("")
                        ForEach(studio.devices, id: \.uid) { d in Text(d.name).tag(d.name) }
                    }.labelsHidden().frame(maxWidth: 280)
                    Spacer()
                    Button { studio.sendPrints() } label: { Label("Send prints", systemImage: "paperplane.fill") }
                        .buttonStyle(NeonButton(color: Neon.pink, filled: true)).disabled(studio.client == nil)
                }
                if !studio.printsStatus.isEmpty { GlowText(text: studio.printsStatus, color: Neon.yellow, size: 12) }
                ChatBox(lines: studio.chat, enabled: studio.client != nil) { studio.say($0) }
            }

            Panel(title: "INCOMING", accent: Neon.cyan) {
                if studio.takes.isEmpty {
                    GlowText(text: "Stems and takes from your client land here and in your session.", color: Neon.dim, size: 12)
                } else {
                    ForEach(studio.takes.prefix(4)) { t in
                        HStack {
                            Text(t.id).font(.system(size: 12, weight: .semibold, design: .monospaced)).foregroundColor(Neon.ice)
                            Spacer()
                            GlowText(text: t.state, color: t.state.hasPrefix("Receiving") ? Neon.cyan : Neon.dim, size: 12)
                        }
                        if !t.detail.isEmpty { GlowText(text: t.detail, color: Neon.yellow, size: 11) }
                    }
                }
                HStack {
                    Toggle("Import into the open Pro Tools session", isOn: $studio.importIntoProTools)
                        .toggleStyle(.switch).tint(Neon.pink).foregroundColor(Neon.dim)
                    Spacer()
                    Button("Show in Finder") { NSWorkspace.shared.open(studio.folder) }.buttonStyle(NeonButton(color: Neon.purple))
                }
            }
        }
        .onAppear { studio.start() }
    }
}

func timeString(_ t: TimeInterval) -> String {
    let s = Int(t)
    return String(format: "%02d:%02d:%02d", s / 3600, s / 60 % 60, s % 60)
}

// MARK: - Join view

struct JoinView: View {
    @ObservedObject var client: Client

    var body: some View {
        VStack(spacing: 14) {
            Panel(title: "STUDIO CODE", accent: Neon.pink) {
                HStack {
                    NeonField(placeholder: "Paste the code the studio sent you", text: $client.code, size: 22, mono: true) { client.join() }
                        .disabled(client.joined)
                    Button("Paste") {
                        client.code = NSPasteboard.general.string(forType: .string)?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
                    }.buttonStyle(NeonButton(color: Neon.purple)).disabled(client.joined)
                }
                if !client.code.isEmpty && !client.codeLooksValid {
                    GlowText(text: "That doesn't look like a studio code (like 8RC4TK-8WX0K7-GDZCFA).", color: Neon.yellow, size: 12)
                }
                HStack {
                    if client.joined {
                        Button("Leave") { client.leave() }.buttonStyle(NeonButton(color: Neon.pink))
                    } else {
                        Button { client.join() } label: { Label("Join", systemImage: "bolt.fill") }
                            .buttonStyle(NeonButton(color: Neon.pink, filled: true)).disabled(!client.codeLooksValid)
                    }
                    StatusDot(color: client.studio == nil ? (client.joined ? Neon.yellow : .gray) : Neon.cyan)
                    GlowText(text: client.studio.map { "In session with \($0)" } ?? (client.status.isEmpty ? "Not connected" : client.status), color: .white)
                    Spacer()
                    Toggle("Import prints into Pro Tools", isOn: $client.importIntoProTools)
                        .toggleStyle(.switch).tint(Neon.pink).foregroundColor(Neon.dim).disabled(client.joined)
                }
            }

            Panel(title: "SESSION", accent: Neon.cyan) {
                HStack(spacing: 12) {
                    Button { client.setListening(!client.listening) } label: {
                        Label(client.listening ? "Stop listening" : "Listen to the studio", systemImage: client.listening ? "speaker.slash.fill" : "headphones")
                    }.buttonStyle(NeonButton(color: Neon.cyan, filled: client.listening)).disabled(client.studio == nil)
                    Button { client.sendStems() } label: { Label("Send stems", systemImage: "square.and.arrow.up.fill") }
                        .buttonStyle(NeonButton(color: Neon.pink, filled: true)).disabled(client.studio == nil)
                    Spacer()
                }
                if !client.stemsStatus.isEmpty { GlowText(text: client.stemsStatus, color: Neon.yellow, size: 12) }
                GlowText(text: "Stems are placed from the start of your song. Bounce them from the session start.", color: Neon.dim, size: 11)
                ChatBox(lines: client.chat, enabled: client.studio != nil) { client.say($0) }
            }

            Panel(title: "PRINTS BACK", accent: Neon.pink) {
                if client.prints.isEmpty {
                    GlowText(text: "Prints from the engineer land in your session at the same spot as your stems.", color: Neon.dim, size: 12)
                } else {
                    ForEach(client.prints.prefix(4)) { t in
                        HStack {
                            Text(t.id).font(.system(size: 12, weight: .semibold, design: .monospaced)).foregroundColor(Neon.ice)
                            Spacer()
                            GlowText(text: t.state, color: t.state.hasPrefix("Receiving") ? Neon.cyan : Neon.dim, size: 12)
                        }
                        if !t.detail.isEmpty { GlowText(text: t.detail, color: Neon.yellow, size: 11) }
                    }
                }
                Button("Show in Finder") { NSWorkspace.shared.open(client.folder) }.buttonStyle(NeonButton(color: Neon.purple))
            }

            Panel(title: "RECORD LIVE TO THE STUDIO", accent: Neon.purple) {
                HStack {
                    Picker("", selection: $client.source) {
                        ForEach(client.sources) { s in Text(s.label).tag(Optional(s)) }
                    }.labelsHidden().disabled(client.recording)
                    Button("Refresh") { client.refreshSources() }.buttonStyle(NeonButton(color: Neon.purple)).disabled(client.recording)
                    if client.recording {
                        Button { client.stopRecording() } label: { Label("Stop", systemImage: "stop.fill") }
                            .buttonStyle(NeonButton(color: Neon.pink, filled: true))
                    } else {
                        Button { client.record() } label: { Label("Rec", systemImage: "record.circle") }
                            .buttonStyle(NeonButton(color: Neon.pink, filled: true))
                            .disabled(!client.codeLooksValid || client.source == nil)
                    }
                }
                GlowText(text: client.recordStatus.isEmpty
                         ? "Any DAW: put the RemoteSession Link plugin on a track. Pro Tools can also use Audio Bridge."
                         : client.recordStatus, color: client.recordStatus.isEmpty ? Neon.dim : Neon.yellow, size: 11)
            }
        }
        .onAppear { client.refreshSources() }
    }
}

// MARK: - App

struct RootView: View {
    @ObservedObject var studio: Studio
    @ObservedObject var client: Client
    @AppStorage("tab") private var tab = 0

    var body: some View {
        ZStack {
            VaporBackground()
            ScrollView {
                VStack(spacing: 16) {
                    ChromeTitle(text: "RemoteSession").padding(.top, 34)
                    HStack(spacing: 0) {
                        tabButton("Studio", 0)
                        tabButton("Join a studio", 1)
                    }
                    .padding(3)
                    .background(RoundedRectangle(cornerRadius: 9).fill(Neon.panel))
                    .overlay(RoundedRectangle(cornerRadius: 9).stroke(Neon.line, lineWidth: 1))
                    if tab == 0 { StudioView(studio: studio) } else { JoinView(client: client) }
                }
                .padding(.horizontal, 28).padding(.bottom, 28)
            }
        }
        .preferredColorScheme(.dark)
        .frame(minWidth: 720, minHeight: 820)
    }

    func tabButton(_ title: String, _ i: Int) -> some View {
        Button { tab = i } label: {
            Text(title)
                .font(.system(size: 13, weight: .medium))
                .foregroundColor(tab == i ? Neon.ice : Neon.dim)
                .padding(.vertical, 6).padding(.horizontal, 18)
                .background(RoundedRectangle(cornerRadius: 7).fill(tab == i ? Color.white.opacity(0.1) : .clear))
        }.buttonStyle(.plain)
    }
}

final class AppDelegate: NSObject, NSApplicationDelegate {
    var onQuit: (() -> Void)?
    func applicationWillTerminate(_ notification: Notification) { onQuit?() }
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { true }
}

@main
struct RemoteSessionApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) var delegate
    @StateObject private var studio = Studio()
    @StateObject private var client = Client()

    var body: some Scene {
        WindowGroup("RemoteSession") {
            RootView(studio: studio, client: client)
                .onAppear {
                    delegate.onQuit = { [studio, client] in
                        client.stopRecording()
                        client.leave()
                        studio.stop(waiting: 5) // closes the router port
                    }
                }
        }
        .windowStyle(.hiddenTitleBar)
        .defaultSize(width: 760, height: 980)
        .windowResizability(.contentMinSize)
    }
}
