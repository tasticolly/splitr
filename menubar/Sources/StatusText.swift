import Foundation

/// Человеческие формулировки состояния демона.
///
/// Вынесены отдельно от меню и от иконки, потому что одни и те же строки
/// нужны в трёх местах (подсказка иконки, шапка меню, уведомления), а сами
/// они — чистые функции от ответа демона: ни AppKit, ни сети, ни состояния.
/// Такое разделение — единственный способ вообще что-то проверять в
/// приложении, которое на 90% состоит из AppKit.
enum StatusText {
    static func tunnel(_ st: DaemonStatus) -> String {
        switch st.tunnel {
        case "up": return st.profile.isEmpty ? "up" : "up (\(st.profile))"
        case "external": return "up, started outside SplitR"
        case "starting": return "connecting…"
        case "failed": return "failed to connect"
        default: return "down"
        }
    }

    /// Короткое имя политики — для строк, которые читаются мельком
    /// (шапка меню, заголовок подменю).
    static func policyShort(_ mode: String) -> String {
        switch mode {
        case "all": return "all routes"
        case "public": return "public ranges"
        case "custom": return "custom list"
        case "off": return "nothing"
        default: return mode
        }
    }

    /// Человеческое имя политики: что именно попадает под защиту.
    static func policy(_ mode: String) -> String {
        switch mode {
        case "all": return "all routes, private ranges included"
        case "public": return "public ranges only"
        case "custom": return "custom list"
        case "off": return "nothing"
        default: return mode
        }
    }

    static func protection(_ st: DaemonStatus) -> String {
        switch st.protection {
        case "off": return "off"
        case "strict": return "strict"
        case "all", "public", "custom": return "on (\(policyShort(st.protection)))"
        default: return st.protection
        }
    }

    /// Строка о защищаемых маршрутах. Число всегда одно и то же — длина
    /// blocked_nets из ответа демона, — а вот пояснение зависит от того, что
    /// с этими маршрутами происходит.
    ///
    /// Пояснение «all going through the tunnel» уместно ровно в одном случае:
    /// туннель поднят И защита включена. При выключенной защите демон чистит
    /// якорь и присылает пустой список, так что «0 маршрутов, все идут через
    /// туннель» было бы неправдой дважды.
    static func blocking(_ st: DaemonStatus) -> String {
        let n = st.blockedNets.count
        let routes = n == 1 ? "1 route" : "\(n) routes"
        if st.protectionOff {
            return "Protected routes: none — protection is off"
        }
        if st.blocking { return "Blocked right now: \(routes)" }
        if st.trafficTunneled { return "Protected routes: \(n) (all going through the tunnel)" }
        return n == 0 ? "No protected routes configured" : "Rules cover \(routes) but block nothing right now"
    }

    static func version(_ st: DaemonStatus) -> String {
        var parts = ["Daemon: " + (st.version.isEmpty ? "unknown version" : st.version)]
        if let up = st.uptime { parts.append("uptime " + duration(up)) }
        return parts.joined(separator: ", ")
    }

    /// Строки об обновлении для подменю Advanced. Пусто, когда демон вообще
    /// ничего про обновления не сказал: старую версию демона нельзя выдавать
    /// за «обновлений нет».
    static func updateLines(_ up: UpdateInfo?) -> [String] {
        guard let up, !(up.installed.isEmpty && up.latest.isEmpty) else { return [] }
        let installed = up.installed.isEmpty ? "unknown" : up.installed
        if up.canUpdate {
            return ["Update: \(up.latest) available (installed \(installed))"]
        }
        var lines = [up.latest.isEmpty || up.latest == installed
                     ? "Update: none, \(installed) is the latest"
                     : "Update: \(up.latest) exists, installed \(installed)"]
        // Причина — самое ценное здесь: без неё серое «обновление недоступно»
        // неотличимо от поломки.
        if !up.reason.isEmpty { lines.append("   " + up.reason) }
        else if up.available && up.repoPath.isEmpty {
            lines.append("   the daemon did not say where the sources are")
        }
        return lines
    }

    /// Аптайм словами: «3 h 20 min» читается быстрее, чем 12000 секунд.
    static func duration(_ seconds: TimeInterval) -> String {
        let s = Int(seconds)
        let d = s / 86400, h = (s % 86400) / 3600, m = (s % 3600) / 60
        if d > 0 { return "\(d) d \(h) h" }
        if h > 0 { return "\(h) h \(m) min" }
        if m > 0 { return "\(m) min" }
        return "\(s) s"
    }

    /// Подсказка под курсором: то же, что в шапке меню, но без клика.
    static func tooltip(state: GuardState, status: DaemonStatus?) -> String {
        var parts = ["SplitR — " + state.title]
        if let st = status {
            parts.append("Tunnel: \(tunnel(st))")
            parts.append("Protection: \(protection(st))")
        }
        return parts.joined(separator: "\n")
    }
}
