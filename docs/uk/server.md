# Сервер для команди

Від нуля до Hookwitness, яким користується ваша команда: відправники
доставляють на шляхи для вебхуків через HTTPS, адмінка питає вхід, а дані
переживають оновлення. Кожен крок посилається на сторінку, де його описано
докладно.

## 1. Вирішіть, хто до чого має доступ

Hookwitness має на одному сервері дві частини:

- **шляхи для вебхуків**, куди відправники доставляють вебхуки: до них має
  дістатися відправник, часто з інтернету;
- **адмінку** (вебінтерфейс, REST API і MCP під `/admin`): вона показує
  кожен упійманий запит і змінює кожен ендпоінт, тож їй потрібен вхід, і
  дістатися до неї має лише ваша команда.

Оберіть схему до початку:

| Хто користується | Шляхи для вебхуків | Адмінка |
|---|---|---|
| Лише ви, на своїй машині | `127.0.0.1:8080` | той самий порт, вхід не потрібен: досить [Перших кроків](getting-started.md) |
| Ви і відправники в інтернеті | `https://hooks.example.com` через проксі | окремий порт на localhost сервера, доступ через SSH-тунель |
| Команда в браузері | `https://hooks.example.com` через проксі | `https://admin.example.com`, окремий сайт на проксі |

Далі на цій сторінці будуються дві останні схеми з Docker Compose і
[Caddy](https://caddyserver.com), який сам отримує сертифікати TLS. Спершу
спрямуйте DNS-імена на сервер і відкрийте порти 80 і 443.

## 2. Пароль першого адміністратора

Перший користувач вмикає вхід і завжди є адміністратором. Тут він береться
зі змінних середовища на першому старті, а пароль лежить у файлі, який може
прочитати лише користувач образу (uid 65532):

<!-- docs-check: skip: changes file ownership with sudo -->
```sh
mkdir hookwitness && cd hookwitness
openssl rand -base64 18 > admin_password.txt   # or write your own: one line, at least 12 characters
sudo chown 65532 admin_password.txt && sudo chmod 400 admin_password.txt
```

Сервер читає цей файл на кожному старті, поки задано
`HOOKWITNESS_ADMIN_PASSWORD_FILE`, і зупиняється, якщо не може: файл, що
належить вам і має режим 600, користувач образу не прочитає. Адміністратора
створено лише в базі без користувачів; наступні старти пишуть у лог, що
пропустили це, а зміна файлу нічого не змінює. Інші способи додати першого
адміністратора — у [Користувачах, вході й токенах](access.md#вхід-вимкнено-доки-немає-першого-користувача).

## 3. Compose і Caddy

`compose.yaml`, для схеми з SSH-тунелем до адмінки:

```yaml
services:
  hookwitness:
    image: ghcr.io/petroladkin/hookwitness:0.2
    restart: unless-stopped
    ports: ["127.0.0.1:8081:8081"]   # адмінка, лише на localhost сервера
    volumes: ["hookwitness:/data"]
    environment:
      HOOKWITNESS_ADMIN_ADDR: ":8081"
      HOOKWITNESS_ADMIN_PREFIX: /
      HOOKWITNESS_PUBLIC_URL: https://hooks.example.com
      HOOKWITNESS_ADMIN_USER: ann
      HOOKWITNESS_ADMIN_PASSWORD_FILE: /run/secrets/admin_password
    secrets: [admin_password]
  caddy:
    image: caddy:2
    restart: unless-stopped
    ports: ["80:80", "443:443"]
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy:/data
secrets:
  admin_password:
    file: ./admin_password.txt
volumes:
  hookwitness:
  caddy:
```

`Caddyfile`:

```
hooks.example.com {
	reverse_proxy hookwitness:8080
}
```

Що робить кожен параметр:

- `HOOKWITNESS_ADMIN_ADDR` дає адмінці окремий порт, 8081. Порт 8080 тоді
  обслуговує лише шляхи для вебхуків і `/healthz`, тож проксі може віддати
  відправникам усе на ньому, а `/admin/…` там — просто ще один шлях для
  вебхуків. Порт 8080 на сервері не опубліковано: до нього дістається лише
  Caddy.
- `HOOKWITNESS_ADMIN_PREFIX: /` ставить інтерфейс у корінь порту адмінки.
- `HOOKWITNESS_PUBLIC_URL` — адреса, якою користуються відправники: на ній
  будуються catch URL, які показує інтерфейс. Cookie входу позначено
  *Secure* на окремому сайті адмінки (крок 3, для команди); через тунель, по
  звичайному HTTP, — ні, інакше браузер його відкинув би ([За зворотним
  проксі](access.md#за-зворотним-проксі)).
- Том `hookwitness` тримає базу і ключ, яким запечатано секрети; див.
  [Тримайте дані в томі](deployment.md#тримайте-дані-в-томі).

Решта параметрів — у [Налаштуваннях і розгортанні](deployment.md#параметри).

### Для команди в браузері

Дайте адмінці окремий сайт замість тунелю. У `compose.yaml` приберіть рядок
`ports` у `hookwitness` і додайте ім'я, на яке вона відповідає:

```yaml
      HOOKWITNESS_ADMIN_HOSTS: admin.example.com
```

і додайте сайт у `Caddyfile`:

```
admin.example.com {
	reverse_proxy hookwitness:8081
}
```

Адмінка відповідає лише на імена з `HOOKWITNESS_ADMIN_HOSTS` (а ще на
IP-адреси і `localhost`), і це не дає ворожій вебсторінці дістатися до неї
через власне ім'я; див. [Доступ з інших машин](deployment.md#доступ-з-інших-машин).

Входи з інтернету обмежено для кожної адреси: десять невдач за 15 хвилин
призупиняють цю адресу. За Caddy кожен вхід приходить з адреси Caddy, тож
один зловмисник, що підбирає пароль, призупинив би всіх. Назвіть Caddy
довіреним проксі, вказавши підмережу мережі compose, яку друкує `docker
network inspect`, коли стек уже запущено:

<!-- docs-check: skip: needs the compose stack running -->
```sh
docker network inspect hookwitness_default -f '{{(index .IPAM.Config 0).Subnet}}'
```

Задайте її як `HOOKWITNESS_TRUSTED_PROXIES` і знову виконайте `docker
compose up -d`; див. [За зворотним проксі](access.md#за-зворотним-проксі).

## 4. Запустіть і увійдіть

<!-- docs-check: skip: starts a compose stack with public names -->
```sh
docker compose up -d
docker compose logs hookwitness | grep -E 'admin user created|bootstrap|level=ERROR'
curl -s https://hooks.example.com/healthz
```

У лозі буде `admin user created from HOOKWITNESS_ADMIN_USER: login is on`.
`level=ERROR` з `HOOKWITNESS_ADMIN_PASSWORD_FILE: … permission denied`
означає, що користувач образу не може прочитати файл: поверніться до
кроку 2.

Відкрийте адмінку. Для тунелю виконайте на своїй машині:

<!-- docs-check: skip: opens an SSH tunnel to your server -->
```sh
ssh -N -L 8081:127.0.0.1:8081 you@your-server
```

і відкрийте <http://127.0.0.1:8081/>. Для команди відкрийте
<https://admin.example.com/>. Увійдіть як `ann` з паролем з
`admin_password.txt`, а тоді змініть його на сторінці акаунта (ваше ім'я
вгорі праворуч, *Account and tokens*, *Change password*). Файл свою справу
зробив: його можна видалити, прибравши з `compose.yaml` обидві змінні
`HOOKWITNESS_ADMIN_…` і секрет, або лишити всі три; сервер зупиняється на
старті, якщо змінні називають файл, якого вже немає.

## 5. Залучіть команду

- **Люди:** адміністратор додає їх на сторінці **Users** з першим паролем;
  кожен змінює його на своїй сторінці акаунта. *User* керує ендпоінтами,
  *admin* ще й людьми. Ніхто не може понизити чи видалити себе, тож
  адміністратор завжди лишається: [Ролі](access.md#ролі).
- **Скрипти, CI й агенти:** кожен робить особисті токени на своїй сторінці
  акаунта і надсилає їх як `Authorization: Bearer …`: [Токени для скриптів
  і агентів](access.md#токени-для-скриптів-і-агентів). Для Claude Code через
  MCP див. [MCP з токеном](access.md#mcp-з-токеном).
- **Відправники:** створіть ендпоінти і дайте кожному відправнику його catch
  URL, тепер на `https://hooks.example.com`. Відомий набір ендпоінтів може
  братися з файлу на кожному старті: [Як поділитися налаштуванням
  ендпоінта](sharing.md).

## 6. Резервна копія

Усе лежить у томі `hookwitness`: база і її ключ. Копіюйте їх разом; база,
відновлена без ключа, зберігає запити і втрачає секрети ([Ключ
секретів](deployment.md#ключ-секретів)). Зупинивши сервер на хвилину:

<!-- docs-check: skip: stops your server and writes a backup file -->
```sh
docker compose stop hookwitness
docker run --rm -v hookwitness_hookwitness:/data:ro -v "$PWD":/backup alpine \
  tar -czf /backup/hookwitness-$(date +%F).tar.gz -C /data .
docker compose start hookwitness
```

Щоб відновити, зупиніть сервер, розпакуйте архів у порожній том і запустіть
знову. Назва тому починається з назви проєкту compose
(`hookwitness_hookwitness` для теки `hookwitness`); `docker volume ls` її
покаже.

## Готово

- [ ] `https://hooks.example.com/healthz` відповідає `"status":"ok"`.
- [ ] Адмінка питає вхід, а пунктирного рядка «No login» немає.
- [ ] З інтернету адмінка недоступна ні на порту 8080, ні на 8081:
      `https://hooks.example.com/admin/` потрапляє в реєстр Unmatched.
- [ ] Пароль першого адміністратора змінено, і є другий адміністратор.
- [ ] Резервна копія тому є, і її вже раз відновлювали.

Про оновлення — в [Оновленні](deployment.md#оновлення); про звичні
несподіванки — у [Розв'язанні проблем](troubleshooting.md).
