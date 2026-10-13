# TikTok Gateway

Самостоятельный Go API для n8n: обнаружение подписок, webhook и последовательная
очередь текстов/стикеров. Один TikTok-аккаунт, один процесс, один постоянный volume.
Сценарии, задержки и выбор получателей принадлежат n8n.

Используется существующая сессия и недокументированный TikTok API. Принятие сессии
с IP homelab проверяется при развёртывании. Контейнеризация этого не гарантирует.

## Запуск

Публичный image: `ghcr.io/tiktok-gateway/tiktok-gateway`. Используйте версию `0.2.1`
или digest (`ghcr.io/tiktok-gateway/tiktok-gateway@sha256:…`). `main` и `sha-<полный SHA>` —
сборки разработки; `latest` указывает на стабильный релиз. Автообновления нет.

1. Скопируйте `compose.yaml`. Создайте отдельный каталог secrets вне Git.
2. Поместите существующий заголовок Cookie одной строкой в `tiktok-cookie.txt`.
   Создайте два разных случайных токена для API и webhook, каждый в отдельном файле.
3. Задайте переменные ниже и выполните `docker compose up -d`.
4. Проверьте `/readyz`, `/v1/status`, каталог стикеров и webhook.
5. Новый volume всегда начинает с **паузы отправок**. Вызовите
   `POST /v1/control/resume`, когда готовы включить очередь.

| Переменная | Значение |
|---|---|
| `TIKTOK_ACCOUNT_ID` | Обязательный числовой ID владельца, строкой |
| `TIKTOK_EXPECTED_USERNAME` | Дополнительная проверка username без `@` |
| `STICKER_SOURCE_USER_ID` | ID собеседника в исходном диалоге со стикерами |
| `WEBHOOK_URL` | Один production URL n8n; пустой — события копятся |
| `TIKTOK_COOKIE_FILE` | Файл Cookie внутри контейнера |
| `API_TOKEN_FILE` | Файл Bearer-токена входящего API |
| `WEBHOOK_TOKEN_FILE` | Файл отдельного Bearer-токена webhook |
| `DATA_DIR` | `/data`, SQLite, WAL и lock |
| `POLL_SECONDS` | `45`, в live минимум 30 |
| `LISTEN_ADDR` | `:8080`; healthcheck ожидает порт 8080 |
| `TIKTOK_MODE` | `live` по умолчанию; `mock` только для изолированных тестов |

Compose использует host-переменные `TIKTOK_COOKIE_PATH`, `API_TOKEN_PATH`,
`WEBHOOK_TOKEN_PATH` для путей secrets и `TIKTOK_GATEWAY_IMAGE` для версии/digest.
Файлы должны быть доступны UID 65532 внутри контейнера. В Linux Compose bind-mounted
secrets сохраняют права исходного файла: настройте владельца/ACL для UID 65532,
не делайте секреты общедоступными. Каталог `/data` в image принадлежит UID 65532;
при bind mount подготовьте права отдельно. Сервис не требует root, capabilities или shell.

Пример включает только backend и публикует порт на `127.0.0.1`. Сеть до n8n,
reverse proxy и TLS настройте в своём homelab. Для доступа из другого контейнера
подключите общую сеть и используйте `http://tiktok-gateway:8080`.
Передачу токенов через недоверенную сеть защищайте HTTPS.

## API

Контракт: [OpenAPI](docs/openapi.json). Все `/v1/*` требуют
`Authorization: Bearer <API token>`. `/healthz` и `/readyz` без токена, без деталей.

```http
POST /v1/messages
Authorization: Bearer <API token>
Idempotency-Key: <eventId>:welcome-v1:text-1
Content-Type: application/json

{"recipientUserId":"1000000000000000002","type":"text","text":"Привет!"}
```

Стикер: `{"recipientUserId":"1000000000000000002","type":"sticker","stickerId":"123"}`.
ID передаются **строками**, иначе JavaScript может потерять точность.

`202` возвращает `operationId` и текущую операцию: это принятие команды, не доставка.
`GET /v1/operations/{id}` возвращает `queued → sending → verifying → succeeded`,
`failed` или `uncertain`. `succeeded` означает подтверждение по истории, не прочтение.

Одинаковый ключ и команда возвращают ту же операцию, даже завершённую. Другая
команда с тем же ключом — `409`. Хранятся хеш ключа и хеш канонического тела;
дедупликация действует пока сохранена БД. Удаление volume уничтожает эту защиту.
Другой ключ разрешает повторить тот же текст. Подписка получателя не обязательна;
TikTok всё равно может запретить диалог.

До внешней отправки состояние `sending` фиксируется с SQLite `synchronous=FULL`.
Неоднозначный результат и прерывание процесса после этой границы дают `uncertain`
и паузу. Автоматической повторной отправки нет. `POST /v1/operations/{id}/reconcile`
только читает историю, ищет точный server message ID, отправителя и текст/тип.
При отсутствии server ID или доказательства доставки операция остаётся `uncertain`.
Ограничение поиска — 20 страниц истории. **Exactly-once не обещается.**
API не содержит принудительного сброса неопределённой операции: если доказательства
недоступны, очередь остаётся заблокированной, потребуется отдельное расследование.
Не меняйте ключ для обхода неопределённости.

`failed` с `recipient_unavailable` не останавливает другие команды. Подробный код
TikTok читается из `check_message.status_code`: ограничения получателя (7281,
7282, 7283, 7290, 7291, 7394) завершают только его операцию. Ограничения отправителя
и неизвестные отказы `tiktok_rejected_send` / `tiktok_rejected_conversation`
ставят сервис на паузу `tiktok_restriction`. Общая пара `status=3, check=2` сама
по себе не означает бан аккаунта и не определяет, кому запрещена отправка.

При явном отказе операция сохраняет числовую диагностику, например
`tiktokRejection: {"status": 3, "check": 2, "businessCode": 7283}`.
`businessCode` — необязательный подробный код, не HTTP-статус. Он возвращается API
и переживает перезапуск. Старые операции нельзя переклассифицировать по двум
общим числам: подробный код ранее не сохранялся. Неудавшиеся операции не
повторяются автоматически. Ошибки сессии до отправки оставляют команду в очереди
и включают паузу. Сырой `check_message` и другие ответы TikTok не сохраняются,
не возвращаются и не логируются.

`POST /v1/control/pause` сохраняет паузу; уже начатый внешний запрос не отменяется.
`resume` проверяет сессию, baseline и отсутствие неопределённых операций. Пауза
переживает restart. Она не останавливает опрос подписок или webhook.
`/readyz` показывает готовность сессии и baseline; пауза отправок не делает процесс
неработоспособным. Ошибки webhook отражаются отдельно в `/v1/status`.

## Подписки и webhook

Первый успешный опрос нового volume создаёт контрольную точку без событий по старым
подпискам. Последующие события дедуплицируются по ID уведомления TikTok. Новая
переподписка с новым notice ID — отдельное событие, даже для того же пользователя.

```json
{"schemaVersion":"1","eventId":"stable-event-id","type":"follower.created",
 "accountId":"1000000000000000001","occurredAt":"2026-10-13T12:00:00Z",
 "detectedAt":"2026-10-13T12:01:00Z",
 "data":{"userId":"1000000000000000002","username":"example"}}
```

Событие, outbox и checkpoint сохраняются одной транзакцией до HTTP-запроса.
Заголовки: `Authorization: Bearer <webhook token>`, `X-Event-ID: <eventId>`.
`2xx` подтверждает принятие webhook, не завершение workflow. Доставка **at-least-once**:
при таймауте после принятия webhook или аварии до записи ответа возможен повтор.
Повтор сохраняет исходные ID и времена. n8n сам решает, обрабатывать ли запоздалое событие.
Ошибки сети, 429 и 5xx повторяются с интервалом 2 секунды → максимум 1 час.
Числовой Retry-After учитывается до 24 часов. Ошибки URL/авторизации сохраняются в
статусе и повторяются раз в час; тело ответа не сохраняется. Redirect не выполняется.
При отсутствии URL события остаются в outbox. После исправления env перезапустите
контейнер, очередь продолжится. События и завершённые операции автоматически не удаляются;
следите за размером volume. Опрос ограничен доступной историей TikTok; при неполной
пагинации checkpoint не продвигается, статус показывает `follower_poll_failed`.

## n8n

Ноды поддерживаются в отдельном репозитории
[n8n-nodes-tiktok-gateway](https://github.com/TikTok-Gateway/n8n-nodes-tiktok-gateway)
и устанавливаются из [npm](https://www.npmjs.com/package/n8n-nodes-tiktok-gateway).
В self-hosted n8n откройте **Settings → Community Nodes → Install**, введите
`n8n-nodes-tiktok-gateway` и подтвердите установку. Сборка собственного образа
n8n не нужна. Пакет не является verified-нодой для n8n Cloud.

Создайте одно подключение **TikTok Gateway** (URL API, API token и отдельный
webhook token) и выберите его в трёх нодах. Скачайте и импортируйте
[пример workflow из репозитория пакета](https://github.com/TikTok-Gateway/n8n-nodes-tiktok-gateway/blob/main/examples/welcome.json):
**New Follower → Wait → Send Text → Send Sticker**.
HTTP-запросы и ожидание подтверждённого `succeeded` выполняются внутри нод.

Для совместного локального запуска используйте
[`compose.n8n.yaml`](compose.n8n.yaml) поверх основной конфигурации:

```sh
docker compose -f compose.yaml -f compose.n8n.yaml up -d
```

Предварительно задайте `TIKTOK_ACCOUNT_ID` и подготовьте три файла secrets из
раздела «Запуск». Эта команда запускает публичный образ gateway из GHCR; n8n использует
официальный образ и постоянный volume для workflows, credentials и npm-пакетов.
Стенд работает с реальным TikTok-аккаунтом. n8n доступен на http://localhost:5678,
API — на http://localhost:8080. На новом volume отправки остаются на паузе до
осознанного `resume`. Установите пакет через Community Nodes после первого запуска.

Для триггера Events задайте в `.env`
`WEBHOOK_URL=http://n8n:5678/webhook/tiktok-events-gateway` и `RECEIVE_MESSAGES=true`.
Без переопределения `WEBHOOK_URL` используется путь триггера New Follower.

[Настройка, обновление и ограничения](docs/N8N.md).

Получайте `userId` из `follower.created.data.userId` или доверенного источника
числовых TikTok ID; API не принимает username вместо ID.
`GET /v1/stickers` возвращает ID исходных сообщений владельца в настроенном диалоге
и защищённые URL preview. Получите preview с API Bearer-токеном. CDN URL наружу не
выдаётся. Выбранный source ID перечитывается перед отправкой; при исчезновении
источника операция завершается ошибкой, другой стикер не подставляется.

## Миграция, backup и обновление

См. [инструкцию](docs/MIGRATION.md). Старый процесс и новый gateway не должны
одновременно отправлять от одного аккаунта. Lock защищает только один volume на
одном хосте; не создавайте вторую реплику/копию volume для того же аккаунта.

Остановленный volume сохраняйте целиком, включая WAL/SHM, с ограниченными правами:
там содержатся тексты команд, ID пользователей и история. Для обновления сохраните
backup, задайте новую версию/digest, выполните `docker compose pull` и
`docker compose up -d`. Не используйте `down -v`. При несовместимой схеме откатывайте
image вместе с соответствующим backup, а не только бинарник.

## Разработка и проверки

```sh
go test -race ./...
go vet ./...
(cd third_party/matrix-tiktok && go test -race ./...)
docker build -t tiktok-gateway:smoke .
python3 scripts/smoke.py
python3 scripts/audit_public.py
```

Smoke test запускает отдельный Compose project с mock TikTok и webhook-приёмником,
проверяет очередь, дубликаты, сохранение после пересоздания контейнера и webhook.
Ни один тест не читает реальные cookies и не отправляет реальные сообщения.
Unit-тесты нод и smoke-тест с настоящим n8n и mock gateway выполняются
[в CI npm-пакета](https://github.com/TikTok-Gateway/n8n-nodes-tiktok-gateway/actions).
Runtime image содержит только gateway, CA-сертификаты и стороннюю лицензию.
Старый локальный UI, персональные probes и настройки исключены из Git и build context.
`internal/greeter` сохранён как legacy-библиотека для протокола и формата миграции;
его движок сценариев новым сервисом не запускается.

CI: PR — тесты, smoke и multiarch build без публикации; main — `main` и `sha-*`;
`v*` — версия, стабильная версия также `latest`. Публикация использует `GITHUB_TOKEN`
после проверок. Видимость GHCR package настраивается отдельно на Public.

Сторонний клиент: [MIT license](third_party/matrix-tiktok/LICENSE).
Входящие сообщения, `message.received`, AI — вне этой версии.

### Incoming messages and history

The gateway now supports opt-in `message.received` webhooks (`RECEIVE_MESSAGES=true`)
and read-only `GET /v1/conversations/{userId}/messages?cursor=...`. First enablement
baselines existing messages without replay. History is paginated; callers must
check `exhausted` and `hasGaps` before inferring an opening message. No dialog
stages or reply decisions are stored in gateway. See [n8n setup](docs/N8N.md#входящие-сообщения-и-история)
and [OpenAPI](docs/openapi.json). Do not enable incoming delivery with the legacy
follower-only receiver: use the new Events trigger and its production URL.
