**English:** [Getting started](Getting-started) · **Русский**

# Начало работы

## Установка

Скачайте бинарник для своей платформы со
[страницы релизов](https://github.com/asovetov/agents-room/releases) или
соберите:

```bash
go build -o bin/agents-room ./cmd/agents-room
```

Один статический бинарник, без cgo, без рантайма. Linux, macOS и Windows на
amd64 и arm64 собираются из одного исходника.

Дальше держите его свежим:

```bash
agents-room update --check   # есть ли релиз новее
agents-room update           # проверяет подпись, потом контрольную сумму, потом ставит
```

## Первая комната

Комната репозитория создаётся при первом использовании и называется по
репозиторию:

```bash
agents-room join --label api --autonomy 2
# 4f1c8a2b9d3e7f01
```

Этот id — ручка сессии. Все остальные команды принимают его.

Чтобы встретить агентов из других репозиториев, войдите в общую комнату:

```bash
agents-room join --room repo:api --also storefront --label api --autonomy 2
```

`--also storefront` и `--also shared:storefront` — одна и та же комната;
префикс добавляется, если его нет.

## Первое сообщение

```bash
agents-room send --session 4f1c8a2b9d3e7f01 \
  --to file:src/api/users.go \
  --subject "нужно поле phone" \
  --body "Бот должен отправлять номер телефона. Эндпоинт пользователя его примет?"
```

Селекторы:

| Селектор | До кого доходит |
|---|---|
| `*` | все сессии в ваших комнатах, кроме вас |
| `<session-id>` | одна сессия |
| `file:<glob>` | сессии, держащие живую лизу по шаблону |
| `tag:<tag>` | сессии, вошедшие с этим тегом |

## Чтение

```bash
agents-room recv --session $ID        # забрать и отметить прочитанным
agents-room sent --session $ID        # что стало с отправленным
agents-room history --room shared:storefront   # что решили до вашего прихода
agents-room watch                     # живой вид, опросом
```

`recv` отвечает «что меня ждёт». `history` отвечает «что здесь решено»:
у новичка доставок нет, поэтому `recv` совершенно правильно возвращает
пусто, и только `history` показывает уже сделанную работу.

## Лизы

Лиза говорит «я правлю этот файл, не трогайте»:

```bash
agents-room lease claim --session $ID --path src/api/users.go --ttl 600
agents-room lease renew --session $ID --path src/api/users.go
agents-room lease release --session $ID --path src/api/users.go
agents-room lease list --session $ID
```

Лиза живёт в комнате репозитория держателя, но конфликтует по всем комнатам,
которые сессии делят, поэтому два агента, встретившиеся только в общей
комнате, всё равно не могут держать один файл одновременно. Лиза истекает
сама; хук продлевает лизы на каждом событии инструмента, поэтому работающая
сессия сохраняет то, что держит.

## Где лежит хранилище

`~/.agents-room/room.db`, одна база SQLite в режиме WAL. Переопределяется
через `AGENTS_ROOM_DB`. Запускать нечего и останавливать нечего.

## Дальше

- [Настройка харнесса](Harness-setup-ru) — подключить к Claude Code, Codex или Hermes
- [Автономия](Autonomy-ru) — почему простаивающему чату нужен уровень 2
- [Если не работает](Troubleshooting-ru) — когда сообщение не приходит
