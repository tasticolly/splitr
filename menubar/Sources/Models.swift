import Foundation

// Модели ответов демона. Поля повторяют json-теги из internal/daemon/status.go
// и internal/config/config.go. Всё, что демон может не прислать (omitempty),
// объявлено опциональным — иначе один пустой ключ уронил бы разбор целиком,
// и приложение выглядело бы «демон недоступен» при живом демоне.

struct DaemonStatus: Decodable {
    var tunnel: String = "down"
    var profile: String = ""
    var pid: Int?
    var since: String?
    var lastError: String?
    var pfEnabled: Bool = false
    var anchorLoaded: Bool = false
    var anchorLinked: Bool = false
    /// Состояние защиты: off | on (all|public|custom) | strict.
    /// Демон отдаёт его либо новым ключом protection, либо старым killswitch —
    /// разбираем оба, чтобы приложение пережило несогласованную версию демона.
    var protection: String = "off"
    var blocking: Bool = false
    var blockedNets: [String] = []
    var allowedNets: [String] = []
    var sshuttleAnchors: [String] = []
    /// Туннель поднят мимо splitr (якоря sshuttle есть, а процесса у демона нет).
    var external: Bool = false
    var configPath: String = ""
    /// Политика блокировки из конфига (all|public|custom|off) — в отличие от
    /// protection она не «схлопывается» в off/strict и годится для галочки в меню.
    var mode: String = ""
    var version: String = ""
    var startedAt: String = ""
    var logFile: String = ""
    var warnings: [String] = []
    /// Сведения об обновлении. Опционально нарочно: старый демон поля не
    /// присылает, и тогда приложение просто ничего не знает про обновления —
    /// это не ошибка и показывать в меню нечего.
    var update: UpdateInfo?
    /// Что человек должен сделать руками, чтобы туннель поднялся (сейчас это
    /// только «войди заново по ссылке»). Демон присылает поле, лишь когда
    /// действительно чего-то ждёт от человека, — поэтому оно опциональное.
    var actionRequired: ActionRequired?

    enum CodingKeys: String, CodingKey {
        case tunnel, profile, pid, since
        case lastError = "last_error"
        case pfEnabled = "pf_enabled"
        case anchorLoaded = "anchor_loaded"
        case anchorLinked = "anchor_linked"
        case protection, killswitch, blocking
        case blockedNets = "blocked_nets"
        case allowedNets = "allowed_nets"
        case sshuttleAnchors = "sshuttle_anchors"
        case external
        case configPath = "config_path"
        case mode, version
        case startedAt = "started_at"
        case logFile = "log_file"
        case warnings, update
        case actionRequired = "action_required"
    }

    init() {}

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        tunnel = (try? c.decode(String.self, forKey: .tunnel)) ?? "down"
        profile = (try? c.decode(String.self, forKey: .profile)) ?? ""
        pid = try? c.decode(Int.self, forKey: .pid)
        since = try? c.decode(String.self, forKey: .since)
        lastError = try? c.decode(String.self, forKey: .lastError)
        pfEnabled = (try? c.decode(Bool.self, forKey: .pfEnabled)) ?? false
        anchorLoaded = (try? c.decode(Bool.self, forKey: .anchorLoaded)) ?? false
        anchorLinked = (try? c.decode(Bool.self, forKey: .anchorLinked)) ?? false
        // Сначала новое имя поля, потом старое: на старом демоне protection
        // просто не придёт, и подставится killswitch.
        let raw = (try? c.decode(String.self, forKey: .protection))
            ?? (try? c.decode(String.self, forKey: .killswitch))
            ?? "off"
        protection = DaemonStatus.normalize(raw)
        blocking = (try? c.decode(Bool.self, forKey: .blocking)) ?? false
        blockedNets = (try? c.decode([String].self, forKey: .blockedNets)) ?? []
        allowedNets = (try? c.decode([String].self, forKey: .allowedNets)) ?? []
        sshuttleAnchors = (try? c.decode([String].self, forKey: .sshuttleAnchors)) ?? []
        external = (try? c.decode(Bool.self, forKey: .external)) ?? false
        configPath = (try? c.decode(String.self, forKey: .configPath)) ?? ""
        mode = (try? c.decode(String.self, forKey: .mode)) ?? ""
        version = (try? c.decode(String.self, forKey: .version)) ?? ""
        startedAt = (try? c.decode(String.self, forKey: .startedAt)) ?? ""
        logFile = (try? c.decode(String.self, forKey: .logFile)) ?? ""
        warnings = (try? c.decode([String].self, forKey: .warnings)) ?? []
        update = try? c.decode(UpdateInfo.self, forKey: .update)
        // Пустой Kind демон считает «ничего не требуется» — приводим к nil
        // здесь, чтобы дальше по коду проверка была одна: есть требование или нет.
        if let a = try? c.decode(ActionRequired.self, forKey: .actionRequired), !a.kind.isEmpty {
            actionRequired = a
        }
    }

    /// Старое имя режима приводим к новому в одной точке, чтобы дальше по коду
    /// «panic» не встречалось вовсе.
    static func normalize(_ raw: String) -> String {
        raw == "panic" ? "strict" : raw
    }

    var tunnelIsUp: Bool { tunnel == "up" }
    var tunnelIsStarting: Bool { tunnel == "starting" }

    /// Сколько демон уже работает. nil, если момент старта не пришёл или
    /// это нулевое время Go ("0001-01-01...") — показывать «55 лет» глупо.
    var uptime: TimeInterval? {
        guard let date = DaemonStatus.time(startedAt) else { return nil }
        let d = Date().timeIntervalSince(date)
        return d >= 0 ? d : nil
    }

    /// Момент, когда туннель перешёл в нынешнее состояние (демон ставит его
    /// при запуске sshuttle и больше не трогает).
    ///
    /// Нужен ровно для одного: отличить «туннель не поднялся вот сейчас» от
    /// «туннель не поднялся в прошлый раз». Состояние failed демон держит до
    /// следующей попытки, и без этой отметки свежий ответ о старом провале
    /// неотличим от свежего провала — а значит, клик по Connect мог бы сразу
    /// же отчитаться об ошибке, которой в этой попытке ещё не случилось.
    var sinceDate: Date? { DaemonStatus.time(since ?? "") }

    /// Разбор времени от Go: с долями секунды и без них. Нулевое время Go
    /// («0001-01-01…») — это «никогда», а не 55 лет назад.
    static func time(_ raw: String) -> Date? {
        guard !raw.isEmpty, !raw.hasPrefix("0001-01-01") else { return nil }
        let iso = ISO8601DateFormatter()
        iso.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let d = iso.date(from: raw) { return d }
        let plain = ISO8601DateFormatter()
        plain.formatOptions = [.withInternetDateTime]
        return plain.date(from: raw)
    }
    /// Трафик заворачивается в туннель — неважно, нашим процессом или чужим.
    var trafficTunneled: Bool { tunnel == "up" || external }
    var protectionOff: Bool { protection == "off" }
    var strictMode: Bool { protection == "strict" }
    /// Правила защиты действительно лежат в ядре и подключены к pf.
    ///
    /// Это не то же самое, что поле blocking демона: blocking означает «режет
    /// прямо сейчас» и по замыслу ложно при живом туннеле — правила есть, но
    /// их перебивают якоря sshuttle. Судить по нему о том, работает ли защита,
    /// нельзя: получилось бы «защита не действует» на каждом живом туннеле.
    var rulesInEffect: Bool { pfEnabled && anchorLoaded && anchorLinked }
    /// Защиту выключили сознательно, поэтому правил в ядре нет и быть не должно:
    /// демон при protection=off чистит якорь. Ругаться на пустой якорь в этом
    /// случае — сообщать о собственной настройке как о поломке.
    var rulesUnloadedOnPurpose: Bool { protectionOff }
    /// Туннель не поднялся. Это состояние демон держит до следующей попытки,
    /// поэтому по нему можно понять, что подъём кончился неудачей.
    var tunnelFailed: Bool { tunnel == "failed" }
}

/// Требование к человеку из ответа демона (internal/shuttle/action.go).
///
/// Отдельный тип, а не строка, ровно из-за ссылки: «нужно войти» без ссылки —
/// это тупик, а со ссылкой — одно нажатие. Поэтому URL хранится отдельно,
/// и меню умеет предложить его открыть.
struct ActionRequired: Decodable {
    var kind: String = ""
    var message: String = ""
    var url: String = ""

    enum CodingKeys: String, CodingKey { case kind, message, url }

    init() {}

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        kind = (try? c.decode(String.self, forKey: .kind)) ?? ""
        message = (try? c.decode(String.self, forKey: .message)) ?? ""
        url = (try? c.decode(String.self, forKey: .url)) ?? ""
    }

    /// Текст для меню и уведомления: сообщение демона, а если его нет —
    /// нейтральная замена, чтобы строка не оказалась пустой.
    var text: String { message.isEmpty ? "the daemon needs you to sign in" : message }
    /// Ссылку открываем только если она похожа на ссылку.
    var link: URL? { url.hasPrefix("http") ? URL(string: url) : nil }
}

/// Кусок /config, который реально нужен меню: список профилей и профиль по умолчанию.
/// Остальные поля конфига намеренно не разбираем — чтобы новые ключи в yaml
/// не ломали приложение.
struct DaemonConfig: Decodable {
    struct Profile: Decodable {
        var remote: String = ""
        enum CodingKeys: String, CodingKey { case remote }
        init(from decoder: Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            remote = (try? c.decode(String.self, forKey: .remote)) ?? ""
        }
    }

    var defaultProfile: String = ""
    var profiles: [String: Profile] = [:]
    var logFile: String = ""
    var httpAddr: String = "127.0.0.1:8787"
    var socketPath: String = SocketClient.defaultPath
    /// Пишет ли демон отброшенные пакеты в pflog0 (protection.log в конфиге,
    /// раньше killswitch.log). Без него живой поток пакетов отдаёт 412,
    /// и пункт меню лучше сразу показать неактивным.
    var packetLogEnabled: Bool = false

    enum CodingKeys: String, CodingKey {
        case defaultProfile = "default_profile"
        case profiles, daemon, protection, killswitch
    }
    enum DaemonKeys: String, CodingKey {
        case logFile = "log_file"
        case httpAddr = "http_addr"
        case socketPath = "socket_path"
    }
    enum ProtectionKeys: String, CodingKey {
        case log
    }

    init() {}

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        defaultProfile = (try? c.decode(String.self, forKey: .defaultProfile)) ?? ""
        profiles = (try? c.decode([String: Profile].self, forKey: .profiles)) ?? [:]
        if let d = try? c.nestedContainer(keyedBy: DaemonKeys.self, forKey: .daemon) {
            logFile = (try? d.decode(String.self, forKey: .logFile)) ?? ""
            httpAddr = (try? d.decode(String.self, forKey: .httpAddr)) ?? "127.0.0.1:8787"
            // Путь к управляющему сокету берём из конфига, а не зашиваем:
            // запись конфига идёт именно туда, и на нестандартной установке
            // жёсткая константа молча промахнулась бы.
            socketPath = (try? d.decode(String.self, forKey: .socketPath)) ?? SocketClient.defaultPath
        }
        // Секция защиты: новое имя, при его отсутствии — старое.
        let section = (try? c.nestedContainer(keyedBy: ProtectionKeys.self, forKey: .protection))
            ?? (try? c.nestedContainer(keyedBy: ProtectionKeys.self, forKey: .killswitch))
        if let section {
            packetLogEnabled = (try? section.decode(Bool.self, forKey: .log)) ?? false
        }
    }

    /// Имена профилей в стабильном порядке: словарь в JSON порядка не гарантирует,
    /// а меню, которое каждый раз тасует пункты, невозможно использовать по памяти.
    var profileNames: [String] { profiles.keys.sorted() }
}

/// Одна попытка соединения (internal/daemon/server.go, ProbeTarget).
struct ProbeTarget: Decodable {
    var address: String = ""
    var reachable: Bool = false
    var detail: String = ""

    enum CodingKeys: String, CodingKey { case address, reachable, detail }

    init() {}

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        address = (try? c.decode(String.self, forKey: .address)) ?? ""
        reachable = (try? c.decode(Bool.self, forKey: .reachable)) ?? false
        detail = (try? c.decode(String.self, forKey: .detail)) ?? ""
    }
}

/// Результат POST /probe (ProbeReport). Демон сам выносит вердикт — приложение
/// его только показывает, чтобы формулировка «утечка / всё в порядке» была
/// одна и та же в CLI, вебе и меню.
struct ProbeReport: Decodable {
    var control = ProbeTarget()
    var blocked: [ProbeTarget] = []
    var verdict: String = ""
    var leaked: Bool = false
    var tunnelUp: Bool = false
    var inconclusive: Bool = false

    enum CodingKeys: String, CodingKey {
        case control, blocked, verdict, leaked
        case tunnelUp = "tunnel_up"
        case inconclusive
    }

    init() {}

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        control = (try? c.decode(ProbeTarget.self, forKey: .control)) ?? ProbeTarget()
        blocked = (try? c.decode([ProbeTarget].self, forKey: .blocked)) ?? []
        verdict = (try? c.decode(String.self, forKey: .verdict)) ?? ""
        leaked = (try? c.decode(Bool.self, forKey: .leaked)) ?? false
        tunnelUp = (try? c.decode(Bool.self, forKey: .tunnelUp)) ?? false
        inconclusive = (try? c.decode(Bool.self, forKey: .inconclusive)) ?? false
    }

    /// Текст для NSAlert: вердикт плюс сырые попытки — без них непонятно,
    /// какой именно адрес проверялся и почему он не ответил.
    var report: (title: String, body: String) {
        let title: String
        switch true {
        case inconclusive: title = "Check could not complete"
        case leaked && !tunnelUp: title = "Leak detected"
        case leaked: title = "Routes answer through the tunnel"
        default: title = "Protection works"
        }
        var lines: [String] = [verdict.isEmpty ? "(the daemon returned no verdict)" : verdict, ""]
        if !control.address.isEmpty {
            lines.append("Control \(control.address): \(control.reachable ? "reachable" : "unreachable") — \(control.detail)")
        }
        for t in blocked {
            lines.append("\(t.address): \(t.reachable ? "ANSWERS" : "blocked") — \(t.detail)")
        }
        return (title, lines.joined(separator: "\n"))
    }
}

/// Поле update из GET /status (и тело GET /update).
///
/// Демон сам решает, можно ли обновиться: он знает, есть ли рядом репозиторий,
/// чист ли он и что за версия установлена. Приложение эту логику не повторяет —
/// иначе два ответа на один вопрос неизбежно разойдутся.
struct UpdateInfo: Decodable {
    var installed: String = ""
    var latest: String = ""
    var available: Bool = false
    /// Каталог репозитория, в котором собирать новую версию.
    var repoPath: String = ""
    /// Аннотация тега — то, что показываем подсказкой у пункта меню.
    var notes: String = ""
    /// Почему обновиться нельзя. Пустая строка, когда можно.
    var reason: String = ""

    enum CodingKeys: String, CodingKey {
        case installed, latest, available, notes, reason
        case repoPath = "repo_path"
    }

    init() {}

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        installed = (try? c.decode(String.self, forKey: .installed)) ?? ""
        latest = (try? c.decode(String.self, forKey: .latest)) ?? ""
        available = (try? c.decode(Bool.self, forKey: .available)) ?? false
        repoPath = (try? c.decode(String.self, forKey: .repoPath)) ?? ""
        notes = (try? c.decode(String.self, forKey: .notes)) ?? ""
        reason = (try? c.decode(String.self, forKey: .reason)) ?? ""
    }

    /// Обновляться можно только если демон разрешил и сказал, где репозиторий:
    /// без пути собирать нечего, и пункт меню был бы кнопкой в никуда.
    var canUpdate: Bool { available && !repoPath.isEmpty && !latest.isEmpty }
}

/// Ответ POST /update.
struct UpdateResult: Decodable {
    var installed: String = ""
    var restarting: Bool = false

    enum CodingKeys: String, CodingKey { case installed, restarting }

    init() {}

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        installed = (try? c.decode(String.self, forKey: .installed)) ?? ""
        restarting = (try? c.decode(Bool.self, forKey: .restarting)) ?? false
    }
}
