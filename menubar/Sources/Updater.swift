import Foundation

/// Установка новой версии SplitR из меню.
///
/// Почему это не одна ручка демона. Демон работает от root и умеет подменить
/// только свой собственный, уже собранный бинарь. Собирать он не может: сборка
/// от root оставила бы в репозитории артефакты, принадлежащие root, и следующий
/// `make` от пользователя упал бы на правах. Поэтому обязанности разделены —
/// собирает приложение (от имени пользователя), подменяет себя демон, а бандл
/// приложения подменяет отсоединённый скрипт, потому что живое приложение не
/// может переписать собственный бандл под собой.
///
/// Порядок шагов: build → POST /update → перезапуск демона → подмена бандла.
final class Updater {
    /// Куда идут отчёты о ходе работы. Все вызовы — в главном потоке:
    /// ими рисуется окно прогресса и меняется пункт меню.
    var onLog: ((String) -> Void)?
    var onStage: ((String) -> Void)?
    /// Завершение: nil — успех без перезапуска приложения, строка — ошибка.
    /// При успешной подмене бандла колбэк вызывается с needsRestart = true,
    /// после чего приложение обязано завершиться: его подменит вспомогательный
    /// процесс и запустит заново.
    var onFinish: ((_ error: String?, _ needsRestart: Bool) -> Void)?

    private let repoPath: String
    private let socketPath: String
    private let apiHost: String

    init(repoPath: String, socketPath: String, apiHost: String) {
        self.repoPath = repoPath
        self.socketPath = socketPath
        self.apiHost = apiHost
    }

    // MARK: - сценарий

    func start() {
        DispatchQueue.global(qos: .userInitiated).async { [weak self] in
            guard let self else { return }
            do {
                try self.checkRepo()
                let tools = try self.findTools()
                try self.build(tools)
                let installed = try self.installDaemon()
                self.log("daemon reports version \(installed)")
                try self.waitForDaemon()
                let restart = try self.replaceApp()
                self.finish(nil, restart)
            } catch let e as UpdateError {
                self.finish(e.text, false)
            } catch {
                self.finish(error.localizedDescription, false)
            }
        }
    }

    // MARK: - шаги

    private func checkRepo() throws {
        var isDir: ObjCBool = false
        guard FileManager.default.fileExists(atPath: repoPath, isDirectory: &isDir), isDir.boolValue else {
            throw UpdateError("The source repository is not at \(repoPath) any more, so the new version cannot be built. Move it back or update from a terminal with: make update")
        }
        guard FileManager.default.fileExists(atPath: repoPath + "/Makefile") else {
            throw UpdateError("There is no Makefile in \(repoPath): this does not look like the SplitR repository.")
        }
    }

    /// Пути к make и go.
    ///
    /// GUI-приложение не наследует PATH из шелла: launchd даёт ему голый
    /// /usr/bin:/bin:/usr/sbin:/sbin, в котором go нет. Поэтому PATH собираем
    /// сами — из логин-шелла, path_helper и типичных мест установки — и в нём
    /// же ищем инструменты. Если чего-то нет, честнее сказать чего именно,
    /// чем показать «make: go: command not found» из середины сборки.
    private struct Tools { let make: String; let path: String }

    private func findTools() throws -> Tools {
        var dirs: [String] = []
        func add(_ list: [String]) {
            for d in list where !d.isEmpty && !dirs.contains(d) { dirs.append(d) }
        }
        add(loginShellPath())
        add(pathHelperPath())
        add(["/opt/homebrew/bin", "/usr/local/bin", "/usr/local/go/bin",
             NSHomeDirectory() + "/go/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"])
        // Кеги homebrew с версией (go@1.24) не попадают в /opt/homebrew/bin,
        // если формула не залинкована, — забираем их явно.
        for prefix in ["/opt/homebrew/opt", "/usr/local/opt"] {
            let names = (try? FileManager.default.contentsOfDirectory(atPath: prefix)) ?? []
            add(names.filter { $0.hasPrefix("go") }.sorted().reversed().map { "\(prefix)/\($0)/bin" })
        }

        guard let make = which("make", in: dirs) else {
            throw UpdateError("make was not found. Looked in:\n" + dirs.joined(separator: "\n"))
        }
        guard which("go", in: dirs) != nil else {
            throw UpdateError("The Go compiler was not found, and SplitR cannot be built without it. Looked in:\n"
                              + dirs.joined(separator: "\n")
                              + "\n\nInstall it (brew install go) or update from a terminal with: make update")
        }
        return Tools(make: make, path: dirs.joined(separator: ":"))
    }

    private func build(_ tools: Tools) throws {
        stage("building")
        log("$ make build   (in \(repoPath))")
        // make build прогоняет проверки и тесты. Красная сборка означает, что
        // ставить нечего: до POST /update дело не доходит.
        try run(tools.make, ["build"], path: tools.path, what: "make build")
        log("")
        log("$ make menubar")
        try run(tools.make, ["menubar"], path: tools.path, what: "make menubar")
    }

    private func installDaemon() throws -> String {
        stage("installing the daemon")
        log("")
        log("$ POST /update  (through \(socketPath))")
        // Только через unix-сокет: по TCP демон отвечает 403, потому что
        // подмена его бинаря — это выдача root любому локальному процессу.
        switch SocketClient.postSync(socket: socketPath, endpoint: "/update", body: Data()) {
        case .failure(let e):
            throw UpdateError("The daemon refused to install the update: "
                              + (e.errorDescription ?? "unknown error")
                              + "\n\nThe new version is built and stays in the repository; you can finish from a terminal with: make update")
        case .success(let data):
            let res = (try? JSONDecoder().decode(UpdateResult.self, from: data)) ?? UpdateResult()
            return res.installed.isEmpty ? "unknown" : res.installed
        }
    }

    /// Демон после подмены бинаря завершается, и его поднимает launchd.
    /// Пауза между «ответил» и «умер» есть всегда, поэтому сначала ждём,
    /// пока он перестанет отвечать, а уже потом — пока ответит снова.
    private func waitForDaemon() throws {
        stage("restarting the daemon")
        log("")
        log("waiting for the daemon to come back…")
        let deadline = Date().addingTimeInterval(30)
        while Date() < deadline {
            Thread.sleep(forTimeInterval: 1.0)
            if daemonAnswers() {
                log("the daemon is up again")
                return
            }
        }
        throw UpdateError("The daemon did not come back within 30 seconds. The new binary is installed; start the daemon from a terminal:\n\n\(kickstartCommand)")
    }

    /// Живой ли демон. Проверяем curl'ом, а не URLSession: шаг выполняется
    /// в фоновом потоке синхронно, и заворачивать колбэк в семафор ради
    /// одного GET здесь только запутало бы.
    private func daemonAnswers() -> Bool {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/curl")
        p.arguments = ["--silent", "--output", "/dev/null", "--max-time", "2", "http://\(apiHost)/status"]
        p.standardOutput = FileHandle.nullDevice
        p.standardError = FileHandle.nullDevice
        do { try p.run() } catch { return false }
        p.waitUntilExit()
        return p.terminationStatus == 0
    }

    /// Подмена собственного бандла. Возвращает true, если приложение надо
    /// завершить: дальше работает вспомогательный процесс.
    private func replaceApp() throws -> Bool {
        stage("replacing the app")
        let src = repoPath + "/menubar/build/SplitR.app"
        guard FileManager.default.fileExists(atPath: src) else {
            log("")
            log("no freshly built bundle at \(src) — the menu bar app is left as it is")
            return false
        }
        let current = Bundle.main.bundleURL.path
        let dest = current.hasSuffix(".app") ? current : "/Applications/SplitR.app"
        if URL(fileURLWithPath: dest).standardizedFileURL == URL(fileURLWithPath: src).standardizedFileURL {
            log("")
            log("the app runs straight from the build directory, so it is already up to date")
            return false
        }
        let script = try writeHelper(src: src, dest: dest)
        log("")
        log("replacing \(dest) and restarting")
        // Отсоединённый процесс: он переживёт наш выход (его подхватит launchd),
        // дождётся, пока pid исчезнет, и только тогда тронет бандл.
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/bin/bash")
        p.arguments = [script]
        p.standardInput = FileHandle.nullDevice
        p.standardOutput = FileHandle.nullDevice
        p.standardError = FileHandle.nullDevice
        try p.run()
        return true
    }

    private func writeHelper(src: String, dest: String) throws -> String {
        let log = NSTemporaryDirectory() + "splitr-selfupdate.log"
        let path = NSTemporaryDirectory() + "splitr-selfupdate-\(UUID().uuidString).sh"
        // Копируем сначала рядом и только потом подменяем: если ditto упадёт
        // (нет прав, кончилось место), старый бандл останется целым и будет
        // запущен обратно — обновление не должно оставлять человека без иконки.
        let text = """
        #!/bin/bash
        set -u
        SRC=\(shq(src))
        DEST=\(shq(dest))
        PID=\(ProcessInfo.processInfo.processIdentifier)
        exec >>\(shq(log)) 2>&1
        echo "=== $(date) replacing $DEST"
        for _ in $(seq 1 150); do
        \tkill -0 "$PID" 2>/dev/null || break
        \tsleep 0.2
        done
        rm -rf "$DEST.new"
        if /usr/bin/ditto "$SRC" "$DEST.new"; then
        \trm -rf "$DEST"
        \tmv "$DEST.new" "$DEST"
        \t/usr/bin/xattr -dr com.apple.quarantine "$DEST" 2>/dev/null
        else
        \techo "could not copy the bundle, keeping the old one"
        \trm -rf "$DEST.new"
        fi
        /usr/bin/open -a "$DEST" || /usr/bin/open "$DEST"
        rm -f "$0"
        """
        try text.write(toFile: path, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: path)
        return path
    }

    // MARK: - процессы

    private func run(_ tool: String, _ args: [String], path: String, what: String) throws {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: tool)
        p.arguments = args
        p.currentDirectoryURL = URL(fileURLWithPath: repoPath)
        var env = ProcessInfo.processInfo.environment
        env["PATH"] = path
        // Сборка Go в песочнице GUI-приложения без HOME не найдёт кеш модулей.
        env["HOME"] = NSHomeDirectory()
        p.environment = env

        // Один канал на stdout и stderr: у make ошибки идут в stderr, а команды
        // в stdout, и в двух отдельных потоках их порядок восстановить нельзя.
        let pipe = Pipe()
        p.standardOutput = pipe
        p.standardError = pipe
        p.standardInput = FileHandle.nullDevice
        do {
            try p.run()
        } catch {
            throw UpdateError("Could not run \(tool): \(error.localizedDescription)")
        }
        var tail = ""
        while true {
            let chunk = pipe.fileHandleForReading.availableData
            if chunk.isEmpty { break }
            tail += String(decoding: chunk, as: UTF8.self)
            while let nl = tail.firstIndex(of: "\n") {
                log(String(tail[tail.startIndex..<nl]))
                tail = String(tail[tail.index(after: nl)...])
            }
        }
        if !tail.isEmpty { log(tail) }
        p.waitUntilExit()
        guard p.terminationStatus == 0 else {
            throw UpdateError("\(what) failed (exit code \(p.terminationStatus)). The output is in the progress window: checks and tests run as part of the build, and a red build must not be installed.")
        }
    }

    /// PATH логин-шелла. Он знает про homebrew, rbenv и прочие правки профиля,
    /// которых нет ни в одном списке «типичных мест».
    private func loginShellPath() -> [String] {
        let shell = ProcessInfo.processInfo.environment["SHELL"] ?? "/bin/zsh"
        guard FileManager.default.isExecutableFile(atPath: shell) else { return [] }
        return (capture(shell, ["-lc", "printf %s \"$PATH\""]) ?? "").split(separator: ":").map(String.init)
    }

    private func pathHelperPath() -> [String] {
        // Печатает PATH="a:b:c"; export PATH; — из него забираем середину.
        guard let out = capture("/usr/libexec/path_helper", ["-s"]),
              let start = out.range(of: "PATH=\""),
              let end = out.range(of: "\"", range: start.upperBound..<out.endIndex) else { return [] }
        return out[start.upperBound..<end.lowerBound].split(separator: ":").map(String.init)
    }

    private func capture(_ tool: String, _ args: [String]) -> String? {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: tool)
        p.arguments = args
        let out = Pipe()
        p.standardOutput = out
        p.standardError = FileHandle.nullDevice
        p.standardInput = FileHandle.nullDevice
        do { try p.run() } catch { return nil }
        let data = out.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        guard p.terminationStatus == 0 else { return nil }
        return String(decoding: data, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
    }

    private func which(_ tool: String, in dirs: [String]) -> String? {
        for d in dirs {
            let p = (d as NSString).appendingPathComponent(tool)
            if FileManager.default.isExecutableFile(atPath: p) { return p }
        }
        return nil
    }

    private func shq(_ s: String) -> String { "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'" }

    // MARK: - отчёты

    private func log(_ line: String) { DispatchQueue.main.async { self.onLog?(line) } }
    private func stage(_ s: String) { DispatchQueue.main.async { self.onStage?(s) } }
    private func finish(_ error: String?, _ restart: Bool) {
        DispatchQueue.main.async { self.onFinish?(error, restart) }
    }
}

/// Ошибка обновления. Отдельный тип, чтобы текст в окне и в алерте был один
/// и тот же и всегда содержал, что делать дальше.
struct UpdateError: Error {
    let text: String
    init(_ text: String) { self.text = text }
}
