# Тестування відправника з AI-агентом

Агент, що пише відправника вебхуків, може й довести, що той працює:
запустити Hookwitness, спрямувати на нього відправника, запустити
відправника і прочитати з REST API, що прийшло, разом із вердиктами. Без
браузера і без циклу опитування.

## Skill

Hookwitness постачається зі skill для [Claude Code](https://claude.com/claude-code),
[`skills/hookwitness`](../../skills/hookwitness/SKILL.md), який навчає агента
цього порядку дій:

1. запустити Hookwitness у Docker або скористатися вже запущеним;
2. створити ендпоінт і дати відправнику його URL (з хоста чи з іншого
   контейнера);
3. вимагати те, що відправник має надіслати (Basic-пароль, bearer-токен,
   підпис), і задати збої, наприклад `503` на перші дві доставки чи
   таймаут, щоб перевірити повтори відправника;
4. запустити відправника і дочекатися запиту (`wait=30s` тримає виклик,
   доки запит не прийде);
5. перевірити статус, вердикт, заголовки й тіло.

Встановіть його в проєкт або в `~/.claude/skills/hookwitness/` для всіх
проєктів:

<!-- docs-check: skip: downloads from GitHub -->
```sh
mkdir -p .claude/skills/hookwitness && curl -fsSL -o .claude/skills/hookwitness/SKILL.md \
  https://raw.githubusercontent.com/petroladkin/hookwitness/dev/skills/hookwitness/SKILL.md
```

Потім попросіть своїми словами, наприклад: *«Перевір, що сервіс замовлень
підписує вебхуки за схемою GitHub і повторює після 503»*. Агент підхопить
skill за його описом.

## Що робить агент, вручну

Та сама сесія в shell, як її проганяв би тест:

```sh
API=http://127.0.0.1:8080/admin/api/v1
ID=$(curl -s -X POST $API/endpoints -H 'Content-Type: application/json' -d '{"path":"/orders/events"}' | jq .id)
curl -s -o /dev/null -X PUT $API/endpoints/$ID/secrets/ORDERS_PASSWORD -H 'Content-Type: application/json' \
  -d '{"kind":"token","value":"test-password"}'
TAG=$(curl -sI $API/endpoints/$ID/behaviour | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -s -o /dev/null -X PUT $API/endpoints/$ID/behaviour -H 'Content-Type: application/json' -H "If-Match: $TAG" \
  -d '{"checks":[{"kind":"basic","secret":"ORDERS_PASSWORD","user":"orders","enforce":true}]}'

curl -s -u orders:test-password -H 'Content-Type: application/json' -d '{"order":42}' \
  http://127.0.0.1:8080/orders/events   # 200: the sender under test

curl -s "$API/endpoints/$ID/requests?after=0&wait=30s" | jq -c '.requests[] | {id, status, verdict}'
# {"id":1,"status":200,"verdict":"pass"}
```

Усі маршрути, документ поведінки і більше рецептів — у
[довіднику API](../api.md); для відправника в контейнері див.
[Testing a sender in Docker](../api.md#testing-a-sender-in-docker).

## MCP

MCP-сервер, щоб агенти керували Hookwitness як інструментами без shell,
заплановано, але його ще немає.
