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

Hookwitness — це ще й MCP-сервер, тож агент може керувати ним через
інструменти замість shell. Він відповідає по MCP через Streamable HTTP на
`http://127.0.0.1:8080/admin/mcp`, з тими самими правилами доступу, що й
REST API. `hookwitness mcp` з'єднує його з клієнтами, які вміють лише stdio.

Додайте його в Claude Code:

<!-- docs-check: skip: adds the server to your Claude Code configuration -->
```sh
claude mcp add --transport http hookwitness http://127.0.0.1:8080/admin/mcp
```

Для клієнта, що вміє лише stdio, запустіть міст. Це може бути бінарник
або той, що всередині контейнера:

<!-- docs-check: skip: adds the server to your Claude Code configuration -->
```sh
claude mcp add hookwitness -- hookwitness mcp -url http://127.0.0.1:8080
claude mcp add hookwitness -- docker exec -i hookwitness /hookwitness mcp -url http://127.0.0.1:8080
```

Інші клієнти приймають те саме у своєму конфігураційному файлі:

```json
{
  "mcpServers": {
    "hookwitness": { "type": "http", "url": "http://127.0.0.1:8080/admin/mcp" }
  }
}
```

```json
{
  "mcpServers": {
    "hookwitness": { "command": "hookwitness", "args": ["mcp", "-url", "http://127.0.0.1:8080"] }
  }
}
```

З адмінкою на окремому порту ([Налаштування й
розгортання](deployment.md#адмінка-на-окремому-порту)) MCP теж на тому
порту: `http://127.0.0.1:8081/mcp`, якщо адмінка в його корені. Тоді
передайте мосту повну адресу: `hookwitness mcp -url http://127.0.0.1:8081/mcp`.

З увімкненим входом ([Користувачі, вхід і токени](access.md)) MCP потребує
токена: `--header "Authorization: Bearer <token>"` для клієнта, що говорить
HTTP, `-token-file <file>` для мосту (див. [MCP з
токеном](access.md#mcp-з-токеном)). Агент тоді діє від імені власника
токена.

Перевірте, що сервер відповідає по MCP:

```sh
curl -s -X POST http://127.0.0.1:8080/admin/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"1"}}}' \
  | grep -o '"name":"hookwitness"'
# "name":"hookwitness"
```

Інструменти побудовані під задачі. Кожен результат — рядок-підсумок, а за
ним компактний JSON.

| Інструмент | Що робить |
|---|---|
| `create_endpoint` | ендпоінт і його catch URL, адреса для відправника |
| `list_endpoints`, `get_endpoint` | ендпоінти; один — з поведінкою і секретами (лише імена й відбитки) |
| `configure_endpoint` | задає частини поведінки (`default`, `rules`, `checks`, `vars`, `schemas`); те, чого не передали, лишається |
| `set_secret` | ключ підпису чи токен для перевірок; лише на запис, у відповідь — відбиток |
| `wait_for_request` | чекає на наступний запит, що підходить (вердикт, статус, текст, значення в JSON), до 300 с, і повертає його повністю |
| `list_requests`, `get_request` | запити з тими самими фільтрами; один повністю, тіло обрізане на 16 KiB із розміром |
| `expect` | перевіряє, що N потрібних запитів прийдуть за певний час: pass або fail із тим, що прийшло, і причинами |
| `reset_endpoint`, `recheck`, `delete_endpoint` | скинути лічильник чи очистити запити, перевірити знову, видалити |

`delete_endpoint` і `reset_endpoint` позначені як деструктивні, а
інструменти читання — як read-only, тож клієнт може спитати вас перед
деструктивними. Сесія виглядає так:

1. `create_endpoint` із `/github/events`, потім дайте його URL відправнику;
2. `set_secret` `GH` як ключ і `configure_endpoint` з перевіркою
   `{"kind": "github", "secret": "GH"}`;
3. запустіть відправника, потім `expect` з `verdict: pass`: ви отримаєте
   `pass` або `fail: … #2 fail GitHub signature: signature does not match`.

Заголовки впійманого запиту — це доказ, тож токен чи пароль, що надіслав
відправник, видно в `get_request`, як і в API. Значення секретів, які ви
задаєте, не показуються ніколи.
