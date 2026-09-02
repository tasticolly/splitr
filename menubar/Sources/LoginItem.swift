import Foundation
import ServiceManagement

/// Автозапуск при логине.
///
/// Правильный способ на macOS 13+ — SMAppService.mainApp: система сама
/// показывает приложение в «Настройки → Основные → Объекты входа», пользователь
/// может выключить его там, а не искать плист в ~/Library/LaunchAgents.
/// Так делают зрелые проекты (Ice через LaunchAtLogin-Modern, Stats — руками
/// в Kit/helpers.swift с тем же SMAppService.mainApp.status).
///
/// Почему LaunchAgent всё же остаётся запасным путём. SMAppService привязывает
/// регистрацию к подписи бандла, а мы подписываемся ad-hoc: у такой подписи нет
/// устойчивого удостоверения, и после каждой пересборки регистрация может
/// отвалиться с «Operation not permitted». Плист в ~/Library/LaunchAgents
/// такой привязки не имеет и работает всегда — ценой того, что пользователь
/// не найдёт приложение в списке объектов входа.
enum LoginItem {
    enum State: String {
        case enabled = "enabled"
        case notRegistered = "not registered"
        case requiresApproval = "waiting for the user to approve it in System Settings"
        case notFound = "the system does not know about it"
        case unknown = "unknown"
    }

    static var launchAgentPlist: URL {
        FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Library/LaunchAgents/com.splitr.menubar.plist")
    }

    static var hasLaunchAgent: Bool {
        FileManager.default.fileExists(atPath: launchAgentPlist.path)
    }

    static var state: State {
        switch SMAppService.mainApp.status {
        case .enabled: return .enabled
        case .notRegistered: return .notRegistered
        case .requiresApproval: return .requiresApproval
        case .notFound: return .notFound
        @unknown default: return .unknown
        }
    }

    /// Регистрирует приложение как объект входа. Бросает — значит, путь
    /// не сработал и вызывающий (install.sh) должен положить LaunchAgent.
    static func register() throws {
        // Повторная регистрация уже включённого сервиса возвращает ошибку,
        // поэтому сначала снимаем. Тот же приём в Stats (Kit/helpers.swift).
        if SMAppService.mainApp.status == .enabled {
            try? SMAppService.mainApp.unregister()
        }
        try SMAppService.mainApp.register()
    }

    static func unregister() throws {
        try SMAppService.mainApp.unregister()
    }

    /// Строка для журнала: по ней видно, каким именно способом приложение
    /// стартует, и не задвоился ли автозапуск.
    static func describe() -> String {
        var parts = ["login item (SMAppService): \(state.rawValue)"]
        parts.append("LaunchAgent: \(hasLaunchAgent ? "present" : "none")")
        if state == .enabled && hasLaunchAgent {
            parts.append("WARNING: both mechanisms are active — the app may start twice")
        }
        return parts.joined(separator: ", ")
    }
}
