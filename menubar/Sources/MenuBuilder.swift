import AppKit

/// Команда меню. Раньше каждый пункт нёс свой @objc-селектор, и построение
/// меню намертво срасталось с делегатом. Одно перечисление разрывает эту связь:
/// строитель меню знает только, какие команды бывают, а кто и как их исполняет —
/// его не касается.
enum MenuAction {
    case up(profile: String)
    case update
    case checkUpdate
    case showUpdateProgress
    case down
    case protection(mode: String)   // on | off | strict
    case policy(String)             // all | public | custom | off
    case probe
    case editConfig
    case showRules
    case reload
    case openConfig
    case showBlocked
    case showLog
    case openWeb
    case showKickstart
    case copyKickstart
    case refresh
    /// Открыть ссылку, по которой демон просит войти (action_required).
    case openAuthURL(String)
    case quit
}

/// Коробка для representedObject: тот хранит только объекты, а MenuAction —
/// перечисление со значениями.
final class MenuActionBox: NSObject {
    let action: MenuAction
    init(_ action: MenuAction) { self.action = action }
}

/// Всё, из чего строится меню. Отдельная структура вместо чтения полей делегата:
/// меню становится функцией от снимка состояния, и его нельзя случайно
/// построить наполовину из свежих данных, наполовину из устаревших.
struct MenuModel {
    let state: GuardState
    let status: DaemonStatus?
    let config: DaemonConfig?
    let lastError: APIError?
    let lastAction: String?
    /// Есть ли управляющий сокет. Без него правку конфига сохранить некуда,
    /// и честнее показать пункт серым, чем принять текст и уронить его в ошибку.
    let socketPresent: Bool
    /// Текущий шаг обновления, если оно идёт прямо сейчас (building, installing
    /// the daemon, …). Пока он не nil, все действия меню выключены: команда
    /// демону посреди подмены его бинаря — верный способ получить непонятную
    /// ошибку в момент, когда и так всё меняется.
    var updateStage: String? = nil
    /// Начатая, но не подтверждённая операция с туннелем. Пока она идёт,
    /// Connect и Disconnect неактивны: второй POST /up ничего не ускоряет,
    /// а человек, не видя реакции, жмёт именно так — несколько раз подряд.
    var pending: PendingOperation? = nil
    /// Команда демону, ответ на которую ещё не пришёл (смена защиты, политики,
    /// перечитывание конфига, проверка). Не мгновенные, но и не длинные — им
    /// хватает надписи в шапке и погашенных пунктов, отдельного вида иконки
    /// они не заслуживают.
    var busy: String? = nil
}

/// Сборка NSMenu из снимка состояния. Никаких запросов и побочных эффектов:
/// строитель только раскладывает пункты.
///
/// Два правила определяют раскладку.
/// 1. Корень меню — только то, чем пользуются каждый день: одно контекстное
///    действие с туннелем и защита. Всё редкое («покажи правила», «поправь
///    конфиг», «логи») уехало в подменю Advanced, чтобы не мозолить глаза.
/// 2. Бессмысленное сейчас не прячется, а гаснет. Прятать нельзя: меню, где
///    пункты то есть, то нет, невозможно использовать по мышечной памяти —
///    рука промахивается. Поэтому у каждого серого пункта есть toolTip
///    с причиной: серый пункт без объяснения читается как поломка.
enum MenuBuilder {
    /// Политики блокировки с человеческим пояснением. Разница между all и public
    /// не умозрительная: all режет и приватные сети, поэтому в чужом Wi-Fi
    /// на 10.x или 192.168.0.x без туннеля ляжет вообще весь интернет.
    static let policies: [(String, String, String)] = [
        ("all", "All routes, private ranges included",
         "Strongest coverage. Also blocks 10/172.16/192.168: on someone else's Wi-Fi with that addressing the whole internet goes down while the tunnel is off."),
        ("public", "Public ranges only",
         "Blocks only the public ranges where your real IP would be visible. Local networks and foreign Wi-Fi keep working."),
        ("custom", "Custom list from the configuration",
         "Blocks exactly what is listed under protection.block in the configuration."),
        ("off", "Nothing (unsafe)",
         "Protection rules stop blocking anything: without a tunnel, requests to protected routes go out over the plain internet."),
    ]

    static func build(into menu: NSMenu, model: MenuModel, target: AnyObject, selector: Selector) {
        let ctx = Context(model: model, target: target, selector: selector)
        menu.removeAllItems()

        // Обновление — самое верхнее, что есть в меню, и появляется оно только
        // когда обновляться действительно есть куда. Пункта «обновлений нет»
        // здесь нет нарочно: строка, которая всю жизнь говорит «нет», просто
        // отодвигает вниз всё полезное.
        if let stage = model.updateStage {
            menu.addItem(dim("Updating… (\(stage))"))
            menu.addItem(ctx.item("Show progress…", .showUpdateProgress))
            menu.addItem(.separator())
        } else if let up = model.status?.update, up.canUpdate {
            let it = ctx.item("Update to \(up.latest)…", .update)
            it.toolTip = updateTooltip(up)
            menu.addItem(it)
            menu.addItem(.separator())
        }

        summary(menu, ctx)
        menu.addItem(.separator())
        var actions = primaryAction(ctx)
        // Починка стоит рядом с проблемой. Выключенная защита — единственное
        // состояние, которое человек чинит одним кликом, и заставлять его
        // искать этот клик в подменю, пока в заголовке написано «страховки
        // нет», значит показать проблему и спрятать решение.
        if let st = ctx.model.status, st.protectionOff {
            let it = ctx.item("Turn protection on", .protection(mode: "on"))
            it.toolTip = st.trafficTunneled
                ? "Block protected routes whenever the tunnel is down, including if this one drops."
                : "Block protected routes whenever the tunnel is down."
            actions.append(it)
        }
        for it in actions { menu.addItem(it) }
        menu.addItem(.separator())
        menu.addItem(protectionSubmenu(ctx))
        menu.addItem(advancedSubmenu(ctx))
        if ctx.model.status == nil {
            // Единственное, что имеет смысл при мёртвом демоне, — поднять его.
            menu.addItem(.separator())
            menu.addItem(ctx.item("How to start the daemon…", .showKickstart))
            menu.addItem(ctx.item("Copy start command", .copyKickstart))
            menu.addItem(ctx.item("Check again", .refresh))
        }
        menu.addItem(.separator())
        menu.addItem(ctx.item("Quit SplitR", .quit))

        // Пока обновление идёт, живыми остаются только «показать прогресс»
        // и «выйти»: всё остальное либо обращается к перезапускающемуся демону,
        // либо запускает вторую сборку поверх первой.
        if model.updateStage != nil {
            disableActions(in: menu, reason: "SplitR is updating itself right now.")
        } else if let busy = model.busy {
            // Ответа демона ещё нет, и повторный клик по любому пункту либо
            // отменит только что отданную команду, либо ляжет поверх неё.
            disableActions(in: menu, reason: "SplitR is waiting for the daemon: \(busy)")
        }
    }

    /// Подсказка пункта обновления: аннотация тега, если демон её прислал.
    private static func updateTooltip(_ up: UpdateInfo) -> String {
        var parts = ["Installed \(up.installed.isEmpty ? "version unknown" : up.installed), available \(up.latest)."]
        if !up.notes.isEmpty { parts.append(up.notes) }
        parts.append("SplitR is rebuilt from \(up.repoPath), then the daemon and the menu bar app are replaced.")
        return parts.joined(separator: "\n\n")
    }

    private static func disableActions(in menu: NSMenu, reason: String) {
        for it in menu.items {
            if let sub = it.submenu {
                disableActions(in: sub, reason: reason)
                it.isEnabled = false
                continue
            }
            guard let box = it.representedObject as? MenuActionBox else { continue }
            switch box.action {
            case .quit, .showUpdateProgress, .openAuthURL:
                continue
            default:
                it.action = nil
                it.isEnabled = false
                it.toolTip = reason
            }
        }
    }

    // MARK: - шапка

    /// Верхняя часть меню: одна фраза-вердикт и под ней подробности.
    /// Порядок строк — от «что со мной сейчас» к деталям, версия демона
    /// уехала в Advanced: раз в год она нужна, а место занимает всегда.
    private static func summary(_ menu: NSMenu, _ ctx: Context) {
        menu.addItem(ctx.header(ctx.model.state.title))
        guard let st = ctx.model.status else {
            if let e = ctx.model.lastError { menu.addItem(dim(e.localizedDescription)) }
            menu.addItem(dim("SplitR cannot tell whether traffic is protected."))
            return
        }
        // Состояние защиты не дублируем строкой: оно уже стоит в заголовке
        // подменю Protection ниже, а два одинаковых текста в одном меню
        // читаются как ошибка.
        menu.addItem(dim("Tunnel: \(StatusText.tunnel(st))"))
        menu.addItem(dim(StatusText.blocking(st)))
        // Что именно значит жёлтое состояние — одной строкой, рядом с
        // заголовком: «сейчас безопасно, страховки нет».
        switch ctx.model.state {
        case .unguarded, .externalUnguarded:
            menu.addItem(dim("If the tunnel drops, protected routes go out in the clear."))
        default:
            break
        }
        // Предупреждения — только настоящие. Незагруженный якорь при сознательно
        // выключенной защите ожидаем: демон сам чистит правила, когда защиту
        // выключили. Тревога — это когда защита включена, а правил в ядре нет.
        if !st.rulesUnloadedOnPurpose {
            if !st.pfEnabled { menu.addItem(dim("⚠︎ pf is disabled")) }
            if !st.anchorLoaded { menu.addItem(dim("⚠︎ the SplitR pf anchor is not loaded")) }
            if !st.anchorLinked { menu.addItem(dim("⚠︎ the anchor is not linked from /etc/pf.conf")) }
        }
        if let err = st.lastError, !err.isEmpty { menu.addItem(dim("Error: " + err)) }
        for w in st.warnings { menu.addItem(dim("⚠︎ " + w)) }
        // Требование войти по ссылке — не «ошибка», а работа для человека,
        // поэтому оно идёт со ссылкой, которую можно открыть отсюда же:
        // сообщение «нужно заново войти» без ссылки — это тупик.
        if let action = st.actionRequired {
            menu.addItem(dim("⚠︎ " + action.text))
            if action.link != nil {
                let it = ctx.item("Open the sign-in link", .openAuthURL(action.url))
                it.toolTip = action.url
                menu.addItem(it)
            }
        }
        if let last = ctx.model.lastAction { menu.addItem(dim("→ " + last)) }
    }

    // MARK: - главное действие

    /// Одно контекстное действие вместо пары «поднять / опустить».
    ///
    /// Держать обе команды рядом бессмысленно: одна из них всегда серая, и
    /// человеку приходится каждый раз читать, какая именно. Слот один, меню
    /// не прыгает, а название прямо отвечает, что произойдёт по клику.
    private static func primaryAction(_ ctx: Context) -> [NSMenuItem] {
        /// Серый пункт всегда идёт с причиной строкой ниже: подсказка под
        /// курсором помогает только тому, кто догадался там задержаться,
        /// а главное действие обязано объясняться сразу.
        func blocked(_ title: String, _ reason: String, short: String) -> [NSMenuItem] {
            [ctx.disabled(title, because: reason), dim("   " + short)]
        }
        // Начатая операция перебивает всё остальное: пока она идёт, любой
        // пункт здесь либо повторит уже отданную команду, либо отменит её.
        // Пункты не прячем, а гасим — иначе меню под рукой перестраивается
        // ровно в тот момент, когда человек в него целится.
        if let p = ctx.model.pending {
            switch p.kind {
            case .connecting:
                var items = blocked(p.profile.isEmpty ? "Connecting…" : "Connecting (\(p.profile))…",
                                    p.blockReason, short: "already in progress")
                if let sub = connectToItem(ctx, disabledBecause: p.blockReason) { items.append(sub) }
                return items
            case .disconnecting:
                return blocked("Disconnecting…", p.blockReason, short: "already in progress")
            }
        }
        guard let st = ctx.model.status else {
            return blocked("Connect", "The SplitR daemon is not responding, so it cannot bring a tunnel up.",
                           short: "the daemon is not responding")
        }
        // Туннель поднимает не наше нажатие, а, например, CLI или прошлый
        // запуск — состояние то же самое, и вести себя меню должно так же.
        if st.tunnelIsStarting {
            let reason = "SplitR is already bringing the tunnel up."
            var items = blocked("Connecting…", reason, short: "already in progress")
            if let sub = connectToItem(ctx, disabledBecause: reason) { items.append(sub) }
            return items
        }
        if st.external {
            return blocked("Disconnect", "This tunnel was started outside SplitR, so SplitR cannot take it down. Stop it where it was started.",
                           short: "this tunnel is not managed by SplitR")
        }
        if st.tunnelIsUp {
            let it = ctx.item("Disconnect", .down)
            // Что будет после отключения, зависит от защиты, и обещать
            // блокировку при выключенной защите нельзя — там как раз ничего
            // заблокировано не будет.
            if st.protectionOff {
                it.toolTip = "Stop the tunnel. Protection is off, so afterwards requests to protected routes go out over the plain internet."
            } else if st.strictMode {
                it.toolTip = "Stop the tunnel. Protected routes stay blocked — strict mode blocks them either way."
            } else {
                it.toolTip = "Stop the tunnel. Protected routes will be blocked instead."
            }
            return [it]
        }

        let names = ctx.model.config?.profileNames ?? []
        if names.isEmpty {
            return blocked("Connect", "No profiles in the configuration — nothing to connect to.",
                           short: "no profiles in the configuration")
        }
        // Connect — обычное действие, а не подменю. Родительский пункт с
        // подменю в AppKit по клику не срабатывает вовсе, поэтому «подключись
        // уже куда обычно» превращалось в наведение и второй выбор. Теперь
        // профиль по умолчанию стоит прямо в заголовке (видно, куда поднимут),
        // а остальные уехали в соседний пункт.
        let fallback = ctx.model.config?.defaultProfile ?? ""
        // Демон при пустом default_profile и единственном профиле берёт его —
        // повторяем это правило, иначе меню было бы строже самого демона.
        let preferred = names.contains(fallback) ? fallback : (names.count == 1 ? names[0] : "")

        var items: [NSMenuItem] = []
        if preferred.isEmpty {
            items += blocked("Connect", "There are several profiles and no default_profile in the configuration, so SplitR cannot tell which one to bring up. Pick one under \"Connect to…\".",
                             short: "no default profile — pick one below")
        } else {
            let it = ctx.item("Connect (\(preferred))", .up(profile: preferred))
            it.toolTip = "Bring the tunnel up through \(ctx.profileTitle(preferred))."
            items.append(it)
        }
        if let sub = connectToItem(ctx, disabledBecause: nil) { items.append(sub) }
        return items
    }

    /// Пункт «Connect to…» со списком профилей. nil, когда профиль один:
    /// подменю с единственной строкой, повторяющей строку выше, ничего
    /// не сообщает.
    ///
    /// `disabledBecause` гасит и сам пункт, и все профили внутри: во время
    /// подключения выбор второго профиля — это второй POST /up поверх первого.
    private static func connectToItem(_ ctx: Context, disabledBecause reason: String?) -> NSMenuItem? {
        let names = ctx.model.config?.profileNames ?? []
        guard names.count > 1 else { return nil }
        let fallback = ctx.model.config?.defaultProfile ?? ""
        let preferred = names.contains(fallback) ? fallback : ""

        let parent = NSMenuItem(title: "Connect to…", action: nil, keyEquivalent: "")
        // Заголовок подменю задаём явно: под этим именем его видят VoiceOver
        // и Accessibility API, а без имени подменю для них безымянное.
        let sub = NSMenu(title: parent.title)
        sub.autoenablesItems = false
        for name in names {
            // Профиль по умолчанию помечен словом, а не галочкой: галочка в
            // списке действий читается как «сейчас выбран», а выбран здесь
            // никто — туннеля нет.
            let title = ctx.profileTitle(name) + (name == preferred ? "  (default)" : "")
            sub.addItem(reason.map { ctx.disabled(title, because: $0) } ?? ctx.item(title, .up(profile: name)))
        }
        parent.submenu = sub
        if let reason {
            parent.isEnabled = false
            parent.toolTip = reason
        }
        return parent
    }

    // MARK: - защита

    private static func protectionSubmenu(_ ctx: Context) -> NSMenuItem {
        guard let st = ctx.model.status else {
            return ctx.disabled("Protection — daemon not responding",
                                because: "The SplitR daemon is not responding, so protection cannot be changed.")
        }
        let parent = NSMenuItem(title: "Protection: \(StatusText.protection(st))", action: nil, keyEquivalent: "")
        let sub = NSMenu(title: parent.title)
        sub.autoenablesItems = false

        for (mode, title, hint) in [
            ("on", "On", "Block protected routes whenever the tunnel is down."),
            ("off", "Off (unsafe)", "Never block anything: without a tunnel, requests to protected routes go out over the plain internet."),
            ("strict", "Strict", "Block protected routes at all times, even while the tunnel is up."),
        ] {
            let active = currentMode(st) == mode
            let it = active
                ? ctx.disabled(title, because: "This is the current setting.")
                : ctx.item(title, .protection(mode: mode))
            if !active { it.toolTip = hint }
            it.state = active ? .on : .off
            sub.addItem(it)
        }

        sub.addItem(.separator())
        // Пока туннель поднят, резать нечего, и выбор списка сейчас ничего не
        // меняет — серым он не отвлекает от главного и не создаёт иллюзию,
        // будто клик что-то сделает с текущим трафиком.
        let policyBlock: (short: String, long: String)?
        if st.protectionOff {
            // Выключенная защита — причина важнее живого туннеля: пока она
            // выключена, список маршрутов не значит ничего вообще, а туннель
            // рано или поздно опустят.
            policyBlock = ("locked while protection is off",
                           "Protection is off, so no routes are blocked. Turn protection on first.")
        } else if st.trafficTunneled {
            policyBlock = ("locked while the tunnel is up",
                           "The tunnel is up, so nothing is being blocked right now. Disconnect first to change which routes are protected.")
        } else {
            policyBlock = nil
        }
        sub.addItem(dim(policyBlock.map { "Protected routes — " + $0.short } ?? "Protected routes"))
        for (policy, title, hint) in policies {
            let active = st.mode == policy
            let it: NSMenuItem
            if let reason = policyBlock {
                it = ctx.disabled(title, because: reason.long)
            } else if active {
                it = ctx.disabled(title, because: "This is the current setting.")
            } else {
                it = ctx.item(title, .policy(policy))
                it.toolTip = hint
            }
            it.state = active ? .on : .off
            sub.addItem(it)
        }

        parent.submenu = sub
        return parent
    }

    /// on/off/strict из ответа демона: в поле protection он смешивает режим
    /// и политику (all|public|custom означают «включено»).
    private static func currentMode(_ st: DaemonStatus) -> String {
        if st.strictMode { return "strict" }
        return st.protectionOff ? "off" : "on"
    }

    // MARK: - редкое

    /// Всё, куда заходят раз в месяц: диагностика, конфиг, логи. В корне меню
    /// эти восемь пунктов занимали больше места, чем всё остальное вместе.
    private static func advancedSubmenu(_ ctx: Context) -> NSMenuItem {
        guard let st = ctx.model.status else {
            return ctx.disabled("Advanced — daemon not responding",
                                because: "The SplitR daemon is not responding: rules, logs and configuration all come from it.")
        }
        let parent = NSMenuItem(title: "Advanced", action: nil, keyEquivalent: "")
        let sub = NSMenu(title: parent.title)
        sub.autoenablesItems = false

        sub.addItem(dim(StatusText.version(st)))
        // Версии и причина отказа живут рядом с версией демона: когда пункта
        // «Update to …» в корне нет, это единственное место, где видно, почему
        // его нет — «обновления нет» и «обновиться нельзя» выглядят одинаково.
        for line in StatusText.updateLines(st.update) { sub.addItem(dim(line)) }
        if !st.configPath.isEmpty { sub.addItem(dim("Configuration: " + st.configPath)) }
        sub.addItem(ctx.item("Refresh now", .refresh))
        sub.addItem(ctx.item("Check for updates now", .checkUpdate))

        sub.addItem(.separator())
        let probe = ctx.item("Test protection…", .probe)
        probe.toolTip = "Try to reach protected routes and report whether anything leaks."
        sub.addItem(probe)
        sub.addItem(ctx.item("Show pf rules…", .showRules))
        if ctx.model.config?.packetLogEnabled == false {
            sub.addItem(ctx.disabled("Blocked packets (live) — packet logging is off",
                                     because: "Turn on protection.log in the configuration and reload it, then the live stream will work."))
        } else {
            sub.addItem(ctx.item("Blocked packets (live)…", .showBlocked))
        }
        sub.addItem(ctx.item("Log…", .showLog))

        sub.addItem(.separator())
        if ctx.model.socketPresent {
            sub.addItem(ctx.item("Edit configuration…", .editConfig))
        } else {
            sub.addItem(ctx.disabled("Edit configuration — no control socket",
                                     because: "The control socket is not available, so an edited configuration could not be saved from here."))
        }
        sub.addItem(ctx.item("Reload configuration", .reload))
        if st.configPath.isEmpty {
            sub.addItem(ctx.disabled("Open configuration in an external editor — path unknown",
                                     because: "The daemon did not report where the configuration file lives."))
        } else {
            sub.addItem(ctx.item("Open configuration in an external editor", .openConfig))
        }

        sub.addItem(.separator())
        sub.addItem(ctx.item("Open web interface", .openWeb))

        parent.submenu = sub
        return parent
    }

    /// Информационная строка: некликабельная и приглушённая, чтобы визуально
    /// отделяться от команд.
    private static func dim(_ text: String) -> NSMenuItem {
        let it = NSMenuItem(title: text, action: nil, keyEquivalent: "")
        it.attributedTitle = NSAttributedString(
            string: text,
            attributes: [.font: NSFont.menuFont(ofSize: 12),
                         .foregroundColor: NSColor.secondaryLabelColor])
        it.isEnabled = false
        return it
    }

    private struct Context {
        let model: MenuModel
        let target: AnyObject
        let selector: Selector

        func item(_ title: String, _ action: MenuAction) -> NSMenuItem {
            let it = NSMenuItem(title: title, action: selector, keyEquivalent: "")
            it.target = target
            it.representedObject = MenuActionBox(action)
            it.isEnabled = true
            return it
        }

        /// Серый пункт с обязательной причиной. Причина не опциональна нарочно:
        /// пункт, который не работает и не объясняет почему, неотличим от бага.
        func disabled(_ title: String, because reason: String) -> NSMenuItem {
            let it = NSMenuItem(title: title, action: nil, keyEquivalent: "")
            it.isEnabled = false
            it.toolTip = reason
            return it
        }

        func header(_ text: String) -> NSMenuItem {
            let it = NSMenuItem(title: text, action: nil, keyEquivalent: "")
            it.attributedTitle = NSAttributedString(
                string: text,
                attributes: [.font: NSFont.systemFont(ofSize: 13, weight: .semibold),
                             .foregroundColor: model.state.tint])
            it.isEnabled = false
            return it
        }

        func profileTitle(_ name: String) -> String {
            guard let remote = model.config?.profiles[name]?.remote, !remote.isEmpty else { return name }
            return "\(name) — \(remote)"
        }
    }
}
