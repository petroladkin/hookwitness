# Як поділитися налаштуванням ендпоінта

Налаштування ендпоінта — це все, від чого залежить, як він відповідає: його
шлях, відповідь за замовчуванням, правила, перевірки, змінні, схеми і те,
які секрети йому потрібні. Hookwitness записує його у файл
(`<path>.hookwitness.json`) і створює ендпоінти з такого файлу на будь-якому
екземплярі. Два звичні сценарії:

- **Розробник готує, QA тестує.** Розробник знає, як має поводитися
  приймач (підпис Stripe, дві відмови перед успіхом, щоб перевірити
  повтори), налаштовує ендпоінт один раз і передає файл. QA імпортує його
  і починає тестувати; руками нічого не відтворюють.
- **QA додає його до тікета.** Поруч зі [звітом](reports.md) файл каже, як
  відправника тестували, і будь-хто може імпортувати його, щоб відтворити.

## Що є у файлі, а чого там ніколи немає

У файлі є шлях і поведінка ендпоінта, а для кожного секрету — **ім'я і
вид** (ключ підпису або токен чи пароль). Значення секрету, його хешу,
запечатаної форми чи відбитка там ніколи немає: значення лишається на
екземплярі, де його ввели. Запити, лічильник, стан увімкнено/вимкнено та
ідентифікатори теж не входять у налаштування.

Ось файл `/payments/webhooks`: обов'язкова перевірка підпису Stripe із
секретом `STRIPE_SECRET`, а перші дві доставки отримують `503`, щоб було
видно повтори відправника.

```json
{
  "hookwitness": 1,
  "endpoints": [
    {
      "path": "/payments/webhooks",
      "default": {"status": 200},
      "rules": [
        {"when": {"kind": "first", "n": 2}, "do": "reply",
         "reply": {"status": 503, "headers": [{"name": "Retry-After", "value": "2"}]}}
      ],
      "checks": [{"kind": "stripe", "enforce": true, "secret": "STRIPE_SECRET"}],
      "vars": [],
      "schemas": [],
      "secrets": [{"name": "STRIPE_SECRET", "kind": "key"}]
    }
  ]
}
```

`"hookwitness": 1` позначає файл і його формат. Файл новішого Hookwitness,
файл із полем, якого цей не знає, або взагалі не файл налаштування
відхиляється з повідомленням, що саме не так, і нічого не створюється.

## Експорт

- Один ендпоінт: відкрийте його сторінку **Behaviour** і натисніть
  **Export setup**.
- Усі одразу (ціла інтеграція, наприклад події й колбеки): **Export all** на
  сторінці Endpoints.

Браузер зберігає файл. API віддає той самий файл
(`GET <prefix>/api/v1/setup?id=<id>`, без `id` — усі):

<!-- docs-check: setup: payments -->
```sh
ID=$(curl -s 'http://127.0.0.1:8080/admin/api/v1/endpoints?path=/payments/webhooks' | jq '.endpoints[0].id')
curl -s -o payments-webhooks.hookwitness.json "http://127.0.0.1:8080/admin/api/v1/setup?id=$ID"
jq -c '.endpoints[0] | {path, secrets}' payments-webhooks.hookwitness.json
# {"path":"/payments/webhooks","secrets":[{"name":"STRIPE_SECRET","kind":"key"}]}
```

## Імпорт

Натисніть **Import** на сторінці Endpoints, виберіть файл (або вставте його
вміст) і натисніть **Review**. Поки що нічого не створено: перегляд показує
кожен ендпоінт файлу з тим, що він робить, і питає про дві речі.

![Перегляд імпорту: зайнятий шлях і секрет без значення](../images/setup-import.webp)

- **Куди він іде.** Кожен ендпоінт зберігає свій шлях, якщо ви не введете
  інший. Коли шлях тут уже зайнятий ендпоінтом, позначте **replace its
  behaviour** (його запити, лічильник і стан увімкнено/вимкнено
  лишаються; секрет із тим самим ім'ям і видом зберігає своє значення) або
  імпортуйте під іншим шляхом.
- **Значення секретів, якщо вони у вас є.** Введіть свій тестовий ключ
  зараз або залиште порожнім і задайте пізніше. Секрет, залишений
  порожнім, **не задано** (*not set*).

**Import** створює всі ендпоінти файлу або жодного, якщо щось не так.

### Незадані секрети

Імпортований секрет без значення позначено *not set* на вкладці **Secrets**
ендпоінта (з кнопкою **Set value** замість Replace), біля кожної
перевірки, що його використовує, у його реєстрі та на сторінці Endpoints.
Перевірка з таким секретом дає **n/v** з причиною
`secret STRIPE_SECRET not set` і нічого не відхиляє, навіть обов'язкова:
запит іде далі до правил, наче перевірки немає, а вердикт пояснює, чому
вона нічого не каже.

![Секрет, імпортований без значення](../images/setup-unset.webp)

Задайте значення на вкладці Secrets (або через API), і перевірки
працюватимуть з наступного запиту; **Re-check** дасть вердикти запитам,
упійманим тим часом.

Той самий імпорт через API, на новому екземплярі:

<!-- docs-check: fresh; setup: payments_file -->
```sh
curl -s -X POST http://127.0.0.1:8080/admin/api/v1/setup -H 'Content-Type: application/json' \
  --data-binary @payments-webhooks.hookwitness.json | jq -c '.endpoints[] | {result, path, secrets_not_set}'
# {"result":"created","path":"/payments/webhooks","secrets_not_set":["STRIPE_SECRET"]}
curl -s -o /dev/null http://127.0.0.1:8080/payments/webhooks -d '{"id":"evt_1"}' # n/v: secret STRIPE_SECRET not set
```

Зайнятий шлях відхиляється (`409`), якщо виклик не каже
`?on_conflict=replace`:

<!-- docs-check: setup: payments payments_file -->
```sh
curl -s -X POST http://127.0.0.1:8080/admin/api/v1/setup -H 'Content-Type: application/json' \
  --data-binary @payments-webhooks.hookwitness.json | jq -r .error.message
# /payments/webhooks is already an endpoint; nothing was imported…
curl -s -X POST 'http://127.0.0.1:8080/admin/api/v1/setup?on_conflict=replace' -H 'Content-Type: application/json' \
  --data-binary @payments-webhooks.hookwitness.json | jq -c '.endpoints[] | {result, secrets_not_set}'
# {"result":"unchanged","secrets_not_set":[]}
```

Заміна зберегла значення, яке мав `STRIPE_SECRET`, тому нічого не змінилося.

## Відомі ендпоінти без кліків

Завдання CI чи `docker run` може стартувати з потрібними ендпоінтами.

**На кожному старті:** `-import <file>` (або `HOOKWITNESS_IMPORT`) застосовує
файл до того, як сервер почне слухати. Його ендпоінти створюються або їхня
поведінка замінюється, тож перезапуск із тим самим файлом нічого не
змінює, а значення, які хтось задав для секретів, лишаються. Файл, який не
можна застосувати, зупиняє старт одним рядком із причиною.

<!-- docs-check: skip: Docker -->
```sh
docker run -d --name hookwitness -p 127.0.0.1:8080:8080 -v hookwitness:/data \
  -v "$PWD/payments-webhooks.hookwitness.json:/setup.json:ro" -e HOOKWITNESS_IMPORT=/setup.json \
  ghcr.io/petroladkin/hookwitness:0.1
```

**На запущеному сервері:** `hookwitness apply <file>` робить те саме через
API і каже, що зробив із кожним ендпоінтом:

<!-- docs-check: fresh; setup: payments_file -->
```sh
hookwitness apply -url http://127.0.0.1:8080 payments-webhooks.hookwitness.json
# created /payments/webhooks (secrets not set: STRIPE_SECRET)
hookwitness apply -url http://127.0.0.1:8080 payments-webhooks.hookwitness.json
# unchanged /payments/webhooks (secrets not set: STRIPE_SECRET)
```

З увімкненим входом дайте йому токен: `-token-file` називає файл, у якому
він лежить (див. [Користувачі, вхід і токени](access.md#токени-для-скриптів-і-агентів));
імпортувати може будь-який користувач. Для адмінки в корені власного
порту дайте повну адресу API: `-url http://127.0.0.1:8081/api/v1`.

AI-агент має те саме через MCP: `export_endpoint` і `import_endpoints`
([Тестування відправника з AI-агентом](agents.md)).
