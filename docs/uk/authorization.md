# Перевірки автентичності й секрети

Чи правильно підписано цей вебхук і чи несе він правильний токен? Перевірка
автентичності відповідає на це для кожного запиту **вердиктом**:

- **pass** — пройшов;
- **fail** — не пройшов, з причиною в один рядок, наприклад
  `signature does not match`, `timestamp 601 s old (tolerance 300 s)` або
  `header Authorization missing`;
- **n/v** — неможливо перевірити: тіло не збережене або секрет неможливо
  розшифрувати (див. [Вердикти й повторна перевірка](endpoints.md#вердикти-й-повторна-перевірка)).

Перевірці потрібні дві речі на сторінці **Behaviour** ендпоінта: **секрет**
(ключ підпису чи токен) на вкладці **Secrets** і **перевірка**, що ним
користується, на вкладці **Checks**. Сам секрет нічого не робить.

## Захистити ендпоінт токеном, крок за кроком

Відправник надсилає `Authorization: Bearer tok_test_123` на `/hooks`, і ви
хочете бачити вердикт для кожного запиту.

1. Відкрийте сторінку **Behaviour** ендпоінта `/hooks` і вкладку **Secrets**.
   Натисніть **New secret**.
2. Назвіть його `API_TOKEN`, виберіть **token or password (bearer, basic)**,
   вставте `tok_test_123` і збережіть. Відтепер значення більше ніколи не
   показується, лише його відбиток (fingerprint).

   ![Діалог New secret з API_TOKEN](../images/secret-new.webp)

3. Відкрийте вкладку **Checks** і натисніть **New check**. Виберіть тип
   **Bearer token** і секрет `API_TOKEN`. Лишіть **Enforce** вимкненим, щоб
   лише записувати вердикти, або ввімкніть, щоб відмовляти запитам, які не
   пройшли.

   ![Діалог New check із Bearer token і API_TOKEN](../images/check-new.webp)

4. Натисніть **Save check**. Наступний запит отримає вердикт:

<!-- docs-check: setup: bearer -->
```sh
H=http://127.0.0.1:8080/hooks
curl -s -d '{"order":42}' -H 'Authorization: Bearer tok_test_123' $H   # pass
curl -s -d '{"order":42}' -H 'Authorization: Bearer wrong' $H          # fail: token does not match
curl -s -d '{"order":42}' $H                                           # fail: header Authorization missing
```

Реєстр рахує вердикти, і кожен рядок показує свій. Стрічка малює червону
риску під запитами, що не пройшли. Сторінка другого запиту наводить його
причину, `token does not match`:

![Сторінка другого запиту з вердиктом Bearer token fail](../images/auth-fail.webp)

## Перевірки

| Перевірка | Що перевіряє |
|---|---|
| **GitHub signature** | `X-Hub-Signature-256: sha256=<hex HMAC-SHA256 сирого тіла>` |
| **Stripe signature** | `Stripe-Signature: t=<unix>,v1=<hex HMAC-SHA256 від "<t>.<body>">`. Підійти може будь-який `v1`; позначка часу має бути в межах допуску. |
| **Standard Webhooks / Svix signature** | `webhook-id`, `webhook-timestamp` і `webhook-signature` (або `svix-…`). Підпис — `v1,<base64 HMAC-SHA256 від "<id>.<timestamp>.<body>">` під base64-ключем після `whsec_`; позначка часу має бути в межах допуску. |
| **Generic HMAC signature** | Будь-який заголовок: sha1, sha256 чи sha512, hex чи base64, необов'язковий префікс (`sha1=` покриває старий `X-Hub-Signature` GitHub). Підписаний вміст — шаблон: типово сире тіло, або, наприклад, `{{.Header "X-Id"}}.{{.Body}}`. |
| **Bearer token** | `Authorization: Bearer <token>` |
| **Basic credentials** | `Authorization: Basic base64(user:password)`, ім'я користувача задано в перевірці |

Підписам потрібен секрет **signing key** (ключ підпису). Bearer і basic
потребують секрету **token or password** (токен чи пароль).

![Вкладка Checks ендпоінта /hooks з перевіркою Bearer token](../images/checks.webp)

### Приклад для кожної

Кожен блок передбачає ендпоінт `/hooks` із секретом і перевіркою, названими
просто перед ним. `make docs-check` запускає їх на свіжому сервері.

**GitHub.** Секрет `GH_SECRET` — ключ підпису, `gh_test_secret`; перевірка —
**GitHub signature**.

<!-- docs-check: setup: github -->
```sh
H=http://127.0.0.1:8080/hooks
BODY='{"action":"opened","number":7}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac gh_test_secret | awk '{print $2}')
curl -s -d "$BODY" -H "X-Hub-Signature-256: sha256=$SIG" $H                   # pass
curl -s -d "$BODY" -H "X-Hub-Signature-256: sha256=$(printf '%064d' 0)" $H   # fail: signature does not match
```

**Stripe.** Секрет `STRIPE_SECRET` — ключ підпису, `whsec_test_stripe`;
перевірка — **Stripe signature**.

<!-- docs-check: setup: stripe -->
```sh
H=http://127.0.0.1:8080/hooks
BODY='{"id":"evt_1","type":"invoice.paid"}'
T=$(date +%s)
V1=$(printf '%s.%s' "$T" "$BODY" | openssl dgst -sha256 -hmac whsec_test_stripe | awk '{print $2}')
curl -s -d "$BODY" -H "Stripe-Signature: t=$T,v1=$V1" $H     # pass
OLD=$((T-600))
V0=$(printf '%s.%s' "$OLD" "$BODY" | openssl dgst -sha256 -hmac whsec_test_stripe | awk '{print $2}')
curl -s -d "$BODY" -H "Stripe-Signature: t=$OLD,v1=$V0" $H   # fail: timestamp
```

**Standard Webhooks / Svix.** Секрет `SW_SECRET` — ключ підпису,
`whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw`; перевірка — **Standard Webhooks /
Svix signature**.

<!-- docs-check: setup: standard -->
```sh
H=http://127.0.0.1:8080/hooks
BODY='{"type":"user.created"}'
T=$(date +%s)
KEY=$(printf 'MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw' | base64 -d | od -An -tx1 | tr -d ' \n')
SW=$(printf 'msg_1.%s.%s' "$T" "$BODY" | openssl dgst -sha256 -mac HMAC -macopt hexkey:$KEY -binary | base64)
curl -s -d "$BODY" -H "webhook-id: msg_1" -H "webhook-timestamp: $T" -H "webhook-signature: v1,$SW" $H   # pass
curl -s -d "$BODY" -H "webhook-id: msg_2" -H "webhook-timestamp: $T" -H "webhook-signature: v1,$SW" $H   # fail: signature does not match
```

**Generic HMAC.** Секрет `HMAC_KEY` — ключ підпису, `hmac_test_key`;
перевірка — **Generic HMAC signature** із заголовком `X-Signature`, sha512 і
base64.

<!-- docs-check: setup: hmac -->
```sh
H=http://127.0.0.1:8080/hooks
BODY='{"event":"ping"}'
X=$(printf '%s' "$BODY" | openssl dgst -sha512 -hmac hmac_test_key -binary | openssl base64 -A)
curl -s -d "$BODY" -H "X-Signature: $X" $H            # pass
curl -s -d "$BODY" -H "X-Signature: not-base64!" $H   # fail: signature is not base64
```

**Basic.** Секрет `QA_PASSWORD` — токен чи пароль, `pw_test`; перевірка —
**Basic credentials** з користувачем `qa`.

<!-- docs-check: setup: basic -->
```sh
H=http://127.0.0.1:8080/hooks
curl -s -d '{}' -u qa:pw_test $H                      # pass
curl -s -d '{}' -u qa:wrong $H                        # fail: user or password does not match
curl -s -d '{}' -H 'Authorization: Bearer x' $H       # fail: scheme is Bearer, not Basic
```

Приклад для bearer-токена — у покроковій інструкції вище.

### Невдалий підпис

Невдалий підпис показує отриманий підпис повністю і перші 8 символів
очікуваного. Цього досить, щоб відрізнити неправильний секрет від
неправильного вмісту, і недосить, щоб підробити підпис. З неправильним
секретом не проходять усі запити, а очікуваний підпис для того самого тіла
не змінюється. З неправильним вмістом відправник змінив тіло перед підписом,
наприклад заново серіалізував JSON.

### Позначки часу

Stripe і Standard Webhooks підписують позначку часу. Hookwitness порівнює її
з часом, коли прийшов запит, з типовим допуском 300 с. Коли ви повторно
надсилаєте записані запити, поставте **Accept any timestamp (replayed
captures)**.

### Той самий підпис ще раз

Якщо підпис проходить і повторює підпис одного з раніших збережених запитів
цього ендпоінта, під вердиктом з'являється примітка, наприклад «same signature
as #1: a replay, or a redelivery of the same signed request». Рядок у
реєстрі показує «same sig #1». Вердикт лишається pass: GitHub,
повторно доставляючи подію, надсилає той самий підпис, і отримувач має такі
запити приймати. Stripe і Standard Webhooks підписують кожну повторну спробу
заново з новою позначкою часу, тому їхні повтори примітки не отримують;
примітка там означає, що той самий запит надіслали двічі. Запит, якого вже
немає (видалений за лімітом зберігання чи очищений), не порівнюється. Для
цього Hookwitness зберігає лише ключований дайджест кожного підпису.
Повторна перевірка обчислює примітки заново. Bearer-токени і Basic-облікові
дані повторюються в кожному запиті, і примітки до них не буває.

## Enforce

Коли **Enforce** вимкнено, перевірка лише записує вердикт, а запит отримує
звичайну відповідь. Коли ввімкнено, запиту, що не пройшов, відмовляють ще
до правил, і правила його не враховують. Відмова — `401` з причиною (з
`WWW-Authenticate` для bearer і basic) або ваша власна відповідь. Запиту з
вердиктом n/v теж відмовляють.

Приклад **Basic** вище з увімкненим **Enforce** (секрет `QA_PASSWORD` —
`pw_test`, користувач — `qa`):

<!-- docs-check: setup: basic_enforced -->
```sh
H=http://127.0.0.1:8080/hooks
curl -s -o /dev/null -d '{}' -u qa:pw_test $H   # 200
curl -s -o /dev/null -d '{}' -u qa:wrong $H     # 401 refused
```

Виправивши секрет, перевірте збережені запити знову; див.
[Вердикти й повторна перевірка](endpoints.md#вердикти-й-повторна-перевірка).

## Секрети

Вкладка **Secrets** показує секрети ендпоінта з їхнім типом, відбитком і
перевірками, що користуються кожним.

![Вкладка Secrets з API_TOKEN, яким користується перевірка 1](../images/secrets.webp)

- **Лише на запис.** Збережене значення більше ніколи не показується: ні в
  UI, ні в API, ні в логах, ні у звітах. Видно лише 8-символьний відбиток,
  щоб можна було побачити, чи мають два ендпоінти той самий секрет.
  **Replace** задає нове значення.
- **Зберігаються безпечно.** Ключі підпису зашифровано (AES-256-GCM), а
  токени й паролі зберігаються лише як хеші PBKDF2 із сіллю. Ключ
  шифрування — `HOOKWITNESS_SECRET_KEY` або файл ключа поруч із базою (див.
  [Ключ секретів](deployment.md#ключ-секретів)). Копія бази без ключа не
  розкриває жодного секрету.
- **Лише тестові й staging-секрети.** Хто може відкрити адмін-інтерфейс, той
  може змусити сервер перевіряти підписи цими секретами.
- Упійманий запит зберігає свої заголовки як доказ, тож bearer-токен чи
  Basic-пароль, надісланий відправником, видно в цьому запиті. Звіти можуть
  маскувати такі заголовки (див. [Звіти](reports.md)).

## Через API

Секрети: `PUT /endpoints/{id}/secrets/{name}`; перевірки — частина
документа поведінки. Див. [довідник API](../api.md#secrets) і його рецепти
[Require Basic credentials](../api.md#require-basic-credentials) та
[Require a GitHub signature](../api.md#require-a-github-signature).
