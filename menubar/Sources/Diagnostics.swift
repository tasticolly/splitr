import AppKit

/// Диагностический вывод в stderr.
///
/// Зачем он вообще нужен. Приложение живёт в строке меню и запускается
/// launchd'ом: у него нет ни окна, ни консоли, ни «первого запуска», на котором
/// можно было бы что-то заметить. Когда иконка не появляется, снаружи это
/// выглядит совершенно одинаково при десятке разных причин — не нашёлся символ,
/// система запомнила «скрыто», пункт уехал под вырез экрана. Поэтому приложение
/// на старте само рассказывает, что с ним произошло, в /tmp/com.splitr.menubar.log
/// (путь задаёт StandardErrorPath в LaunchAgent).
enum Diag {
    /// Файл журнала. stderr одного мало: под LaunchAgent он уходит в
    /// StandardErrorPath, а под объектом входа (SMAppService) — в никуда,
    /// и именно в этом случае диагностика нужнее всего.
    static let logFile = FileManager.default.homeDirectoryForCurrentUser
        .appendingPathComponent("Library/Logs/SplitR.log")

    static func log(_ message: String) {
        let line = "[\(stamp())] \(message)\n"
        FileHandle.standardError.write(Data(line.utf8))
        append(line)
    }

    private static func append(_ line: String) {
        let data = Data(line.utf8)
        let fm = FileManager.default
        if !fm.fileExists(atPath: logFile.path) {
            try? fm.createDirectory(at: logFile.deletingLastPathComponent(),
                                    withIntermediateDirectories: true)
            try? data.write(to: logFile)
            return
        }
        guard let h = try? FileHandle(forWritingTo: logFile) else { return }
        defer { try? h.close() }
        // Журнал пишется только на старте и на проверках, но приложение живёт
        // неделями — обрезаем, чтобы файл не рос бесконечно.
        if (try? h.seekToEnd()) ?? 0 > 256 * 1024 {
            try? Data().write(to: logFile)
            try? h.seek(toOffset: 0)
        }
        try? h.write(contentsOf: data)
    }

    private static let formatter: DateFormatter = {
        let f = DateFormatter()
        f.dateFormat = "yyyy-MM-dd HH:mm:ss"
        return f
    }()

    private static func stamp() -> String { formatter.string(from: Date()) }

    /// Снимок окружения на старте: по нему видно, из бандла ли запущено
    /// приложение, читается ли Info.plist и какая политика активации сработала.
    static func reportEnvironment() {
        let b = Bundle.main
        log("SplitR started")
        log("  bundle: \(b.bundleURL.path)")
        log("  bundleIdentifier: \(b.bundleIdentifier ?? "none (not launched from a bundle)")")
        log("  LSUIElement: \(b.object(forInfoDictionaryKey: "LSUIElement") ?? "none")")
        log("  activationPolicy: \(NSApp.activationPolicy() == .accessory ? ".accessory" : "\(NSApp.activationPolicy().rawValue)")")
        log("  macOS: \(ProcessInfo.processInfo.operatingSystemVersionString)")
    }
}
