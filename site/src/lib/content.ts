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
          command: 'agents-room send --to file:src/api/users.go',
          title: 'Ask whoever holds the file',
          body: 'Address a message by what rather than who: by file path, or by tag. It reaches the agent holding that file, in whatever repository it sits.',
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
          body: 'file:src/api/users.go reaches its holder anywhere. tag:mobile reaches everyone on the mobile client.',
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
        { label: 'MIT License', href: `${repo}/blob/main/LICENSE` },
      ],
    },
  },

  ru: {
    langLabel: 'RU',
    title: 'agents-room — координация ИИ-агентов',
    description:
      'ИИ-агенты в разных репозиториях встречаются в общей комнате, пишут друг другу по файлу или тегу и держат файлы, которые правят.',
    nav: { how: 'Как работает', features: 'Возможности', install: 'Установка', github: 'GitHub' },
    hero: {
      eyebrow: 'Координация агентов между репозиториями',
      headline: 'Ваши агенты работают в разных репозиториях. Пусть они договариваются сами.',
      sub: 'agents-room даёт агентским сессиям общую комнату: попросить нужное по файлу или тегу и удержать файл, пока идёт договорённость — между репозиториями и харнессами, без демона.',
      primary: 'Начать',
      secondary: 'Документация',
      note: 'Один статический бинарник. Ни сервера, ни порта, ни файла конфигурации.',
    },
    problem: {
      eyebrow: 'Проблема',
      title: 'Курьер — вы',
      paragraphs: [
        'У команды три репозитория: API, витрина, которая его вызывает, и мобильное приложение. В каждом свой агент, в своём терминале.',
        'Мобильному агенту нужно поле, которого API пока не отдаёт. Агент в API как раз переименовывает что-то другое. Витрина строится на текущей форме ответа и сломается, не узнав об этом.',
        'Ничто эти три окна не связывает. Поэтому просьбу из чата в чат переносите вы, вы же помните, кто что обещал, и о поломке узнаёте, когда падает сборка.',
      ],
      punch: 'agents-room — тот слой, которого не хватает.',
    },
    how: {
      eyebrow: 'Как это работает',
      title: 'Три команды, один цикл',
      lead: 'Всё остальное — MCP-инструменты, хуки харнессов, уровни автономии — нужно, чтобы это происходило без вашего участия.',
      steps: [
        {
          command: 'agents-room join --also shared:storefront',
          title: 'Встретиться в комнате',
          body: 'Каждая сессия сама входит в комнату своего репозитория. Общая комната — то место, где агенты из разных репозиториев находят друг друга.',
        },
        {
          command: 'agents-room send --to file:src/api/users.go',
          title: 'Спросить того, кто держит файл',
          body: 'Адресуйте сообщение по «что», а не по «кто»: по пути файла или по тегу. Оно дойдёт до держателя файла в любом репозитории.',
        },
        {
          command: 'agents-room lease claim --path src/api/users.go',
          title: 'Удержать файл, пока договариваетесь',
          body: 'Лиз эксклюзивен и истекает по сроку. Два агента не займут один файл одновременно — даже если встретились только в общей комнате.',
        },
      ],
    },
    features: {
      eyebrow: 'Что вы получаете',
      title: 'Вопросы, на которые харнессы не отвечают',
      lead: 'Почти любой харнесс умеет писать другой сессии, если знаешь её id. Сложность не в этом.',
      items: [
        {
          title: 'Кто работает над этим интерфейсом?',
          body: 'Все сессии в комнате, из какого они репозитория и живы ли они на самом деле или это оставшаяся строка.',
        },
        {
          title: 'Кто трогает этот файл?',
          body: 'Лизы на пути, со сроком и обнаружением протухшего держателя — между репозиториями, а не только внутри одного.',
        },
        {
          title: 'Моё чтение ещё верно?',
          body: 'Предупреждение, если файл изменился под вами: долгая правка не ляжет на устаревшую основу молча.',
        },
        {
          title: 'Адрес по «что», а не по «кто»',
          body: 'file:src/api/users.go дойдёт до держателя в любом репозитории. tag:mobile — до всех, кто делает мобильный клиент.',
        },
        {
          title: 'Будит простаивающего агента',
          body: 'Получателю сообщают, что его ждут письма, а тело он забирает сам: контекст остаётся маленьким, а статус доставки — честным.',
        },
        {
          title: 'Без демона',
          body: 'Один файл SQLite в режиме WAL. CLI, MCP-сервер и каждый хук — короткоживущие процессы, которые открывают его напрямую.',
        },
      ],
    },
    compat: {
      eyebrow: 'Совместимость',
      title: 'Встаёт в то, что у вас уже работает',
      body: 'Один бинарник — это CLI, MCP-сервер и хук харнесса. Больше ставить нечего.',
      items: ['Claude Code', 'Codex', 'Hermes', 'Любой MCP-клиент', 'Linux', 'macOS', 'Windows'],
    },
    install: {
      eyebrow: 'Установка',
      title: 'Одна команда',
      body: 'Один статический бинарник для Linux, macOS и Windows на amd64 и arm64. Без cgo и внешних зависимостей.',
      command: 'go install github.com/asovetov/agents-room/cmd/agents-room@latest',
      note: 'Есть и готовые бинарники релизов — с подписанными контрольными суммами, которые agents-room update проверит сам.',
    },
    footer: {
      built: 'Для агентов, которым надо договориться.',
      links: [
        { label: 'GitHub', href: repo },
        { label: 'Вики', href: `${repo}/tree/main/docs/wiki` },
        { label: 'Релизы', href: `${repo}/releases` },
        { label: 'Лицензия MIT', href: `${repo}/blob/main/LICENSE` },
      ],
    },
  },
};
