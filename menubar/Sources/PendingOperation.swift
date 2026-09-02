import Foundation

/// Чем кончилась начатая человеком операция с туннелем.
enum PendingOutcome {
    /// Демон подтвердил успех. Строка — отчёт для шапки меню.
    case succeeded(String)
    /// Демон сказал, что не вышло.
    case failed(String)
    /// Не вышло, но причина известна и поправима руками: например, удалённый
    /// хост требует заново войти по ссылке.
    case needsAction(ActionRequired)
    /// Демон за отведённое время так ничего и не сказал.
    case timedOut(String)
}

/// Оптимистичное состояние: человек нажал Connect или Disconnect, а демон ещё
/// не подтвердил результат.
///
/// Зачем оно вообще. Подъём туннеля занимает от трёх секунд до десятков, а
/// опрос демона идёт раз в две секунды: между кликом и первым изменением в
/// интерфейсе проходило столько, что человек успевал решить, будто клик не
/// сработал, и нажать ещё раз — а каждое нажатие уходило в демон отдельным
/// POST /up. Поэтому «подключаюсь» рисуется в тот же миг, когда по пункту
/// кликнули, из локального состояния, и не ждёт ответа демона.
///
/// Главное свойство этого состояния — оно обязано заканчиваться. Залипшее
/// навсегда «подключение» врёт про защиту трафика, а это хуже, чем полное
/// отсутствие обратной связи. Отсюда `timeout` и `outcome(for:)`: всё, что
/// не кончилось подтверждением или ошибкой, кончается по времени.
struct PendingOperation {
    enum Kind { case connecting, disconnecting }

    let kind: Kind
    /// Профиль, через который поднимаем. Для disconnecting пуст.
    let profile: String
    let startedAt: Date
    /// Демон хотя бы раз ответил «starting». Без этого признака нельзя
    /// отличить «команда ещё не дошла до демона» (tunnel: down сразу после
    /// клика) от «sshuttle стартовал и умер» (tunnel: down после starting).
    var sawDaemonStart = false

    /// Предел ожидания. Взят с запасом: обычный подъём идёт 3–10 секунд, но с
    /// перелогином Tailscale и медленным ssh бывает заметно дольше, а ложное
    /// «не получилось» на живом подключении — худший из возможных отчётов.
    static let timeout: TimeInterval = 45

    init(kind: Kind, profile: String = "", startedAt: Date = Date()) {
        self.kind = kind
        self.profile = profile
        self.startedAt = startedAt
    }

    func expired(_ now: Date = Date()) -> Bool {
        now.timeIntervalSince(startedAt) >= Self.timeout
    }

    /// Что показывать иконкой и заголовком меню, пока операция идёт.
    var state: GuardState {
        kind == .connecting ? .connecting : .disconnecting
    }

    /// Строка отчёта в шапке меню.
    var label: String {
        switch kind {
        case .connecting:
            return profile.isEmpty ? "connecting…" : "connecting through \(profile)…"
        case .disconnecting:
            return "disconnecting…"
        }
    }

    /// Причина, по которой повторный клик сейчас бессмыслен.
    var blockReason: String {
        switch kind {
        case .connecting:
            return "SplitR is already bringing the tunnel up. It usually takes a few seconds."
        case .disconnecting:
            return "SplitR is already taking the tunnel down."
        }
    }

    /// Относится ли нынешнее состояние туннеля к попытке, которую начали мы.
    ///
    /// Два независимых признака, и оба нужны. Отметка времени точна, но её
    /// может не быть (старый демон, туннель никогда не запускался); признак
    /// «видели starting» есть всегда, но между опросами короткий провал можно
    /// и пропустить. Вместе они закрывают обе дыры.
    private func startedThisAttempt(_ st: DaemonStatus) -> Bool {
        if sawDaemonStart { return true }
        guard let since = st.sinceDate else { return false }
        return since >= startedAt
    }

    /// Чем кончилось, судя по свежему ответу демона. nil — ещё идёт.
    ///
    /// Функция чистая нарочно: это единственное место, где решается, когда
    /// оптимистичное состояние снимается, и его надо уметь проверить без
    /// живого демона и без AppKit.
    func outcome(for st: DaemonStatus, now: Date = Date()) -> PendingOutcome? {
        switch kind {
        case .connecting:
            // Требование войти по ссылке разбираем первым: это не безымянный
            // отказ, а конкретное действие, которое человек может выполнить.
            if let action = st.actionRequired { return .needsAction(action) }
            if st.trafficTunneled {
                return .succeeded(profile.isEmpty ? "connected" : "connected through \(profile)")
            }
            // «Не поднялся» засчитываем только тогда, когда речь про нашу
            // попытку. Демон держит failed до следующего запуска, а до старта
            // sshuttle успевает потратить секунды на чужие процессы, DNS и
            // правила — то есть сразу после клика он честно отвечает failed
            // от прошлого раза. Без этой проверки клик по Connect после
            // неудачи мгновенно рисовал бы ошибку, которой ещё не произошло.
            if (st.tunnelFailed || st.tunnel == "down") && startedThisAttempt(st) {
                let reason = st.lastError.flatMap { $0.isEmpty ? nil : $0 }
                return .failed(reason ?? "the tunnel did not come up")
            }
        case .disconnecting:
            if !st.trafficTunneled && !st.tunnelIsStarting {
                return .succeeded("tunnel stopped")
            }
        }
        guard expired(now) else { return nil }
        switch kind {
        case .connecting:
            return .timedOut("the tunnel did not come up within \(Int(Self.timeout)) seconds")
        case .disconnecting:
            return .timedOut("the tunnel did not go down within \(Int(Self.timeout)) seconds")
        }
    }
}
