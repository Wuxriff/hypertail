# PLAN-hypertail-pipeline

Revert коммита FIXEZ + пайплайн по образцу ponchik: Dependabot следит за версиями tailscale → merge PR → Actions собирает образ → GHCR → Watchtower на Pi обновляет контейнер. Конфиг в репо приводится к проверенному рабочему конфигу Pi.

## Выясненные факты

**Про коммит 7257bb4 (FIXEZ):**
- Это HEAD, master синхронизирован с origin, дерево чистое → revert пройдёт без конфликтов
- Что вернёт revert: старый многостадийный Dockerfile (golang:1.26-alpine → alpine:3.20, статическая сборка, ENTRYPOINT с дефолтами `-listen 0.0.0.0:8080 -state-dir /var/lib/hypertail`), README-секцию про Docker, старый .dockerignore
- Что исчезнет: compose.yaml (с health-флагами), health-watchdog в main.go (~220 строк: monitor, /healthz, self-exit), его тесты
- Важное следствие: код на Pi (образ собран 2026-06-17, до FIXEZ) — это как раз pre-revert версия. Т.е. revert приводит репозиторий к тому коду, который реально работает на Pi

**Про пайплайн ponchik (образец):**
- Dockerfile — единственное место пина версии (`FROM tailscale/tailscale:v1.102.4`), Dependabot (ecosystem: docker, weekly) открывает bump-PR
- Actions: push в main с paths на Dockerfile → читает версию из Dockerfile → buildx amd64+arm64 → push в `ghcr.io/wuxriff/ponchik-proxy:{vX, latest}` через GITHUB_TOKEN
- На Pi: compose использует `:latest`, Watchtower (poll 300s, cleanup, все контейнеры) обновляет сам

**Отличие hypertail от ponchik:** tailscale здесь — Go-зависимость (`tailscale.com v1.100.0` в go.mod, используется tsnet), а не base-образ. Поэтому «Dependabot ищет новые версии тейла» = ecosystem `gomod`, а версия для тега читается из go.mod, не из Dockerfile

**Про Pi:**
- Архитектура aarch64 → нужен linux/arm64 в buildx (как в ponchik)
- Watchtower: `nickfedor/watchtower`, WATCHTOWER_POLL_INTERVAL=300, WATCHTOWER_CLEANUP=true, без label-фильтра (следит за всеми), auth ghcr.io примонтирован из `/home/pi/.docker/config.json` → приватный пакет из GHCR подтянет без доп. настройки
- Рабочий контейнер (эталон): cmd `-listen=0.0.0.0:8080 -exit-node=barbados-proxy.tail0c919.ts.net -state-dir=/var/lib/hypertail`, env `TS_DEBUG_ALWAYS_USE_DERP=true`, порт 8080:8080, unless-stopped, bind `/home/pi/raspi_tail/tailscale-state:/var/lib/hypertail`
- Деплой на Pi живёт в `/home/pi/raspi_tail/compose.yaml` (compose-проект raspi_tail), образ сейчас локальный `raspi_tail-hypertail`
- Конфиг FIXEZ vs эталон: exit-node совпадает, но в FIXEZ нет `TS_DEBUG_ALWAYS_USE_DERP` и добавлены health-флаги, которых после revert в коде не будет

## Изменения

### 1. Revert
```
git revert --no-edit 7257bb4
```
Один чистый коммит отмены, push в master.

### 2. `.github/workflows/build.yml` (по образцу ponchik, с адаптациями)
- Триггер: push в **master** (дефолтная ветка hypertail — master, не main как в ponchik), paths: `go.mod`, `go.sum`, `Dockerfile`, workflow; + `workflow_dispatch`
- Шаг тестов: `actions/setup-go` (go-version из go.mod toolchain) → `go test ./...` — в репо есть тесты, грех не гонять перед публикацией образа
- Версия: `go list -m -f '{{.Version}}' tailscale.com` (Go уже поднят для тестов; надёжнее sed по go.mod)
- buildx: `linux/amd64,linux/arm64`
- Push: `ghcr.io/wuxriff/hypertail:v{tailscale-version}` + `:latest`, права `packages: write`, GITHUB_TOKEN

### 3. `.github/dependabot.yml`
```yaml
version: 2
updates:
  - package-ecosystem: gomod          # это и есть "новые версии тейла" (tailscale.com, сейчас v1.100.0, доступна уже v1.102.4)
    directory: /
    schedule: { interval: weekly }
  - package-ecosystem: docker         # golang:1.26-alpine / alpine:3.20
    directory: /
    schedule: { interval: weekly }
  - package-ecosystem: github-actions # версии экшенов
    directory: /
    schedule: { interval: weekly }
```
Обязателен только gomod; docker/github-actions — по желанию, легко выкинуть.

### 4. `compose.yaml` (новый, по эталону Pi)
```yaml
services:
  hypertail:
    container_name: hypertail
    image: ghcr.io/wuxriff/hypertail:latest   # вместо build: .
    environment:
      - TS_DEBUG_ALWAYS_USE_DERP=true
    ports:
      - "8080:8080"
      - "8081:8081"   # /healthz наружу в LAN: curl http://raspberrypi.local:8081/healthz
    volumes:
      - ./tailscale-state:/var/lib/hypertail
    command:
      - -listen=0.0.0.0:8080
      - -exit-node=barbados-proxy.tail0c919.ts.net
      - -state-dir=/var/lib/hypertail
      - -health-listen=0.0.0.0:8081
    restart: unless-stopped
```
Единственное отличие от эталона Pi — добавленный `-health-listen` (пассивный эндпоинт из п.6, поведение прокси не меняет).

### 5. Dockerfile — две правки
- Cross-compile через `ARG TARGETARCH` (`GOARCH=$TARGETARCH go build`): Go с CGO_ENABLED=0 кросс-компилируется нативно, иначе buildx будет эмулировать arm64 через QEMU и сборка растянется на минуты
- `HEALTHCHECK` на `wget http://127.0.0.1:8081/healthz` — только индикация `(healthy)/(unhealthy)` в `docker ps`, никаких перезапусков (autoheal-демона на Pi нет). Парный к флагу из п.6: compose всегда передаёт `-health-listen`

### 6. Минимальный /healthz (пассивный, ~60 строк вместо 380 из FIXEZ)
- Флаг `-health-listen` (по умолчанию пуст = выключен), отдельный порт, не миксовать с прокси (CONNECT-трафик)
- На запрос: `lc.Status()` → JSON `200`: backend_state, exit_node (селектор), exit_node_online, uptime; при проблемах те же поля + `503`
- Никакого rebind, счётчиков падений и self-exit — только наблюдаемость. tsnet сам реконнектится, а Watchtower при каждом обновлении образа пересоздаёт контейнер, что перевыбирает exit node; сценарий стухшего биндинга при стабильном barbados не актуален. Если когда-то случится — `docker compose restart hypertail`

### 7. README
После revert вернётся секция Docker с локальным `docker build`. Обновить её: образ берётся из GHCR, схема обновления (Dependabot bump → merge → Actions → GHCR → Watchtower на Pi) + абзац про /healthz.

## Деплой на Pi (одноразово, вручную)
1. Дождаться первого образа в GHCR (первый push после revert+пайплайна)
2. На Pi в `/home/pi/raspi_tail/compose.yaml`: `image: raspi_tail-hypertail` → `image: ghcr.io/wuxriff/hypertail:latest`, остальное не трогать (или положить туда compose.yaml из репо — они будут идентичны)
3. `docker compose pull && docker compose up -d`
4. Дальше Watchtower сам: раз в 5 минут сверяет digest, при обновлении пересоздаёт контейнер и чистит старый образ

## Порядок работ
1. Revert + push
2. Добавить workflow, dependabot.yml, новый compose.yaml, правки Dockerfile/README — один коммит «Add GHCR pipeline: dependabot on gomod, multi-arch build, compose for Pi»
3. Push → Actions собирает и публикует первый образ
4. Переключение Pi на ghcr-образ
5. Проверка сквозняка: dependabot-PR (или ручной bump tailscale.com) → merge → новый образ в GHCR → Watchtower обновил контейнер на Pi за ~5 минут

## Риски и примечания
- Self-exit костыль FIXEZ не возвращаем осознанно: tsnet сам переподключается к контролплейну, а Watchtower при каждом обновлении образа пересоздаёт контейнер (перевыбор exit node). Стухший биндинг exit node — редкий сценарий при стабильном barbados, лечится ручным restart
- Вместо него остаётся пассивный /healthz (п.6) + HEALTHCHECK в Dockerfile — наблюдаемость без вмешательства
- Тег `v1.100.0` = версия tailscale-модуля внутри образа, не версия самого hypertail
- Раз метки образа: пакет создастся при первом push; его видимость (public/private) настраивается в GitHub — на Pi auth уже есть, оба варианта будут тянуться
- Выключенный на хосте Pi tailscaled hypertail не касается: tsnet работает в userspace внутри контейнера
