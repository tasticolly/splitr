# Отдельный Makefile стенда: подключается из корневого через
#   -include test/docker/Makefile
# либо вызывается напрямую: make -C test/docker docker-test

.PHONY: docker-test docker-test-up docker-test-down docker-test-shell

# Полный автоматический прогон: сборка, стенд, тесты, уборка.
docker-test:
	@$(dir $(lastword $(MAKEFILE_LIST)))run.sh

# Поднять стенд и оставить его живым (для ручной отладки).
docker-test-up:
	@SPLITR_E2E_KEEP=1 $(dir $(lastword $(MAKEFILE_LIST)))run.sh

docker-test-down:
	@docker compose -f $(dir $(lastword $(MAKEFILE_LIST)))docker-compose.yml down -v --remove-orphans

docker-test-shell:
	@docker exec -it splitr-e2e-client bash
