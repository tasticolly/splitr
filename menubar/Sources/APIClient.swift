import Foundation

/// Ошибка, которую видит пользователь. Отдельный тип нужен, чтобы отличать
/// «демон не отвечает» (нормальная ситуация, демон может быть выключен)
/// от «демон ответил ошибкой» (её надо показать текстом).
enum APIError: LocalizedError {
    case unreachable(String)
    case daemon(String)
    case notFound
    case badResponse

    var errorDescription: String? {
        switch self {
        case .unreachable(let s): return "daemon is unreachable: \(s)"
        case .daemon(let s): return s
        case .notFound: return "the daemon does not know this command"
        case .badResponse: return "unexpected response from the daemon"
        }
    }

    var isUnreachable: Bool {
        if case .unreachable = self { return true }
        return false
    }

    var isNotFound: Bool {
        if case .notFound = self { return true }
        return false
    }

    /// Достаёт человеческий текст ошибки из тела ответа.
    /// Демон отвечает и JSON'ом ({"error": ...}), и голым текстом
    /// (http.Error на 403 при записи конфига по TCP) — разбираем оба вида,
    /// иначе пользователь увидит бесполезное «HTTP 403».
    static func message(from data: Data) -> String? {
        if let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
           let msg = obj["error"] as? String, !msg.isEmpty {
            return msg
        }
        let text = String(decoding: data.prefix(2000), as: UTF8.self)
            .trimmingCharacters(in: .whitespacesAndNewlines)
        return text.isEmpty ? nil : text
    }
}

/// Клиент управляющего API демона (см. internal/daemon/server.go).
///
/// Ходим по HTTP на 127.0.0.1:8787, а не в /var/run/splitr.sock: URLSession
/// unix-сокеты не умеет, а поднимать ради этого собственный HTTP поверх сокета —
/// лишний код там, где localhost-порт демон и так открывает по умолчанию.
final class APIClient {
    private let session: URLSession
    private let base: String

    init(host: String = "127.0.0.1:8787") {
        let cfg = URLSessionConfiguration.ephemeral
        // Таймауты короткие: опрос идёт раз в 2 секунды, зависший запрос
        // не должен пережить следующий тик и копиться в очереди.
        cfg.timeoutIntervalForRequest = 3
        // Общий предел на всю задачу: он перекрывает таймаут отдельного
        // запроса, поэтому держим его выше самой долгой команды (см.
        // commandTimeout) — иначе подъём туннеля обрывался бы на пятой секунде.
        cfg.timeoutIntervalForResource = 60
        cfg.requestCachePolicy = .reloadIgnoringLocalAndRemoteCacheData
        // Системный прокси на localhost не нужен и ломает запросы у тех,
        // у кого настроен корпоративный PAC.
        cfg.connectionProxyDictionary = [:]
        session = URLSession(configuration: cfg)
        base = "http://\(host)"
    }

    deinit { session.invalidateAndCancel() }

    // MARK: - низкий уровень

    /// Таймаут для команд, которые демон выполняет до ответа: подъём туннеля
    /// он делает синхронно (гасит чужие процессы, готовит DNS, накладывает
    /// правила), и в обычные три секунды это не укладывается. Короткий таймаут
    /// здесь давал бы «демон недоступен» ровно тогда, когда демон работает.
    static let commandTimeout: TimeInterval = 30

    private func request(_ method: String, _ path: String, body: [String: String]?,
                         timeout: TimeInterval? = nil,
                         completion: @escaping (Result<Data, APIError>) -> Void) {
        // URL собираем строкой, а не appendingPathComponent: тот экранирует «?»
        // и превратил бы /log?tail=300 в путь с %3F.
        guard let url = URL(string: base + "/" + path) else {
            DispatchQueue.main.async { completion(.failure(.badResponse)) }
            return
        }
        var req = URLRequest(url: url)
        req.httpMethod = method
        if let timeout { req.timeoutInterval = timeout }
        if let body {
            req.httpBody = try? JSONSerialization.data(withJSONObject: body)
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        let task = session.dataTask(with: req) { data, resp, err in
            // Все колбэки поднимаем в главный поток: они правят меню и иконку.
            let finish: (Result<Data, APIError>) -> Void = { res in
                DispatchQueue.main.async { completion(res) }
            }
            if let err {
                finish(.failure(.unreachable(err.localizedDescription)))
                return
            }
            guard let http = resp as? HTTPURLResponse, let data else {
                finish(.failure(.badResponse))
                return
            }
            // 404 — отдельный случай: демон жив, но такой ручки у него нет.
            // По нему клиент отличает старую версию демона от настоящей ошибки
            // и уходит на прежнее имя эндпоинта.
            if http.statusCode == 404 {
                finish(.failure(.notFound))
                return
            }
            if http.statusCode >= 400 {
                finish(.failure(.daemon(APIError.message(from: data) ?? "HTTP \(http.statusCode)")))
                return
            }
            finish(.success(data))
        }
        task.resume()
    }

    private func decode<T: Decodable>(_ type: T.Type, _ result: Result<Data, APIError>) -> Result<T, APIError> {
        switch result {
        case .failure(let e): return .failure(e)
        case .success(let data):
            guard let v = try? JSONDecoder().decode(T.self, from: data) else {
                return .failure(.badResponse)
            }
            return .success(v)
        }
    }

    // MARK: - ручки

    func status(_ completion: @escaping (Result<DaemonStatus, APIError>) -> Void) {
        request("GET", "status", body: nil) { [weak self] r in
            guard let self else { return }
            completion(self.decode(DaemonStatus.self, r))
        }
    }

    func config(_ completion: @escaping (Result<DaemonConfig, APIError>) -> Void) {
        request("GET", "config", body: nil) { [weak self] r in
            guard let self else { return }
            completion(self.decode(DaemonConfig.self, r))
        }
    }

    func rules(_ completion: @escaping (Result<String, APIError>) -> Void) {
        request("GET", "rules", body: nil) { r in
            completion(r.map { String(data: $0, encoding: .utf8) ?? "" })
        }
    }

    func up(profile: String, _ completion: @escaping (Result<DaemonStatus, APIError>) -> Void) {
        request("POST", "up", body: ["profile": profile], timeout: Self.commandTimeout) { [weak self] r in
            guard let self else { return }
            completion(self.decode(DaemonStatus.self, r))
        }
    }

    func down(_ completion: @escaping (Result<DaemonStatus, APIError>) -> Void) {
        request("POST", "down", body: nil, timeout: Self.commandTimeout) { [weak self] r in
            guard let self else { return }
            completion(self.decode(DaemonStatus.self, r))
        }
    }

    /// mode — on|off|strict, policy — all|public|custom|off. Демон применяет
    /// policy первой, поэтому их можно слать вместе.
    ///
    /// Имя ручки перебираем: в новых версиях демона она называется /protect,
    /// в старых — /killswitch. На 404 просто пробуем следующее имя, а первое
    /// удачное запоминаем, чтобы не платить лишним запросом на каждый клик.
    /// Так же поступаем со значением strict: старый демон знает его как panic.
    private static let protectionEndpoints = ["protect", "protection", "killswitch"]
    private var knownProtectionEndpoint: String?

    func protection(mode: String? = nil, policy: String? = nil,
                    _ completion: @escaping (Result<DaemonStatus, APIError>) -> Void) {
        var body: [String: String] = [:]
        if let mode { body["mode"] = mode }
        if let policy { body["policy"] = policy }
        let endpoints = knownProtectionEndpoint.map { [$0] } ?? Self.protectionEndpoints
        postProtection(body: body, endpoints: endpoints, completion)
    }

    private func postProtection(body: [String: String], endpoints: [String],
                                _ completion: @escaping (Result<DaemonStatus, APIError>) -> Void) {
        guard let endpoint = endpoints.first else {
            completion(.failure(.notFound))
            return
        }
        request("POST", endpoint, body: body) { [weak self] r in
            guard let self else { return }
            if case .failure(let err) = r, err.isNotFound {
                self.postProtection(body: body, endpoints: Array(endpoints.dropFirst()), completion)
                return
            }
            // Старый демон не знает слова strict и отвечает ошибкой:
            // повторяем тем же запросом со старым именем режима.
            if case .failure(let err) = r, case .daemon = err, body["mode"] == "strict" {
                var legacy = body
                legacy["mode"] = "panic"
                self.request("POST", endpoint, body: legacy) { r2 in
                    if case .success = r2 { self.knownProtectionEndpoint = endpoint }
                    completion(self.decode(DaemonStatus.self, r2))
                }
                return
            }
            if case .success = r { self.knownProtectionEndpoint = endpoint }
            completion(self.decode(DaemonStatus.self, r))
        }
    }

    /// Текст конфига как есть. Чтение демон разрешает и по TCP — секрета в
    /// конфиге нет, опасна только запись.
    func configRaw(_ completion: @escaping (Result<String, APIError>) -> Void) {
        request("GET", "config/raw", body: nil) { r in
            completion(r.map { String(decoding: $0, as: UTF8.self) })
        }
    }

    /// Хвост журнала. Читаем через демон, а не файл напрямую: журнал пишет
    /// root, и права на файл перестают быть нашей проблемой.
    func log(tail: Int, _ completion: @escaping (Result<String, APIError>) -> Void) {
        request("GET", "log?tail=\(tail)", body: nil) { r in
            completion(r.map { String(decoding: $0, as: UTF8.self) })
        }
    }

    /// Запись конфига идёт только через unix-сокет (см. SocketClient).
    func writeConfig(_ yaml: String, socketPath: String = SocketClient.defaultPath,
                     _ completion: @escaping (Result<DaemonStatus, APIError>) -> Void) {
        SocketClient.post(path: socketPath, endpoint: "/config", body: Data(yaml.utf8)) { [weak self] r in
            guard let self else { return }
            completion(self.decode(DaemonStatus.self, r))
        }
    }

    func reload(_ completion: @escaping (Result<DaemonStatus, APIError>) -> Void) {
        request("POST", "reload", body: nil) { [weak self] r in
            guard let self else { return }
            completion(self.decode(DaemonStatus.self, r))
        }
    }

    /// Отдельный запрос о доступности обновления. То же самое приходит в поле
    /// update ответа /status; эта ручка нужна кнопке «Check for updates now»,
    /// чтобы не ждать следующего тика опроса.
    func updateInfo(_ completion: @escaping (Result<UpdateInfo, APIError>) -> Void) {
        request("GET", "update", body: nil) { [weak self] r in
            guard let self else { return }
            completion(self.decode(UpdateInfo.self, r))
        }
    }

    func probe(_ completion: @escaping (Result<ProbeReport, APIError>) -> Void) {
        request("POST", "probe", body: nil) { [weak self] r in
            guard let self else { return }
            completion(self.decode(ProbeReport.self, r))
        }
    }
}
