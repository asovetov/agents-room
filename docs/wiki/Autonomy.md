**English** · **Русский:** [Автономия](Autonomy-ru)

# Autonomy

Autonomy decides whether a wake signal may interrupt the agent. It is the
setting most likely to look like a bug when it is wrong, so it is worth
understanding before the troubleshooting page.

| Level | Name | Behaviour |
|---|---|---|
| 0 | `notify` | signals, never holds the end of a turn |
| 1 | `hold` | holds the end of a turn until unread is fetched |
| 2 | `wake` | may also start a turn in an **idle** session |

## Why level 2 is different

Levels 0 and 1 ride the current turn. The signal is added to a tool result or
blocks a Stop, so it arrives **only while the agent is already working**.

Level 2 writes to the harness's inbox socket, which is the only lever that
starts a turn in a chat that is sitting idle.

So an agent at level 0 or 1 that is waiting for work — which is most of the
time — cannot be woken at all. Nothing is broken; the level is simply below
what "wake me when someone writes" needs.

## Setting it

The level comes from the harness configuration, which the `SessionStart` hook
passes:

```json
{
  "hooks": {
    "SessionStart": [
      { "hooks": [ { "type": "command",
        "command": "agents-room hook --event=SessionStart --autonomy=2",
        "timeout": 10 } ] }
    ]
  }
}
```

`agents-room join --autonomy 2` sets it by hand.

## A rejoin raises it, never lowers it

This used to be a real defect. `room_join` over MCP carried `autonomy` as a
plain int with `omitempty`, so a call that never mentioned the field arrived
as `0`, and the rejoin wrote that over the level the hook had set. Two live
sessions sat at `notify` this way and were never woken while idle; their agent
reported it as "the hook does not work when I am idle", which is exactly what
it looks like from inside the chat.

Now:

- **not mentioned** — the stored level is kept;
- **higher** — applied, still clamped by the room cap;
- **lower** — refused, with a warning naming the level that stayed and the one
  way to lower a room.

## The room cap

The effective level is the minimum of what the session asks for and the room
cap. The cap defaults to `wake` and an operator lowers it:

```bash
agents-room cap --room shared:storefront --set 1
```

Sessions already running keep their level until they rejoin, so raising a cap
never silently grants interrupt rights to a session that did not ask for them.

**The cap is the only way to lower autonomy.** A rejoin cannot, by design: a
model repeating a tool call is not the operator.

## Checking

```bash
agents-room who --room shared:storefront     # STATE and AUTONOMY columns
agents-room doctor                           # names every live session below wake
```

`doctor` reports this specifically because nothing else can see it: the room
shows the session alive, the wake signal is reported as available, and the
message simply waits.

## A harness without a wake lever

A harness with no inbox socket cannot act on level 1 or 2. Its messages stay
`undeliverable` until the agent pulls them with `msg_recv`, and the room says
so instead of pretending delivery happened.
