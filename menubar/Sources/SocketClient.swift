import Foundation

/// Клиент управляющего сокета /var/run/splitr.sock.
///
/// Нужен ровно для одной ручки: POST /config демон принимает только через
/// unix-сокет (root:staff, 0660), потому что конфиг задаёт путь к sshuttle,
/// который запускается от root — по TCP такая запись была бы раздачей root
/// любому локальному процессу. По TCP демон отвечает 403.
///
/// Почему /usr/bin/curl, а не NWConnection: URLSession unix-сокеты не умеет,
/// а на NWConnection пришлось бы писать свой HTTP — формировать запрос,
/// разбирать статус, заголовки, Content-Length и chunked, следить за таймаутом.
/// Это сотня строк ради одного редкого запроса «сохранить конфиг», который
/// делает человек руками. curl входит в macOS, умеет --unix-socket одной
/// опцией и уже реализует весь этот разбор. Цена — один короткоживущий
/// процесс на нажатие кнопки, что здесь совершенно незаметно.
enum SocketClient {
    static let defaultPath = "/var/run/splitr.sock"

    /// Отправляет тело на путь ручки через unix-сокет.
    /// Колбэк вызывается в главном потоке — им обновляют окно редактора.
    static func post(path: String = defaultPath, endpoint: String, body: Data,
                     completion: @escaping (Result<Data, APIError>) -> Void) {
        DispatchQueue.global(qos: .userInitiated).async {
            let result = postSync(socket: path, endpoint: endpoint, body: body)
            DispatchQueue.main.async { completion(result) }
        }
    }

    /// Синхронный вариант — для сценария обновления, который и так целиком
    /// выполняется в фоновом потоке шаг за шагом. Заворачивать его колбэк
    /// в семафор ради этого было бы только запутывающей обёрткой.
    static func postSync(socket: String, endpoint: String, body: Data) -> Result<Data, APIError> {
        guard FileManager.default.fileExists(atPath: socket) else {
            return .failure(.unreachable("no control socket at \(socket) — the daemon is not running"))
        }

        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: "/usr/bin/curl")
        // %{http_code} отдельной строкой в конце: так один вызов даёт и тело,
        // и код ответа, не заставляя разбирать заголовки руками.
        proc.arguments = [
            "--silent", "--show-error",
            "--unix-socket", socket,
            "--max-time", "10",
            "-X", "POST",
            "--data-binary", "@-",
            "-H", "Content-Type: text/plain",
            "-w", "\n%{http_code}",
            "http://localhost\(endpoint)",
        ]

        let stdin = Pipe(), stdout = Pipe(), stderr = Pipe()
        proc.standardInput = stdin
        proc.standardOutput = stdout
        proc.standardError = stderr

        do {
            try proc.run()
        } catch {
            return .failure(.unreachable("could not run curl: \(error.localizedDescription)"))
        }
        stdin.fileHandleForWriting.write(body)
        try? stdin.fileHandleForWriting.close()
        let out = stdout.fileHandleForReading.readDataToEndOfFile()
        let err = stderr.fileHandleForReading.readDataToEndOfFile()
        proc.waitUntilExit()

        guard proc.terminationStatus == 0 else {
            let msg = String(decoding: err, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
            // Самая частая причина — нет прав на сокет (пользователь не в staff).
            return .failure(.unreachable(msg.isEmpty ? "curl exited with code \(proc.terminationStatus)" : msg))
        }

        var text = String(decoding: out, as: UTF8.self)
        guard let nl = text.lastIndex(of: "\n") else { return .failure(.badResponse) }
        let code = Int(text[text.index(after: nl)...].trimmingCharacters(in: .whitespaces)) ?? 0
        text = String(text[text.startIndex..<nl])
        let data = Data(text.utf8)

        if code >= 400 {
            return .failure(.daemon(APIError.message(from: data) ?? "HTTP \(code)"))
        }
        return .success(data)
    }
}
