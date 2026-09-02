import AppKit
import UserNotifications

/// Уведомления о смене состояния защиты.
///
/// Правило одно: уведомляем только на переходах. Опрос идёт раз в 2 секунды,
/// и если слать баннер по каждому ответу демона, Центр уведомлений превратится
/// в ленту из тысячи одинаковых строк за час.
final class Notifier {
    private var previous: GuardState?
    private var authorized = false
    /// Уведомления работают только у подписанного приложения с bundle id.
    /// Запущенный «голым» бинарём UNUserNotificationCenter.current() падает,
    /// поэтому доступность проверяем до первого обращения к нему.
    private let available: Bool

    init() {
        available = Bundle.main.bundleIdentifier != nil
            && Bundle.main.bundleURL.pathExtension == "app"
        guard available else { return }
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound]) { ok, _ in
            DispatchQueue.main.async { self.authorized = ok }
        }
    }

    /// Сообщает о новом состоянии. Первый вызов только запоминает состояние:
    /// баннер «защита выключена» сразу после логина — это не событие,
    /// а констатация, которую пользователь и так видит по иконке.
    func observe(_ state: GuardState) {
        defer { previous = state }
        guard let prev = previous, prev != state, state.deservesNotification else { return }

        let (title, body): (String, String)
        switch (prev, state) {
        case (_, .protected):
            (title, body) = ("Tunnel is up", "Protected routes are reachable again, protection is active.")
        case (_, .external):
            (title, body) = ("Tunnel started outside SplitR", "Traffic goes into a tunnel SplitR does not manage, so its failure will not be noticed.")
        case (.protected, .blocking), (.external, .blocking):
            (title, body) = ("Tunnel went down", "Traffic to protected routes is being blocked.")
        // Жёлтые состояния: прямо сейчас ничего не утекает, поэтому и текст
        // не про утечку, а про её возможность.
        case (_, .unguarded):
            (title, body) = ("Protection is off", "Traffic still goes through the tunnel, but nothing will block protected routes if it drops.")
        case (_, .externalUnguarded):
            (title, body) = ("Protection is off", "Traffic goes through a tunnel SplitR does not manage, and nothing will block protected routes if it drops.")
        case (_, .unprotected):
            (title, body) = ("Traffic is not protected", "No tunnel and protection is off — requests to protected routes go out directly.")
        case (_, .leaking):
            (title, body) = ("Protection is not in effect", "Protection is on, but its pf rules are not loaded and there is no tunnel — traffic may leak.")
        case (_, .daemonDown):
            (title, body) = ("SplitR daemon is not responding", "Protection state is unknown.")
        case (_, .blocking):
            (title, body) = ("No tunnel", "Traffic to protected routes is being blocked.")
        default:
            return
        }
        post(title: title, body: body)
    }

    private func post(title: String, body: String) {
        guard available, authorized else { return }
        let content = UNMutableNotificationContent()
        content.title = title
        content.body = body
        content.sound = .default
        let req = UNNotificationRequest(identifier: UUID().uuidString, content: content, trigger: nil)
        UNUserNotificationCenter.current().add(req, withCompletionHandler: nil)
    }
}
