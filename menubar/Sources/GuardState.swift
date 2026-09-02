import AppKit

/// Сведённое состояние системы — то, что должно читаться по иконке за долю секунды.
///
/// Демон отдаёт полдюжины независимых флагов (tunnel/protection/blocking/pf...),
/// но пользователю важны два разных вопроса, и путать их нельзя:
///   1. «утекает ли трафик прямо сейчас» — только это красный;
///   2. «есть ли страховка, если туннель упадёт» — это жёлтый, но не красный.
/// Пока туннель жив, трафик идёт в него, и выключенная защита опасна не сейчас,
/// а потом. Поэтому вся логика сведения флагов живёт здесь, а не размазана по меню.
enum GuardState {
    /// Туннель поднят нами, защита на страже — штатный режим.
    case protected
    /// Туннель поднят мимо splitr (руками или старым скриптом): трафик идёт
    /// в туннель, защита под ним есть, но падением туннеля демон не управляет.
    case external
    /// Туннель поднят нами, но защита выключена: трафик в туннеле, а страховки
    /// под ним нет — упадёт туннель, и всё пойдёт напрямую.
    case unguarded
    /// То же, но туннель ещё и не наш: ни страховки, ни управления.
    case externalUnguarded
    /// Туннеля нет, но трафик режется — безопасно, просто нет доступа.
    case blocking
    /// Strict: режем protected routes безусловно, даже при живом туннеле.
    case strict
    /// Туннеля нет и защита выключена — трафик уходит напрямую. Опасно сейчас.
    case unprotected
    /// Туннеля нет, защита включена, но правила не действуют
    /// (pf выключен, якорь отвалился) — утечка при защите «на бумаге».
    case leaking
    /// Демон не отвечает: что происходит с трафиком — неизвестно.
    case daemonDown
    /// Туннель поднимается прямо сейчас. Состояние переходное: либо демон сам
    /// сказал «starting», либо человек только что нажал Connect и мы показываем
    /// это, не дожидаясь ответа демона (см. PendingOperation).
    case connecting
    /// Туннель опускается прямо сейчас — по той же причине переходное.
    case disconnecting

    /// Порядок проверок отвечает ровно на вопрос «уйдёт ли трафик мимо туннеля
    /// прямо сейчас»: сначала смотрим, куда идёт трафик, и только потом — есть
    /// ли под ним страховка. Обратный порядок и давал красное «Not protected»
    /// при живом туннеле.
    static func from(_ st: DaemonStatus) -> GuardState {
        // Подъём туннеля перебивает все прочие признаки: пока он идёт, любое
        // «итоговое» состояние — неправда, которая через несколько секунд
        // сменится другой. Защиту демон накладывает до старта sshuttle,
        // поэтому переходное состояние не прячет утечку: правила уже в ядре.
        if st.tunnelIsStarting { return .connecting }
        if st.strictMode {
            // Strict режет маршруты всегда, поэтому опасен он только в одном
            // случае: правил в ядре нет, и туннеля тоже нет.
            return (st.trafficTunneled || st.rulesInEffect) ? .strict : .leaking
        }
        if st.trafficTunneled {
            if st.protectionOff { return st.external ? .externalUnguarded : .unguarded }
            return st.external ? .external : .protected
        }
        if st.protectionOff { return .unprotected }
        return st.rulesInEffect ? .blocking : .leaking
    }

    /// SF Symbol для строки меню. Формы выбраны так, чтобы отличаться силуэтом,
    /// а не только цветом: часть людей цвет в menu bar не различает,
    /// плюс в тёмной теме оттенки сливаются.
    ///
    /// Замок (закрытый/открытый) означает «трафик в туннеле», щит — «маршруты
    /// режутся правилами». Заливка отличает наш туннель от чужого.
    var symbol: String {
        switch self {
        case .protected:         return "lock.shield.fill"
        case .external:          return "lock.shield"
        case .unguarded:         return "lock.open.fill"
        case .externalUnguarded: return "lock.open"
        case .blocking:          return "shield.fill"
        case .strict:            return "exclamationmark.octagon.fill"
        case .unprotected:       return "shield.slash.fill"
        case .leaking:           return "exclamationmark.triangle.fill"
        case .daemonDown:        return "questionmark.circle"
        // Переходные состояния — своим силуэтом: круговые стрелки не похожи
        // ни на замок, ни на щит, поэтому «идёт работа» видно, не вчитываясь.
        case .connecting:        return "arrow.triangle.2.circlepath"
        case .disconnecting:     return "arrow.down.circle"
        }
    }

    /// Красный — только там, где трафик может уйти с машины прямо сейчас.
    /// Жёлтый — «сейчас безопасно, но страховки нет».
    ///
    /// Системные NSColor.system* сюда не годятся, хотя и выглядят уместно.
    /// Они рассчитаны на заливки — на кнопку, переключатель, кружок, — а не
    /// на текст и не на тонкий символ поверх светлого фона. Померено: жёлтый
    /// #FFCC00 на светлом фоне меню даёт контраст 1.45:1, то есть надпись
    /// «Tunnel up — but no protection if it drops» физически не читается;
    /// зелёный даёт 2.03, бирюзовый 2.45, оранжевый 2.10 — не лучше.
    /// Поэтому тон системного цвета сохранён, а яркость подобрана под фон:
    /// на светлой теме темнее, на тёмной светлее. Все пары проверены
    /// расчётом контраста, цифры — в таблице ниже.
    ///
    ///   состояние            светлая тема      тёмная тема
    ///   protected            #0E8320  4.7:1    #32D74B  7.6:1
    ///   external             #367A8D  4.6:1    #6AC4DC  7.3:1
    ///   unguarded            #8A6E00  4.7:1    #FFD60A 10.3:1
    ///   blocking             #006EE6  4.6:1    #3495FF  4.6:1
    ///   strict               #A249CE  4.6:1    #C073E8  4.6:1
    ///   unprotected/leaking  #D92A21  4.7:1    #FF5C53  4.6:1
    ///   connecting           #A66100  4.7:1    #FF9F0A  7.1:1
    ///   daemonDown           #6E6E6E  5.0:1    #A8A8A8  6.1:1
    var tint: NSColor {
        switch self {
        case .protected:         return Self.tone(light: 0x0E8320, dark: 0x32D74B)
        case .external:          return Self.tone(light: 0x367A8D, dark: 0x6AC4DC)
        case .unguarded:         return Self.tone(light: 0x8A6E00, dark: 0xFFD60A)
        case .externalUnguarded: return Self.tone(light: 0x8A6E00, dark: 0xFFD60A)
        case .blocking:          return Self.tone(light: 0x006EE6, dark: 0x3495FF)
        case .strict:            return Self.tone(light: 0xA249CE, dark: 0xC073E8)
        case .unprotected:       return Self.tone(light: 0xD92A21, dark: 0xFF5C53)
        case .leaking:           return Self.tone(light: 0xD92A21, dark: 0xFF5C53)
        // Здесь напрашивался secondaryLabelColor, и для текста он бы сгодился.
        // Но тем же цветом рисуется значок в строке меню, а этот цвет
        // полупрозрачный: тонкий вопросительный знак на светлой строке меню
        // давал 1.6:1, то есть «демон не отвечает» выглядело как пустое место.
        // Непрозрачный серый той же роли — 5.0:1 и 6.1:1.
        case .daemonDown:        return Self.tone(light: 0x6E6E6E, dark: 0xA8A8A8)
        // Оранжевый не занят ни одним устойчивым состоянием — он и означает
        // «сейчас ничего не решено, идёт переход».
        case .connecting, .disconnecting: return Self.tone(light: 0xA66100, dark: 0xFF9F0A)
        }
    }

    /// Цвет, зависящий от темы. Именно динамический NSColor, а не «спросить
    /// тему один раз и запомнить»: строка меню и меню перерисовываются
    /// системой, в том числе при смене темы на лету, и запомненный цвет
    /// остался бы от прошлой темы — то есть снова нечитаемым.
    private static func tone(light: Int, dark: Int) -> NSColor {
        NSColor(name: nil) { appearance in
            let isDark = appearance.bestMatch(from: [.aqua, .darkAqua]) == .darkAqua
            return rgb(isDark ? dark : light)
        }
    }

    private static func rgb(_ v: Int) -> NSColor {
        NSColor(srgbRed: CGFloat((v >> 16) & 0xFF) / 255,
                green: CGFloat((v >> 8) & 0xFF) / 255,
                blue: CGFloat(v & 0xFF) / 255,
                alpha: 1)
    }

    /// Первая строка меню. Одной фразой объясняет положение: куда идёт трафик
    /// и чем это подстраховано. Слово «Not protected» оставлено ровно двум
    /// состояниям, в которых трафик действительно может уйти напрямую.
    var title: String {
        switch self {
        case .protected:         return "Protected — traffic goes through the tunnel"
        case .external:          return "Protected — tunnel started outside SplitR"
        case .unguarded:         return "Tunnel up — but no protection if it drops"
        case .externalUnguarded: return "Tunnel up outside SplitR — no protection if it drops"
        case .blocking:          return "Protected — no tunnel, protected routes are blocked"
        case .strict:            return "Protected — strict mode, protected routes always blocked"
        case .unprotected:       return "Not protected — no tunnel, traffic goes out directly"
        case .leaking:           return "Not protected — protection is on but its rules are not in effect"
        case .daemonDown:        return "Unknown — SplitR daemon is not responding"
        case .connecting:        return "Connecting — bringing the tunnel up"
        case .disconnecting:     return "Disconnecting — taking the tunnel down"
        }
    }

    /// Текстовая метка на случай, когда SF Symbol не нашёлся.
    /// Одна-три буквы: в строке меню больше не поместится, а различать
    /// состояния всё равно нужно.
    var badge: String {
        switch self {
        case .protected:         return "SR"
        case .external:          return "SR?"
        case .unguarded:         return "OPEN"
        case .externalUnguarded: return "OPEN?"
        case .blocking:          return "SR!"
        case .strict:            return "!!"
        case .unprotected:       return "OFF"
        case .leaking:           return "!"
        case .daemonDown:        return "?"
        case .connecting:        return "..."
        case .disconnecting:     return "v"
        }
    }

    /// Состояния, о переходе в которые есть смысл уведомлять.
    var deservesNotification: Bool {
        switch self {
        case .protected, .external, .unguarded, .externalUnguarded,
             .unprotected, .leaking, .blocking, .daemonDown: return true
        // О переходных состояниях не уведомляем: баннер «подключаюсь» через
        // секунду после клика сообщает человеку то, что он только что сделал
        // сам, а следом всё равно придёт баннер о результате.
        case .strict, .connecting, .disconnecting: return false
        }
    }

    /// Переходное состояние: работа идёт, результат ещё не известен.
    /// Иконка в этом случае пульсирует — процесс должен быть виден
    /// из строки меню, не открывая её.
    var isTransient: Bool {
        switch self {
        case .connecting, .disconnecting: return true
        default: return false
        }
    }
}
