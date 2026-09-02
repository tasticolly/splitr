import Foundation

/// Всё, что приложение умеет спросить у демона.
///
/// Протокол здесь не ради «архитектурности»: он проводит границу между
/// единственным местом с побочными эффектами (сеть, unix-сокет, запуск curl)
/// и остальным приложением. Делегат зависит от этого набора операций, а не от
/// URLSession, поэтому меню и состояние можно поднять с подставной реализацией
/// и посмотреть на них без живого демона.
protocol GuardAPI: AnyObject {
    func status(_ completion: @escaping (Result<DaemonStatus, APIError>) -> Void)
    func config(_ completion: @escaping (Result<DaemonConfig, APIError>) -> Void)
    func rules(_ completion: @escaping (Result<String, APIError>) -> Void)
    func up(profile: String, _ completion: @escaping (Result<DaemonStatus, APIError>) -> Void)
    func down(_ completion: @escaping (Result<DaemonStatus, APIError>) -> Void)
    func protection(mode: String?, policy: String?, _ completion: @escaping (Result<DaemonStatus, APIError>) -> Void)
    func configRaw(_ completion: @escaping (Result<String, APIError>) -> Void)
    func log(tail: Int, _ completion: @escaping (Result<String, APIError>) -> Void)
    func writeConfig(_ yaml: String, socketPath: String, _ completion: @escaping (Result<DaemonStatus, APIError>) -> Void)
    func reload(_ completion: @escaping (Result<DaemonStatus, APIError>) -> Void)
    func probe(_ completion: @escaping (Result<ProbeReport, APIError>) -> Void)
    func updateInfo(_ completion: @escaping (Result<UpdateInfo, APIError>) -> Void)
}

extension APIClient: GuardAPI {}
