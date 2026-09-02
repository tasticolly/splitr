//go:build !darwin

package netmon

import "context"

// watchRoutes на не-Darwin ничего не слушает.
//
// Маршрутный сокет PF_ROUTE — механизм BSD; в Linux ту же роль играет netlink,
// но splitr там существует только внутри тестового стенда, где сеть за время
// прогона не меняется. Ловля пробуждения из сна переносима и остаётся включённой,
// а страховочный тикер сторожа работает везде одинаково.
func watchRoutes(ctx context.Context, m *Monitor) {
	<-ctx.Done()
}
