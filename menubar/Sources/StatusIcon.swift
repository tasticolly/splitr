import AppKit

/// Отрисовка символа состояния для строки меню.
///
/// Отдельно от GuardState: состояние — это чистое перечисление, которое можно
/// проверять без AppKit, а всё, что умеет вернуть nil и зависеть от версии
/// системы, живёт здесь.
enum StatusIcon {
    /// Размер символа подобран под фиксированную ширину пункта (24 pt):
    /// 13 pt дают картинку 17×15, которая помещается с полями и не заставляет
    /// систему расширять пункт.
    private static let pointSize: CGFloat = 13

    /// Красим символ палитрой, а не template-режимом: template-иконка всегда
    /// чёрно-белая, а нам нужен именно цвет как основной сигнал.
    ///
    /// Возвращает nil, если символа нет в этой версии macOS. Это не мелочь:
    /// имена SF Symbols появляются в разных выпусках, и на более старой системе
    /// nil здесь означал бы пустую кнопку — то есть «приложение не установлено»
    /// с точки зрения пользователя. Обработку nil берёт на себя StatusItemController.
    /// `appearance` — оформление той самой кнопки в строке меню. Цвет
    /// состояния зависит от темы, а картинку мы собираем не в момент
    /// отрисовки, а по таймеру: без явного указания оформления динамический
    /// цвет разрешился бы по теме приложения, и на светлой строке меню
    /// оказался бы вариант для тёмной — то есть светлый значок на светлом.
    static func image(for state: GuardState, appearance: NSAppearance? = nil) -> NSImage? {
        guard let base = NSImage(systemSymbolName: state.symbol,
                                 accessibilityDescription: state.title) else {
            return nil
        }
        var tint = state.tint
        if let appearance {
            appearance.performAsCurrentDrawingAppearance {
                tint = state.tint.usingColorSpace(.sRGB) ?? state.tint
            }
        }
        let cfg = NSImage.SymbolConfiguration(pointSize: pointSize, weight: .regular)
            .applying(NSImage.SymbolConfiguration(paletteColors: [tint]))
        let img = base.withSymbolConfiguration(cfg) ?? base
        img.isTemplate = false
        return img
    }
}
