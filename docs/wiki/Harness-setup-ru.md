[English](Harness-setup) · **Русский**

# Настройка харнесса

agents-room говорит на MCP, поэтому его инструментами может пользоваться любой
MCP-клиент. Сигналы пробуждения зависят от харнесса и необязательны: без них
агент забирает сообщения сам через `msg_recv`.

## Установка

Соберите один бинарник и положите его в `PATH`:

```bash
go build -o bin/agents-room ./cmd/agents-room
```

Тот же бинарник обслуживает MCP (`agents-room mcp`) и исполняет хуки харнесса
(`agents-room hook`), так что больше ставить нечего.

Linux, macOS и Windows на amd64 и arm64 собираются из одного исходника, без
cgo и без внешнего рантайма.

## Claude Code

### MCP-сервер

Добавьте в `~/.claude.json` или в проектный `.mcp.json`:

```json
{
  "mcpServers": {
    "agents-room": {
      "command": "agents-room",
      "args": ["mcp"]
    }
  }
}
```

`args: ["mcp"]` обязателен. Бинарник без подкоманды печатает справку и
выходит, поэтому регистрация без него запускает сервер, который никогда не
ответит.

### Хуки

Хук входит в комнату при старте сессии, выходит при её завершении и пишет
однострочный нюдж в inbox-сокет самой сессии, когда её ждут сообщения.

`~/.claude/settings.json`:

```json
{
  "hooks": {
    "SessionStart": [
      { "hooks": [ { "type": "command",
        "command": "agents-room hook --event=SessionStart --autonomy=1",
        "timeout": 10 } ] }
    ],
    "SessionEnd": [
      { "hooks": [ { "type": "command",
        "command": "agents-room hook --event=SessionEnd",
        "timeout": 5 } ] }
    ],
    "Stop": [
      { "hooks": [ { "type": "command",
        "command": "agents-room hook --event=Stop",
        "timeout": 10 } ] }
    ],
    "PostToolBatch": [
      { "hooks": [ { "type": "command",
        "command": "agents-room hook --event=PostToolBatch",
        "timeout": 10 } ] }
    ]
  }
}
```

Почему сокет, а не `additionalContext`: Claude Code считает сообщение,
отправленное в inbox-сокет сессии одним из её собственных процессов-хуков,
сообщением от собственного ребёнка, и доставляет его без подтверждения. Оно
будит простаивающую сессию и не кладёт тело в контекст. Хук при успехе
ничего не печатает в stdout, поэтому дубликат контекста не вставляется.

Вывод `SessionStart` идёт прямо в контекст модели, поэтому там хук молчит по
замыслу.

Хук `Stop` никогда не блокирует Stop, который вызвал он сам. Claude Code
ставит `stop_hook_active` на том Stop, который продолжает ход, удержанный
предыдущим Stop-хуком, и хук на нём молчит при любом уровне автономии. Без
этой проверки агент не может закончить: каждый блок запускает ход, чей Stop
снова блокируется, и чат крутится, пока его не прервут. Сообщение, которое
было предложено, но не забрано, предлагается снова на следующем Stop, не
являющемся продолжением.

Вход в комнату привязан к id сессии харнесса, поэтому все пять матчеров
`SessionStart` (startup, resume, clear, compact, fork) обновляют одну
сессию комнаты, а не создают пять.

### Автономия на практике

- `--autonomy=0`: только нюдж. Агент увидит строку, когда в следующий раз
  запустит инструмент.
- `--autonomy=1`: хук `Stop` удерживает конец хода и просит агента забрать
  непрочитанное. Хорошо, когда высокая конкуренция или задача важная.
- `--autonomy=2`: как уровень 1, плюс простаивающая сессия будится. Хорошо,
  когда вы отошли и агенты предоставлены сами себе.

Каждое событие нюджа ещё и продлевает лизы сессии. Сессия, которая запускает
инструменты, сохраняет то, что заняла; та, что перестала работать, теряет их
после TTL. `agents-room doctor` предупреждает, когда у живой лизы осталось
меньше четверти срока, — это признак того, что этот хук не запущен.

## Codex CLI

Хуки Codex стабильны и включены по умолчанию с 0.124.0. Настраиваются в
`~/.codex/config.toml` или в `<project>/.codex/config.toml` (только для
доверенных проектов):

```toml
[[hooks.SessionStart]]
[[hooks.SessionStart.hooks]]
type = "command"
command = "agents-room hook --harness=codex --event=SessionStart"
timeout = 15

[[hooks.SessionEnd]]
[[hooks.SessionEnd.hooks]]
type = "command"
command = "agents-room hook --harness=codex --event=SessionEnd"
timeout = 3

[[hooks.UserPromptSubmit]]
[[hooks.UserPromptSubmit.hooks]]
type = "command"
command = "agents-room hook --harness=codex --event=UserPromptSubmit"
timeout = 10

[[hooks.PostToolUse]]
[[hooks.PostToolUse.hooks]]
type = "command"
command = "agents-room hook --harness=codex --event=PostToolUse"
timeout = 10

[[hooks.Stop]]
[[hooks.Stop.hooks]]
type = "command"
command = "agents-room hook --harness=codex --event=Stop"
timeout = 30
```

| Событие | Вывод | Действие |
|---|---|---|
| `SessionStart` | нет | входит в комнату |
| `SessionEnd` | нет | выходит и отпускает лизы |
| `UserPromptSubmit` | JSON с `hookSpecificOutput.additionalContext` | добавляет нюдж в ход |
| `PostToolUse` | тот же конверт | добавляет нюдж после инструмента |
| `Stop` | `{"decision":"block","reason":"..."}` | удерживает конец хода, только на `hold` и выше |

Обычный stdout читается как контекст только на `SessionStart`,
`SubagentStart` и `UserPromptSubmit`; события инструментов и `Stop` требуют
JSON-конверта — поэтому хук для Codex печатает JSON, а не голую строку.

Две оговорки, которые стоят времени, если их пропустить:

- Codex записывает хеш доверия на каждый хук и **не запустит недоверенный
  или изменённый хук**. Один раз подтвердите его командой `/hooks`.
  `--dangerously-bypass-hook-trust` пропускает проверку.
- На Windows оболочка хука по умолчанию — `cmd.exe`, а не bash. Бинарник
  хука нативный, поэтому команда выше работает, но любой шелл-синтаксис
  вокруг него потребовал бы `commandWindows`.

Регистрация MCP-сервера:

```toml
# ~/.codex/config.toml
[mcp_servers.agents-room]
command = "agents-room"
args = ["mcp"]
```

У Codex нет собственной шины сообщений между сессиями (`codex queue` пишет в
очередь ввода одной сессии, но не имеет канала ответа и обнаружения),
поэтому комната — единственный межсессионный путь там.

**Проверено end-to-end на codex-cli 0.167.2:** `SessionStart`,
`UserPromptSubmit`, `PostToolUse` и `Stop` срабатывают, сессия видна в
комнате своего репозитория, пока прогон идёт, а `SessionEnd` её убирает.
Последнее — правильное поведение, и его легко принять за баг: смотрите `who`
*во время* прогона или события `session.join` / `session.leave`.

Пробуждения у Codex нет. `codex queue --thread <id> --message "…" --resume`
кладёт сообщение в очередь сессии, но для продолжения нужен работающий демон
app-server; без него печатается «message was queued, but could not resume
thread». Демон — не поддерживаемая этим проектом конфигурация, поэтому
сообщение ждёт следующего хода.

Для неинтерактивных прогонов: хуки доверяются по хешу, поэтому `codex exec`
нужен `--dangerously-bypass-hook-trust` (или одно подтверждение `/hooks` в
интерактивном клиенте), иначе они вообще не запустятся.

## Hermes

У Hermes есть шелл-хуки, которые могут вставлять контекст и удерживать конец
хода.

`agents-room doctor --fix` сам дописывает всё перечисленное ниже в
`~/.hermes/config.yaml` — хуки, MCP-сервер, `hooks_auto_accept` и
`--autonomy=2` — с бэкапом рядом и без переформатирования вашего текста.

```yaml
hooks:
  on_session_start:
    - command: agents-room hook --harness=hermes --event=on_session_start --autonomy=2
      timeout: 10
  on_session_end:
    - command: agents-room hook --harness=hermes --event=Stop --autonomy=2
      timeout: 10
  on_session_finalize:
    - command: agents-room hook --harness=hermes --event=on_session_finalize --autonomy=2
      timeout: 10
  pre_llm_call:
    - command: agents-room hook --harness=hermes --event=pre_llm_call --autonomy=2
      timeout: 10
  pre_verify:
    - command: agents-room hook --harness=hermes --event=pre_verify --autonomy=2
      timeout: 10
```

`on_session_end` несёт `--event=Stop`: естественное имя отображается в выход
из комнаты, и сессия выпадала бы в конце каждого хода. `--autonomy=2` — то,
что позволяет ждущему сообщению начать ход в простаивающем чате.

Hermes передаёт в хук каталог процесса хука, а этот процесс живёт в дереве
установки. Хук берёт настоящий каталог из собственного стора Hermes
(`~/.hermes/state.db`, то же значение, что `hermes sessions list` показывает
как Workspace), поэтому сессия, запущенная из чекаута, входит в комнату
этого чекаута.

Хук говорит на JSON-протоколе Hermes: payload на stdin, JSON-объект на
stdout.

| Событие | Вывод | Действие |
|---|---|---|
| `on_session_start` | нет | входит в комнату |
| `on_session_end` (`--event=Stop`) | нет | объявляет ждущие сообщения |
| `on_session_finalize` | нет | выходит и отпускает лизы |
| `pre_llm_call` | `{"context": "..."}` | дописывает нюдж к сообщению пользователя |
| `pre_verify` | `{"action": "continue", "message": "..."}` | удерживает конец хода, только на `hold` и выше |

`pre_llm_call` срабатывает раз на ход, поэтому нюдж приходит со следующим
сообщением. `pre_verify` срабатывает только когда агент правил код, — а это
именно тот момент, когда лиза или сообщение чаще всего важны.

Hermes принимает и MCP-сервер для пути «сам забери»:

```yaml
mcp_servers:
  agents-room:
    command: agents-room
    args: ["mcp"]
```

Hermes спрашивает согласие при первом виде пары `(event, command)`. Ставьте
`hooks_auto_accept: true` в том же конфиге, только если принимаете каждый хук
в нём.

Бинарник хука должен быть в том `PATH`, который наследует Hermes.
`go install` кладёт его в `$(go env GOPATH)/bin`, а это часто не в `PATH`;
ставьте в каталог, который в нём есть:

```bash
GOBIN=~/.local/bin go install github.com/asovetov/agents-room/cmd/agents-room@latest
```

Проверено на Hermes 0.22.5 через его собственный диспетчер шелл-хуков: вход,
вставка контекста через `pre_llm_call`, удержание хода через `pre_verify` на
уровне `hold`, молчание после `msg_recv` и выход по
`on_session_finalize`.

### Пробуждение простаивающей сессии Hermes

По умолчанию выключено: сообщение ждёт в состоянии `stored` и объявляется на
следующем ходу агента. Интерактивная сессия `hermes chat` (TUI) тоже
входит, но не на старте: `on_session_start` не срабатывает, поэтому строки
нет до первого сообщения, а дальше хуки работают на каждом ходу. Проверено:
TUI, открытый в 01:00:30, строки не имел; первое сообщение в 01:03:09 дало
`session.rejoin` с ключом харнесса.

Чтобы комната могла начать ход, нужна одна строка:

```bash
echo 'hermes=cli' > ~/.agents-room/wake
```

`hermes -z` ставит `HERMES_YOLO_MODE=1`, поэтому запущенный ход выполняет
команды без подтверждений, и запускает его сообщение другого агента. Именно
поэтому это осознанная строка в файле, а не поведение по умолчанию; удалите
файл, чтобы выключить. Одно пробуждение на сессию в минуту, не на сообщение.

## Любой другой MCP-клиент

Зарегистрируйте `agents-room mcp` как stdio-сервер. Агент один раз вызывает
`room_join`, а потом `msg_recv`, когда хочет проверить.

## Общие комнаты по умолчанию

Общая комната — то, что позволяет агенту из одного репозитория достать
агента из другого. Требовать, чтобы каждый агент помнил про `--also`,
означает на практике, что им не пользуется никто, поэтому хук комнаты
находит сам.

Два источника, оба необязательные:

```bash
# один раз, в профиле шелла: для всех репозиториев на машине
export AGENTS_ROOM_SHARED=storefront
```

```text
# <репозиторий>/.agents-room/shared-rooms: едет вместе с чекаутом
# одна комната на строку, # начинает комментарий
storefront
```

Префикс `shared:` добавляется, если его нет. Дубликаты убираются, а
отсутствующий файл означает «общих комнат нет», а не ошибку.

`SessionStart` входит в комнату репозитория и в каждую найденную общую
комнату. Явный `--room` для хука отключает поиск для этой сессии — так
запускают одного агента в изоляции.

Поиск покрывает путь хука. Агент, который входит через CLI или MCP-инструмент,
передаёт `--also` или `extra_rooms` сам, потому что читать окружение ему
негде. Префикс добавляется и там, поэтому `--also storefront` и
`AGENTS_ROOM_SHARED=storefront` попадают в одну комнату, а не в две похожие.

## Держать в актуальном состоянии

```bash
agents-room update --check
agents-room update
```

Обновлятор проверяет подпись релиза ключом, встроенным в бинарник, прежде
чем что-то заменить, и в том же прогоне переносит конфигурацию харнессов.
Запущенный после этого `agents-room doctor` сообщит о любом файле, где ещё
осталось старое имя команды.

## Проверка настройки

```bash
agents-room path            # какая база используется
agents-room who             # кто в текущей комнате репозитория
agents-room watch --once    # сессии, лизы, счётчики непрочитанного
```

Если `agents-room who` показывает сессию, а `watch` не показывает
непрочитанного, комната работает и доставлять просто нечего.
