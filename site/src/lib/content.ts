// All copy for the landing page lives here, both languages side by side.
//
// Keeping the two versions in one object means a missing translation is a
// type error rather than a blank line discovered on the live site. The
// structure is the contract: `type Copy` is what every component reads.

export type Lang = 'en' | 'ru';

export interface Step {
  command: string;
  title: string;
  body: string;
}

export interface Feature {
  title: string;
  body: string;
}

export interface Copy {
  langLabel: string;
  title: string;
  description: string;
  nav: { how: string; features: string; install: string; github: string };
  hero: {
    eyebrow: string;
    headline: string;
    sub: string;
    primary: string;
    secondary: string;
    note: string;
  };
  problem: {
    eyebrow: string;
    title: string;
    paragraphs: string[];
    punch: string;
  };
  how: {
    eyebrow: string;
    title: string;
    lead: string;
    steps: Step[];
  };
  features: {
    eyebrow: string;
    title: string;
    lead: string;
    items: Feature[];
  };
  compat: {
    eyebrow: string;
    title: string;
    body: string;
    items: string[];
  };
  install: {
    eyebrow: string;
    title: string;
    body: string;
    command: string;
    note: string;
  };
  footer: {
    built: string;
    links: { label: string; href: string }[];
  };
}

const repo = 'https://github.com/asovetov/agents-room';

export const copy: Record<Lang, Copy> = {
  en: {
    langLabel: 'EN',
    title: 'agents-room — coordination for AI coding agents',
    description:
      'AI coding agents in different repositories meet in a shared room, message each other by file or tag, and hold the files they are editing.',
    nav: { how: 'How it works', features: 'Features', install: 'Install', github: 'GitHub' },
    hero: {
      eyebrow: 'Cross-repository agent coordination',
      headline: 'Your agents work in different repositories. They should talk to each other.',
      sub: 'agents-room lets agent sessions meet in a shared room, ask for what they need by file or tag, and hold the files they are editing — across repositories, across harnesses, with no daemon.',
      primary: 'Get started',
      secondary: 'Read the docs',
      note: 'One static binary. No server, no port, no config file.',
    },
    problem: {
      eyebrow: 'The problem',
      title: 'You are the courier',
      paragraphs: [
        'A team keeps three repositories: an API, the storefront that calls it, and the mobile app. Each has its own agent, in its own terminal.',
        'The mobile agent needs a field the API does not return yet. The API agent is midway through a rename. The web agent is building against the current shape and will break without knowing.',
        'Nothing connects those three windows. So you copy the request from one chat into another, remember who promised what, and find out about the break when the build fails.',
      ],
      punch: 'agents-room is the layer that is missing.',
    },
    how: {
      eyebrow: 'How it works',
      title: 'Three commands, one loop',
      lead: 'Everything else — MCP tools, harness hooks, autonomy levels — exists so this happens without you typing.',
      steps: [
        {
          command: 'agents-room join --also shared:storefront',
          title: 'Meet in a room',
          body: 'Each session joins its own repository room automatically. A shared room is where agents from different repositories find each other.',
        },
        {
          command: 'agents-room send --to file:api:src/api/users.go',
          title: 'Ask whoever holds the file',
          body: 'Address a message by what rather than who: by file path, or by tag. file:<repo>:<path> names the checkout, so it reaches the holder in another repository with the path written as it appears there.',
        },
        {
          command: 'agents-room lease claim --path src/api/users.go',
          title: 'Hold the file while you agree',
          body: 'A lease is exclusive and expires. Two agents claiming one file cannot both win — even when they only meet in a shared room.',
        },
      ],
    },
    features: {
      eyebrow: 'What you get',
      title: 'The questions harnesses do not answer',
      lead: 'Most harnesses can message another session if you know its id. That is not the hard part.',
      items: [
        {
          title: 'Who is working on this interface?',
          body: 'Every session in the room, which repository it came from, and whether it is actually still running or just a leftover row.',
        },
        {
          title: 'Who is touching this file?',
          body: 'Leases on paths with expiry and stale-holder detection — across repositories, not just inside one working tree.',
        },
        {
          title: 'Is my read still valid?',
          body: 'A warning when a file changed under you, so a long edit does not silently land on a stale base.',
        },
        {
          title: 'Reach by what, not who',
          body: 'file:api:src/api/users.go reaches its holder in another repository. tag:mobile reaches everyone on the mobile client.',
        },
        {
          title: 'Wakes an idle agent',
          body: 'A recipient is told that messages are waiting and fetches the body itself, so the context stays small and the delivery state stays honest.',
        },
        {
          title: 'No daemon',
          body: 'One SQLite file in WAL mode. The CLI, the MCP server and every hook are short-lived processes that open it directly.',
        },
      ],
    },
    compat: {
      eyebrow: 'Compatibility',
      title: 'Plugs into what you already run',
      body: 'One binary is the CLI, the MCP server and the harness hook. Nothing else to install.',
      items: ['Claude Code', 'Codex', 'Hermes', 'Any MCP client', 'Linux', 'macOS', 'Windows'],
    },
    install: {
      eyebrow: 'Install',
      title: 'One command',
      body: 'A single static binary for Linux, macOS and Windows on amd64 and arm64. No cgo, no external runtime.',
      command: 'go install github.com/asovetov/agents-room/cmd/agents-room@latest',
      note: 'Release binaries are also available, with signed checksums that agents-room update verifies for you.',
    },
    footer: {
      built: 'Built for agents that have to agree.',
      links: [
        { label: 'GitHub', href: repo },
        { label: 'Wiki', href: `${repo}/tree/main/docs/wiki` },
        { label: 'Releases', href: `${repo}/releases` },
        { label: 'GPLv3', href: `${repo}/blob/main/LICENSE` },
      ],
    },
  },

  ru: {
    langLabel: 'RU',
    title: 'agents-room — координация ИИ-агентов',
    description:
      'ИИ-агенты из разных репозиториев встречаются в общей комнате, пишут друг другу по файлу или тегу и придерживают файлы, которые правят.',
    nav: { how: 'Как работает', features: 'Что даёт', install: 'Установка', github: 'GitHub' },
    hero: {
      eyebrow: 'Агенты договариваются между репозиториями',
      headline: 'Агенты работают в разных репозиториях. Пусть договариваются сами.',
      sub: 'agents-room сводит сессии агентов в общую комнату. Там можно попросить нужное по файлу или тегу и придержать файл, пока идёт разговор. Работает между репозиториями и харнессами, демон не нужен.',
      primary: 'Как начать',
      secondary: 'Документация',
      note: 'Один статический бинарник. Ни сервера, ни порта, ни конфигурации.',
    },
    problem: {
      eyebrow: 'Проблема',
      title: 'Курьером работаете вы',
      paragraphs: [
        'Есть три репозитория: API, витрина, которая его вызывает, и мобильное приложение. В каждом сидит свой агент, у каждого свой терминал.',
        'Мобильному агенту нужно поле, которого API пока не отдаёт. Агент в API в это время переименовывает что-то другое. Витрина написана под нынешний ответ и сломается, если переименование приедет без неё.',
        'Эти три окна друг о друге не знают. Просьбу из чата в чат переносите вы, вы помните, кто что обещал, и о поломке узнаёте, когда упала сборка.',
      ],
      punch: 'agents-room — тот самый недостающий слой.',
    },
    how: {
      eyebrow: 'Как это работает',
      title: 'Три команды — весь цикл',
      lead: 'Всё остальное — MCP-инструменты, хуки, уровни автономии — нужно только для того, чтобы это работало без вас.',
      steps: [
        {
          command: 'agents-room join --also shared:storefront',
          title: 'Сойтись в комнате',
          body: 'В комнату своего репозитория сессия попадает сама. Общая комната — место, где агенты из разных репозиториев видят друг друга.',
        },
        {
          command: 'agents-room send --to file:api:src/api/users.go',
          title: 'Спросить владельца файла',
          body: 'Сообщение адресуют не человеку, а предмету: файлу или тегу. file:<repo>:<path> называет чекаут, поэтому дойдёт до держателя в другом репозитории — путь пишется так, как он выглядит там.',
        },
        {
          command: 'agents-room lease claim --path src/api/users.go',
          title: 'Придержать файл на время разговора',
          body: 'Лиз выдаётся одному и сам истекает по сроку. Двое не займут один файл одновременно, даже если видят друг друга только в общей комнате.',
        },
      ],
    },
    features: {
      eyebrow: 'Что это даёт',
      title: 'Вопросы, на которые харнесс не ответит',
      lead: 'Почти любой харнесс умеет написать другой сессии — были бы её id. Сложность не в этом.',
      items: [
        {
          title: 'Кто работает над этим интерфейсом?',
          body: 'Видно все сессии в комнате: из какого они репозитория и работают сейчас или это просто оставшаяся запись.',
        },
        {
          title: 'Кто трогает этот файл?',
          body: 'Лизы на путь со сроком и проверкой, не протух ли держатель. Между репозиториями, а не только внутри одного.',
        },
        {
          title: 'Моё чтение ещё в силе?',
          body: 'Приходит предупреждение, если файл изменился у вас под руками. Долгая правка не ляжет молча на устаревшую версию.',
        },
        {
          title: 'Адресат — предмет, а не человек',
          body: 'file:api:src/api/users.go дойдёт до владельца файла в другом репозитории. tag:mobile — до всех, кто делает мобильный клиент.',
        },
        {
          title: 'Поднимает спящего агента',
          body: 'Получатель узнаёт, что ему пишут, а текст забирает сам. Контекст остаётся коротким, а статус доставки не врёт.',
        },
        {
          title: 'Демона нет',
          body: 'Один файл SQLite в режиме WAL. CLI, MCP-сервер и каждый хук — короткоживущие процессы, которые открывают его напрямую.',
        },
      ],
    },
    compat: {
      eyebrow: 'Совместимость',
      title: 'Встраивается в то, что уже стоит',
      body: 'Один бинарник — это сразу CLI, MCP-сервер и хук. Больше ничего ставить не нужно.',
      items: ['Claude Code', 'Codex', 'Hermes', 'Любой MCP-клиент', 'Linux', 'macOS', 'Windows'],
    },
    install: {
      eyebrow: 'Установка',
      title: 'Одна команда',
      body: 'Один статический бинарник для Linux, macOS и Windows, amd64 и arm64. Без cgo и без внешних зависимостей.',
      command: 'go install github.com/asovetov/agents-room/cmd/agents-room@latest',
      note: 'Можно взять и готовый бинарник из релиза — контрольные суммы подписаны, agents-room update проверит их сам.',
    },
    footer: {
      built: 'Для агентов, которым нужно договориться.',
      links: [
        { label: 'GitHub', href: repo },
        { label: 'Вики', href: `${repo}/tree/main/docs/wiki` },
        { label: 'Релизы', href: `${repo}/releases` },
        { label: 'GPLv3', href: `${repo}/blob/main/LICENSE` },
      ],
    },
  },
};
