# Подключение n8n

Импортируйте `examples/n8n-workflow.json` вручную. Workflow по умолчанию выключен.
Нужны стандартные Webhook, Wait, Edit Fields, HTTP Request, If и Stop And Error.

1. В Webhook создайте **Header Auth** credential: Name `Authorization`, Value
   `Bearer <отдельный webhook token>`. Он должен совпадать с `WEBHOOK_TOKEN_FILE`.
2. Для всех четырёх HTTP Request нод выберите другой **Header Auth** credential:
   Name `Authorization`, Value `Bearer <API token>`. Токены в экспорт не вставляйте.
3. В Configuration задайте `gatewayUrl`, текст и `stickerId` из `GET /v1/stickers`.
   Посмотрите `/v1/stickers/{id}/preview` с тем же API credential перед выбором.
4. При необходимости измените первую Wait. После каждой отправки Wait + GET
   опрашивают операцию каждые 5 секунд. Продолжение допускается только при succeeded.
   failed, uncertain и неизвестные состояния заканчиваются Stop And Error с причиной.
   Ошибка HTTP также останавливает execution. Пока queued/sending/verifying, опрос
   продолжается; настройте timeout execution в n8n под ожидаемую длительность паузы.
5. Включите workflow и укажите его **Production URL** в gateway `WEBHOOK_URL`.

Test URL работает только когда n8n слушает тестовый webhook в редакторе. Production
URL предназначен для включённого/опубликованного workflow. Не оставляйте gateway
на Test URL после отладки. Нода отвечает сразу после принятия; gateway не ждёт Wait
и окончания workflow. Ошибка позднее в n8n не возвращает событие автоматически:
используйте n8n retry execution с исходным webhook payload.

Ключи: `eventId:welcome-v1:text-1` и `eventId:welcome-v1:sticker-1`. Они не зависят
от ID execution. Повторное событие создаёт ещё один execution, но API возвращает
прежние операции обоим executions. Оба могут дойти до стикера, однако его ключ
также один, поэтому повторной отправки нет. Если одна операция uncertain, обе
ветки останавливаются. Не меняйте ключ для обхода ошибки. При изменении текста
уже обработанного события прежний ключ даст 409 — это намеренная защита.

Сохраняйте постоянные ID сценария/действий между перезапусками и импортами.
Новый ID сценария означает новые команды для того же eventId. При добавлении
нового действия дайте ему отдельный постоянный ID. Логику «один раз приветствовать
пользователя за всё время», маршрутизацию событий, фильтр давности по `occurredAt`
и любые AI-решения добавляйте в n8n. Пример принимает все follower.created версии 1.

Выключение workflow прекращает приём новых webhook по production URL, но не
отменяет уже принятые операции gateway и не гарантирует остановку активных
executions n8n. Для общей остановки отправок используйте `/v1/control/pause`.
При выключенном workflow события могут накопиться в gateway и прийти позднее.

Официальные справочники:
- [Webhook: test и production URL](https://docs.n8n.io/integrations/builtin/core-nodes/n8n-nodes-base.webhook/)
- [HTTP Request credentials](https://docs.n8n.io/integrations/builtin/credentials/httprequest/)
- [Wait](https://docs.n8n.io/integrations/builtin/core-nodes/n8n-nodes-base.wait/)
