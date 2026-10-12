-- Lake, права AGDLP и производные таблицы (roadmap §4, §6, §7).
--
-- Отдельная миграция от 0001 не из деления на файлы, а потому что 0001 —
-- дамп Alembic чужого MVP, и его нельзя переписывать: от него зависят
-- knowledge_bases, documents и jobs. Здесь только то, чего в 0001 нет:
-- журнал lake с разбиением по времени, очередь мёртвых сообщений, модель
-- прав AGDLP, граф и состояние коннекторов.
--
-- Два решения, которые читаются не из типа колонок, а из комментариев, и
-- поэтому вынесены в начало файла.
--
-- 1) ClickHouse и Kafka — фаза 2 (§9). Сейчас lake лежит в Postgres, но за
--    интерфейсом ports.EventStore: подключение ClickHouse не должно
--    затронуть ни одного сценария, поэтому ни одна таблица ниже не упоминает
--    свою колонку «под ClickHouse» и ни один потребитель не знает, где она.
--
-- 2) Разбиение RANGE идёт по occurred_at, а первичный ключ включает его же:
--    Postgres требует, чтобы уникальный индекс партиционированной таблицы
--    содержал весь ключ партициции, иначе CREATE UNIQUE INDEX (event_id)
--    падает с «unique constraint on partitioned table must include all
--    partitioning columns». Побочный эффект тот же важнее самого разбиения:
--    ON CONFLICT для партиционированной таблицы умеет работать только с
--    таким составным индексом, а (event_id, occurred_at) не выражает
--    идемпотентность §4 — occurred_at при ретрае всегда другой. Поэтому
--    ключ идемпотентности живёт отдельно, в raw_event_ids, и именно он
--    единственный закрывает повтор. Разбиение при этом остаётся: старые
--    партиции отсоединяются и удаляются вместе с TTL, без переписывания
--    живых строк.


-- ── Реестр идемпотентности lake ─────────────────────────────────────────

-- Реестр событий: одна строка на событие, независимо от того, сколько раз
-- стадия повторялась. Это единственное место, где закрывается идемпотентность
-- §4 («все стадии идемпотентны по ключу doc_id + content_hash»), потому что
-- event_id детерминирован (UUIDv5 от version|tenant|stage|doc_id|hash) и
-- при ретрае не меняется — а occurred_at меняется всегда, поэтому уникальный
-- индекс на самой raw_events такой повтор поймать не может (см. доказательство
-- в комментарии выше).
--
-- Наружу из схемы это не выходит: писатель вставляет сюда и в raw_events в
-- одной транзакции, а читатель лака сюда не ходит вообще. Конкурентные
-- повторы блокируются на этом индексе, а не на проверке «а нет ли уже?»:
-- два параллельных ретрая одного документа оба увидят «нет», оба дойдут до
-- raw_events и запишут по копии события.
--
-- tenant_id едет в каждой строке и в ключе: §3/§4 требуют его с первого дня
-- именно потому, что он дешёвый, и фаза 2 имеет превратить таблицу в
-- ClickHouse Distributed по (tenant_id, doc_id).
CREATE TABLE raw_event_ids (
    event_id UUID NOT NULL,
    tenant_id UUID NOT NULL,
    -- occurred_at копируется, чтобы по реестру можно было понять, в какое
    -- окно попал оригинал, не читая партиционированную таблицу.
    occurred_at TIMESTAMP WITH TIME ZONE NOT NULL,
    PRIMARY KEY (event_id)
);

COMMENT ON TABLE raw_event_ids IS
    'Реестр идемпотентности lake (§4): один event_id — одна запись в raw_events. '
    'Отдельная таблица обязательна: у партиционированной по occurred_at таблицы '
    'не может быть уникального индекса только по event_id, а составной '
    '(event_id, occurred_at) не защищает от ретрая с новым occurred_at.';


-- ── Lake: журнал только на добавление ────────────────────────────────────

-- Сырой слой — единственный источник правды (§1, принцип 1). Поисковый индекс,
-- граф, PageRank и эмбеддинги пересобираются из него за часы, поэтому журнал
--Append-only и никогда не правится: «починить» событие в нём нельзя, можно
-- только дописать новое.
--
-- Разбиение RANGE по occurred_at выбрано по трём причинам, и все три важнее
-- самой разбивки: TTL старых данных — это DETACH и DROP одной партиции, без
-- переписывания горячих строк; пересборка индекса читает ленту времени
-- последовательно; подключение ClickHouse на фазе 2 (§9) получает таблицы,
-- которые уже разложены по датам.
CREATE TABLE raw_events (
    event_id UUID NOT NULL,
    version VARCHAR(64) NOT NULL,
    tenant_id UUID NOT NULL,
    stage VARCHAR(32) NOT NULL,
    doc_id VARCHAR(300) NOT NULL,
    -- Пустой content_hash — законное состояние: у события стадии collect сырьё
    -- ещё не разобрано и не посчитано (§4, «ключ doc_id + content_hash»).
    --
    -- Ширина 128, а не 64: хэш едет с префиксом алгоритма («sha256:…»), и тот же
    -- формат использует весь остальной конверт (block_id, chunk_id). Проверка
    -- ниже ограничена формой, а не длиной — длина зависела бы от выбранного
    -- алгоритма и ломалась бы на следующем хэше, который окажется длиннее.
    content_hash VARCHAR(128) NOT NULL DEFAULT '',
    occurred_at TIMESTAMP WITH TIME ZONE NOT NULL,
    payload JSONB NOT NULL,
    received_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL,
    -- occurred_at в ключе обязателен: без него CREATE TABLE ... PARTITIONED
    -- с UNIQUE(event_id) отвергается сервером. Обратная сторона — что сам
    -- этот индекс не выражает идемпотентность, и её закрывает raw_event_ids.
    PRIMARY KEY (event_id, occurred_at),
    CONSTRAINT ck_raw_events_stage CHECK (stage IN ('collect','normalize','enrich','index','derive')),
    -- Проверяется форма, а не значение: завтрашний raw.v2 должен писаться в
    -- базу без миграции. Непонимание версии лечится на стороне писателя,
    -- который отправляет событие в dead_letter_events, а не запретом в схеме
    -- — иначе новая версия не попадёт в lake вообще.
    CONSTRAINT ck_raw_events_version CHECK (version ~ '^raw\.v[0-9]+$'),
    CONSTRAINT ck_raw_events_payload CHECK (jsonb_typeof(payload) = 'object'),
    -- Пусто либо «<алгоритм>:<hex>». Алгоритм не перечислен намеренно: смена
    -- хэш-функции не должна требовать миграции схемы (§8: новая технология
    -- появляется, когда её показали замеры, а не когда кто-то решил заранее).
    CONSTRAINT ck_raw_events_content_hash CHECK (
        content_hash = '' OR content_hash ~ '^[a-z0-9]+:[0-9a-f]+$'
    )
) PARTITION BY RANGE (occurred_at);

COMMENT ON TABLE raw_events IS
    'Lake: журнал только на добавление (§1, принцип 1; §4, конвейер пяти стадий). '
    'Источник правды, из которого пересобираются индекс, граф и PageRank.';

COMMENT ON COLUMN raw_events.version IS
    'Версия схемы payload (§4, «схемы событий версионируются, raw.v1»). В едете в каждом событии, потому что читатель разбирает payload, не зная, кто его написал.';

COMMENT ON COLUMN raw_events.received_at IS
    'Момент записи в Postgres. Отличается от occurred_at (событие случилось) намеренно: лаг ингеста считается по этой паре, и без неё «свежесть» была бы неотличима от задержки базы.';

-- DEFAULT-партиция обязательна: конвейер не должен падать из-за события,
-- пришедшего с чужой частоты или со сбитыми часами.
--
-- Обратная сторона — два правила ухода из неё, оба проверены на PostgreSQL 16
-- и оба неочевидны.
--
-- Первое: партицию на месяц создают ДО того, как в неё попадёт первая строка,
-- и создают заранее, на несколько шагов вперёд. Пока строки месяца лежат в
-- DEFAULT, повторный CREATE TABLE ... PARTITION OF на этот месяц отвергается
-- («updated partition constraint for default partition would be violated by
-- some row»), и лечится это только переносом строк из DEFAULT вручную.
--
-- Второе: TTL делается через DETACH и DROP, а не через DELETE. DELETE по
-- строкам lake запрещён триггером ниже, и это ровно то, что нужно: «удалить
-- старые события» обязано означать «отрезать целый месяц», а не «выборочно
-- вычистить половину ленты». При этом CONCURRENTLY на DETACH неприменим, пока
-- существует DEFAULT-партиция («cannot detach partitions concurrently when a
-- default partition exists»), то есть DETACH берёт короткий ACCESS EXCLUSIVE
-- на родителе. Для ночного TTL по месяцам это приемлемо; для горячего пути
-- надо сначала убрать DEFAULT, и это уже отдельное решение.
CREATE TABLE raw_events_default PARTITION OF raw_events DEFAULT;

CREATE TABLE raw_events_2026_11 PARTITION OF raw_events
    FOR VALUES FROM ('2026-11-01 00:00:00+00') TO ('2026-12-01 00:00:00+00');

CREATE TABLE raw_events_2026_12 PARTITION OF raw_events
    FOR VALUES FROM ('2026-12-01 00:00:00+00') TO ('2027-01-01 00:00:00+00');

CREATE TABLE raw_events_2027_01 PARTITION OF raw_events
    FOR VALUES FROM ('2027-01-01 00:00:00+00') TO ('2027-02-01 00:00:00+00');

-- Лента одного тенанта во времени: главный запрос пересборки индекса (§9,
-- «индекс хотя бы раз полностью пересобран из lake по учебной тревоге»).
-- event_id дописан третьим, чтобы сортировка (occurred_at, event_id), на
-- которой держится воспроизводимость пересборки, шла по индексу, а не через
-- SORT: два прогона пересборки обязаны получить события в одном порядке.
CREATE INDEX ix_raw_events_tenant_occurred ON raw_events (tenant_id, occurred_at, event_id);

-- Документ во времени: чтение всех стадий одного объекта источника.
CREATE INDEX ix_raw_events_tenant_doc ON raw_events (tenant_id, doc_id, occurred_at);

-- Точечная выборка события: разбор пачки стадий по известным event_id.
-- Отдельно от первичного ключа, хотя и начинается с той же колонки: PK
-- партиционирован, и его индекс на каждой партиции упорядочен по паре
-- (event_id, occurred_at) — выбор одного события без окна времени
-- вынуждена обходить все партиции, а этот индекс позволяет планировщику
-- обойтись теми, где это событие физически может лежать.
CREATE INDEX ix_raw_events_event_id ON raw_events (event_id);

-- Append-only как проверяемое правило, а не как соглашение. UPDATE и DELETE
-- в lake не имеют легального применения: событие — факт, а факт не
-- переписывается; если кажется, что его надо поправить, это означает баг
-- в пересборке, и молчаливый no-op скрыл бы его до следующего инцидента.
-- Поэтому запрет поднимает ошибку, а не гасит операцию.
--
-- Триггер BEFORE ROW на партиционированной таблице клонируется в каждую
-- партицию (PostgreSQL 13+), поэтому правило действует и на строки, попавшие
-- в DEFAULT. TRUNCATE триггер не вызывает — очистка тестов и разгрузка
-- партиции остаются возможными.
CREATE FUNCTION raw_events_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION
        'raw_events — журнал только на добавление (§1, принцип 1): % запрещён. '
        'Изменение события означает баг пересборки: событие не переписывают, '
        'в lake дописывают новое.', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_raw_events_append_only
    BEFORE UPDATE OR DELETE ON raw_events
    FOR EACH ROW EXECUTE FUNCTION raw_events_append_only();


-- ── Очередь мёртвых сообщений ────────────────────────────────────────────

-- §4 требует «у каждого топика есть dead-letter-очередь». На фазе 1 топик —
-- это стадия конвейера, и очередь одна на lake: событие, которое не удалось
-- разобрать, обязано где-то осесть с причиной, иначе оно исчезает молча и
-- пересборка индекса даже не заметит, что чего-то не хватает.
--
-- Колонки события здесь nullable намеренно. В очередь попадает ровно то,
-- что не прошло проверки, поэтому требовать корректный tenant_id или валидный
-- stage значило бы терять самое интересное — битое событие. Валидировать надо
-- до записи в lake, а не здесь.
CREATE TABLE dead_letter_events (
    id UUID NOT NULL,
    event_id UUID,
    tenant_id UUID,
    version VARCHAR(64),
    stage VARCHAR(32),
    doc_id VARCHAR(300),
    content_hash VARCHAR(128),
    occurred_at TIMESTAMP WITH TIME ZONE,
    -- payload в двух колонках, потому что битый JSON нельзя положить в jsonb:
    -- такое событие упало бы на вставке в очередь и исчезло, то есть ровно то,
    -- ради чего очередь существует. payload_raw хранит байты как есть (это и
    -- нужно для разбора), payload — их разобранный вид, если разобрать удалось
    -- (это нужно, чтобы читать очередь запросами).
    payload JSONB,
    payload_raw TEXT,
    reason_code VARCHAR(64) NOT NULL,
    reason_message TEXT NOT NULL,
    attempts INTEGER DEFAULT '1' NOT NULL,
    failed_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT ck_dead_letter_attempts CHECK (attempts >= 1),
    CONSTRAINT ck_dead_letter_payload CHECK (payload IS NOT NULL OR payload_raw IS NOT NULL)
);

COMMENT ON TABLE dead_letter_events IS
    'Dead-letter lake (§4: «у каждого топика есть dead-letter-очередь»). Хранит событие, '
    'которое не удалось записать или разобрать, вместе с причиной: событие, исчезнувшее '
    'молча, хуже, чем упавшее явно — пересборка индекса даже не заметит пробела.';

COMMENT ON COLUMN dead_letter_events.reason_code IS
    'Машиночитаемая причина (unknown_version, invalid_payload, invalid_event): по ней очередь разбирают и считают, а reason_message читает человек.';

-- Разбор очереди: что упало у тенанта и на какой стадии, за когда.
CREATE INDEX ix_dead_letter_tenant_stage ON dead_letter_events (tenant_id, stage, failed_at);

-- Один прогон события — одна строка. Реплей неисправного события не должен
-- плодить дубли: иначе повторная доставка забивает очередь тем же самым и
-- за ней уже не видно новых отказов. Событий без event_id (их id не
-- разобрался) под ограничение не попадают — дедуплицировать их нечем.
CREATE UNIQUE INDEX uq_dead_letter_event ON dead_letter_events (event_id) WHERE event_id IS NOT NULL;


-- ── Права: AGDLP (§6) ────────────────────────────────────────────────────

-- §6: права проверяются в самом запросе к индексу по схеме AGDLP, а вход
-- делегируется IdP. Ниже — четыре шага AGDLP и правило, из-за которого эта
-- часть схемы выглядит так, а не проще.
--
-- ГЛАВНОЕ. Правил §6 ровно три: пользователи состоят только в глобальных
-- группах, глобальные группы — только в ресурсных, права назначаются только
-- ресурсным группам. Запретов (deny) нет: только разрешения, «иначе фильтр
-- в индексе усложняется». Эти правила записаны не соглашением, а формой
-- схемы:
--
--   * пользователь попадает в acl_global_group_members — и больше никуда;
--   * ресурсная группа попадает в acl_resource_group_members — и больше никуда;
--   * acl_permissions ссылается только на ресурсную группу.
--
-- Соединить два средних уровня в один обход нельзя: это разные таблицы с
-- независимо выдаваемыми id, и «просто передать id ресурсной группы в
-- membership глобальных» прошло бы, если бы тип не был помечен. Поэтому
-- group_kind — дискриминант типа с CHECK, зафиксированным значением и
-- составным внешним ключом: в membership глобальных групп физически не может
-- оказаться id ресурсной группы, а в membership ресурсных — id пользователя.
-- Это не избыточность, это разница между правилом, которое держит код, и
-- правилом, которое держит база; ворота фазы A — «0 утечек прав» (§9).

-- Account: пользователь. Создаётся при первом входе (JIT, §6 «Аутентификация»),
-- поэтому запись заводит конвейер аутентификации, а не миграция.
-- user_key — это внешний идентификатор (sub IdP или почта), и он же входит
-- в строку принципала «user:<id>», поэтому строки индекса и строки БД
-- собираются из одного значения.
CREATE TABLE acl_users (
    id UUID NOT NULL,
    tenant_id UUID NOT NULL,
    user_key VARCHAR(300) NOT NULL,
    email VARCHAR(320),
    display_name VARCHAR(300),
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT uq_acl_users_tenant_key UNIQUE (tenant_id, user_key),
    -- Цель составных внешних ключей membership-таблиц: id пользователя имеет
    -- смысл только внутри своего тенанта.
    CONSTRAINT uq_acl_users_tenant_id UNIQUE (tenant_id, id),
    CONSTRAINT ck_acl_users_key_not_blank CHECK (btrim(user_key) <> ''),
    FOREIGN KEY(tenant_id) REFERENCES tenants (id)
);

COMMENT ON TABLE acl_users IS
    'Account по AGDLP (§6): пользователь, заводится при первом входе (JIT). '
    'user_key — внешний идентификатор IdP и одновременно часть строки «user:<id>».';

-- Global group: группа людей по роли или команде, приходит из claim groups
-- IdP или из SCIM (§6). Ключ группы — то самое значение claim, поэтому
-- разворачивание принципалов не требует справочника.
CREATE TABLE acl_global_groups (
    id UUID NOT NULL,
    tenant_id UUID NOT NULL,
    group_key VARCHAR(300) NOT NULL,
    -- Константа CHECK, а не перечисление: group_kind участвует в составном
    -- внешнем ключе, и это делает невозможным положить в membership глобальных
    -- групп id ресурсной группы (см. комментарий выше про дискриминант типа).
    group_kind VARCHAR(16) NOT NULL DEFAULT 'global',
    display_name VARCHAR(300),
    -- synced_at — момент последней сверки с IdP. Нужен, чтобы отличать
    -- «группа вышла из состава давно» от «группы ещё не синхронизировали»: без
    -- него отзыв доступа при уходе человека выглядит как вечное отсутствие.
    synced_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT ck_acl_global_groups_kind CHECK (group_kind = 'global'),
    CONSTRAINT ck_acl_global_groups_key_not_blank CHECK (btrim(group_key) <> ''),
    CONSTRAINT uq_acl_global_groups_tenant_key UNIQUE (tenant_id, group_key),
    -- Цель составных внешних ключей: id без tenant_id не имеет смысла, а
    -- составной ключ не может ссылаться на id, не покрытый уникальностью.
    CONSTRAINT uq_acl_global_groups_tenant_id UNIQUE (tenant_id, id),
    CONSTRAINT uq_acl_global_groups_ref UNIQUE (tenant_id, id, group_kind),
    FOREIGN KEY(tenant_id) REFERENCES tenants (id)
);

COMMENT ON TABLE acl_global_groups IS
    'Global group по AGDLP (§6): группа людей по роли или команде, ключ приходит из claim groups IdP. '
    'Пользователи состоят только здесь — прямой путь к ресурсным группам в схеме закрыт внешним ключом.';

-- Шаг «пользователь → глобальная группа». На MVP заполняется из claim groups
-- при входе; членство в таблице появляется вместе со SCIM (§6, фаза 2), и
-- оракул прав читает оба источника сразу, поэтому появление SCIM — это
-- изменение данных, а не кода.
CREATE TABLE acl_global_group_members (
    tenant_id UUID NOT NULL,
    group_id UUID NOT NULL,
    group_kind VARCHAR(16) NOT NULL DEFAULT 'global',
    user_id UUID NOT NULL,
    PRIMARY KEY (tenant_id, group_id, user_id),
    CONSTRAINT ck_acl_global_members_kind CHECK (group_kind = 'global'),
    FOREIGN KEY(tenant_id, group_id, group_kind) REFERENCES acl_global_groups (tenant_id, id, group_kind),
    FOREIGN KEY(tenant_id, user_id) REFERENCES acl_users (tenant_id, id)
);

-- Контейнер — то, на чём висят права: space, канал, папка, ящик (§6). Это
-- ровно то значение, которое §6 называет space_id и которое уходит в индекс
-- термом, поэтому смена прав на контейнер применяется пересчётом списка
-- space у пользователя и не требует переиндексации документов.
CREATE TABLE acl_containers (
    id UUID NOT NULL,
    tenant_id UUID NOT NULL,
    container_key VARCHAR(300) NOT NULL,
    -- Канал, папка, ящик, wiki-пространство. Нужен, чтобы UI админки знал,
    -- как называть контейнер, и чтобы выборка «все контейоры источника X»
    -- не сканировала все таблицы прав.
    kind VARCHAR(64) NOT NULL,
    source VARCHAR(64) NOT NULL,
    display_name VARCHAR(300),
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT uq_acl_containers_tenant_key UNIQUE (tenant_id, container_key),
    CONSTRAINT uq_acl_containers_tenant_id UNIQUE (tenant_id, id),
    CONSTRAINT ck_acl_containers_kind CHECK (kind IN ('space','channel','folder','mailbox','wiki')),
    CONSTRAINT ck_acl_containers_key_not_blank CHECK (btrim(container_key) <> ''),
    FOREIGN KEY(tenant_id) REFERENCES tenants (id)
);

COMMENT ON TABLE acl_containers IS
    'Контейнер, на котором висят права (§6: «права висят на контейнере, точечный acl нужен только для исключений»). '
    'container_key — это space_id из §6 и одновременно терм индекса.';

-- Domain Local group: ресурсная группа «кто может читать эту коллекцию».
-- Именование res:<контейнер>:<действие> взято из §6 дословно и закреплено
-- CHECK, чтобы строки принципалов вида «group:res:space-platform:read», на
-- которых держится фильтр индекса, нельзя было набрать в рассыпь.
CREATE TABLE acl_resource_groups (
    id UUID NOT NULL,
    tenant_id UUID NOT NULL,
    resource_key VARCHAR(300) NOT NULL,
    group_kind VARCHAR(16) NOT NULL DEFAULT 'resource',
    display_name VARCHAR(300),
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT ck_acl_resource_groups_kind CHECK (group_kind = 'resource'),
    CONSTRAINT ck_acl_resource_groups_key CHECK (resource_key ~ '^res:[^:]+:[a-z]+$'),
    CONSTRAINT uq_acl_resource_groups_tenant_key UNIQUE (tenant_id, resource_key),
    CONSTRAINT uq_acl_resource_groups_tenant_id UNIQUE (tenant_id, id),
    CONSTRAINT uq_acl_resource_groups_ref UNIQUE (tenant_id, id, group_kind),
    FOREIGN KEY(tenant_id) REFERENCES tenants (id)
);

COMMENT ON TABLE acl_resource_groups IS
    'Domain Local group по AGDLP (§6): ресурсная группа «кто может читает эту коллекцию». '
    'Права выдаются только таким группам, и в глобальные группы пользователя они попасть не могут.';

-- Шаг «глобальная группа → ресурсная группа». Соединить напрямую
-- пользователя с ресурсной группой здесь нельзя: колонка называется
-- global_group_id и внешний ключ ведёт в acl_global_groups.
CREATE TABLE acl_resource_group_members (
    tenant_id UUID NOT NULL,
    resource_group_id UUID NOT NULL,
    group_kind VARCHAR(16) NOT NULL DEFAULT 'resource',
    global_group_id UUID NOT NULL,
    PRIMARY KEY (tenant_id, resource_group_id, global_group_id),
    CONSTRAINT ck_acl_resource_members_kind CHECK (group_kind = 'resource'),
    FOREIGN KEY(tenant_id, resource_group_id, group_kind) REFERENCES acl_resource_groups (tenant_id, id, group_kind),
    FOREIGN KEY(tenant_id, global_group_id) REFERENCES acl_global_groups (tenant_id, id)
);

-- Permission: право ресурсной группы на контейнер (§6, read/write/admin).
--
-- Колонки deny/effect здесь нет и не появится: §6 запрещает запреты, потому
-- что они усложняют фильтр индекса. Константа не «проверяется отсутствием»,
-- а закреплена: action — закрытый набор, и строка вида «отказать всем в
-- этом контейнере» физически не имеет куда записаться.
--
-- Контейнер задаётся именно здесь, а не в acl_resource_groups. Ресурсная
-- группа — это имя способности («res:space-platform:read»), и одна и та же
-- группа по §6 может быть выдана на несколько контейнеров; если бы контейнер
-- был колонкой группы, вторую выдачу было бы записать некуда.
CREATE TABLE acl_permissions (
    tenant_id UUID NOT NULL,
    resource_group_id UUID NOT NULL,
    container_id UUID NOT NULL,
    action VARCHAR(16) NOT NULL,
    granted_by VARCHAR(300) NOT NULL,
    granted_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL,
    PRIMARY KEY (tenant_id, resource_group_id, container_id, action),
    CONSTRAINT ck_acl_permissions_action CHECK (action IN ('read','write','admin')),
    -- Название ограничения повторяет правило §6, чтобы «здесь не может быть
    -- deny» было видно при чтении схемы, а не только в этом комментарии.
    CONSTRAINT ck_acl_permissions_no_deny CHECK (action NOT IN ('deny','denied','none','no_access')),
    CONSTRAINT ck_acl_permissions_granted_by CHECK (granted_by LIKE 'user:%' OR granted_by = 'system'),
    FOREIGN KEY(tenant_id, resource_group_id) REFERENCES acl_resource_groups (tenant_id, id),
    FOREIGN KEY(tenant_id, container_id) REFERENCES acl_containers (tenant_id, id)
);

COMMENT ON TABLE acl_permissions IS
    'Permission по AGDLP (§6): право ресурсной группы на контейнер. Запретов нет и быть не может — CHECK закрепляет закрытый набор действий, колонки эффекта не существует.';

-- Оракул прав ходит по этим таблицам одним запросом и возвращает термы для
-- фильтра индекса (§6, json-фильтр с terms по space_id и по acl). Поэтому
-- индексы строятся под соединение, а не под первичные ключи.
CREATE INDEX ix_acl_global_members_user ON acl_global_group_members (tenant_id, user_id, group_id);

CREATE INDEX ix_acl_resource_members_group ON acl_resource_group_members (tenant_id, global_group_id, resource_group_id);

CREATE INDEX ix_acl_permissions_container ON acl_permissions (tenant_id, container_id, action);


-- ── Версии как неизменяемые снимки (§7, п. 8) ─────────────────────────────

-- §7: «Не берём настоящий Git». Для ингестированных документов версии бесплатны
-- — каждое изменение это новое событие в raw_events с новым content_hash, и
-- историю можно показать из lake. Для документов, написанных в самой системе,
-- версия — неизменяемый снимок плюс строка в Postgres.
--
-- Колонки добавляются, а таблица не переопределяется: её создал 0001, и от
-- неё зависят documents, jobs и уже написанные репозитории. Ни одна
-- существующая колонка не удаляется и не меняет тип.
ALTER TABLE document_versions ADD COLUMN parent_version_id UUID;

ALTER TABLE document_versions ADD COLUMN author VARCHAR(300);

ALTER TABLE document_versions ADD COLUMN message VARCHAR(1000);

ALTER TABLE document_versions ADD COLUMN snapshot_key VARCHAR;

COMMENT ON COLUMN document_versions.parent_version_id IS
    'Предыдущая версия: §7 требует версию как неизменяемый снимок с parent_version, diff считается на лету. NULL — первая версия документа.';

COMMENT ON COLUMN document_versions.author IS
    'Автор правки (§7). Для ингестированных документов совпадает с owner/author конверта (§4).';

COMMENT ON COLUMN document_versions.snapshot_key IS
    'Ключ неизменяемого снимка в S3, content-addressed по хэшу (§7). Историю документа можно собрать и из lake, но снимок — то, что реально отдают пользователю и что не зависит от пересборки.';

-- Родитель обязан быть версией того же документа того же тенанта. Ссылка
-- составная по той же причине, что и в 0001: версия сама по себе uuid,
-- который известен по URL-ответам, и без tenant_id в ключе ссылка прошла бы
-- между документами разных организаций.
ALTER TABLE document_versions ADD CONSTRAINT fk_versions_parent_version
    FOREIGN KEY(tenant_id, document_id, parent_version_id)
    REFERENCES document_versions (tenant_id, document_id, id);

-- Нумерация версий. Частичный индекс, а не обычный UNIQUE с DEFAULT:
-- у 0001 уже могут быть строки, и backfill номеров означал бы перенумерацию
-- чужой истории. NULL в существующих строках уникальности не нарушает, а там,
-- где нумерация есть, она обязана быть плотной внутри документа.
ALTER TABLE document_versions ADD COLUMN version_number INTEGER;

ALTER TABLE document_versions ADD CONSTRAINT ck_versions_number_positive CHECK (version_number IS NULL OR version_number >= 1);

CREATE UNIQUE INDEX uq_versions_tenant_document_number
    ON document_versions (tenant_id, document_id, version_number)
    WHERE version_number IS NOT NULL;


-- ── Выборочные графы (§7, п. 10) ──────────────────────────────────────────

-- §7: граф строится только из дешёвых и надёжных связей, без LLM-извлечения
-- сущностей по корпусу, и хранится таблицей в Postgres; отдельная графовая БД
-- понадобится, только если запросы уйдут глубже двух шагов (замер через
-- 90 дней после запуска, §8).
--
-- src и dst — это doc_id lake, то есть «хэш tenant + source + external_id»
-- (§4), а не id из documents. Расхождение намеренное: в lake лежат сообщения
-- Slack и почты, которых в реестре documents нет и не будет, пока их не
-- загрузили через API. Внешнего ключа на documents поэтому нет — ссылка на
-- него убила бы половину графа и превратила бы пересборку в обход реестра.
CREATE TABLE doc_edges (
    tenant_id UUID NOT NULL,
    src VARCHAR(300) NOT NULL,
    dst VARCHAR(300) NOT NULL,
    type VARCHAR(32) NOT NULL,
    weight DOUBLE PRECISION NOT NULL DEFAULT '1',
    created_by VARCHAR(300) NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL,
    PRIMARY KEY (tenant_id, src, dst, type),
    CONSTRAINT ck_doc_edges_type CHECK (type IN ('explicit','link','co_search','thread')),
    CONSTRAINT ck_doc_edges_weight CHECK (weight >= 0),
    CONSTRAINT ck_doc_edges_no_self_loop CHECK (src <> dst),
    -- created_by — тот же строковый принципал, что и в индексе: «user:<id>»
    -- для ребра, заведённого человеком (§7, explicit — «админ или владелец
    -- связал документы вручку»), и «system» для рёбер, выведенных
    -- конвейером: link, thread, co_search.
    CONSTRAINT ck_doc_edges_created_by CHECK (created_by LIKE 'user:%' OR created_by = 'system'),
    FOREIGN KEY(tenant_id) REFERENCES tenants (id)
);

COMMENT ON TABLE doc_edges IS
    'Выборочные графы (§7, п. 10): doc_edges(src, dst, type, weight, created_by). Собирается только из дешёвых связей, без LLM. Все id — doc_id lake, а не documents.id: lake покрывает источники, которых в реестре нет.';

-- Обход в один шаг от топ-результатов идёт в обе стороны (связанные — это и
-- «на кого ссылается», и «кто ссылается»), и строго внутри ACL-фильтра (§7).
CREATE INDEX ix_doc_edges_src ON doc_edges (tenant_id, src, type, weight DESC);

CREATE INDEX ix_doc_edges_dst ON doc_edges (tenant_id, dst, type, weight DESC);


-- ── Коннекторы и свежесть (§4, §9) ───────────────────────────────────────

-- §4: «PostgreSQL — пользователи, группы, ACL, настройки источников и
-- курсоры коннекторов». Курсор живёт здесь, а не в памяти процесса:
-- перезапуск воркера не должен начинать обход источника заново.
--
-- §9 требует метрику «последнее успешное обновление» и алерт на отставание.
-- Метрика не выводится из now() в момент чтения таблицы — её читает и
-- подсчитывает один и тот же код, — а лежит здесь же и обновляется писателем,
-- потому что метрика, которую нельзя посмотреть без запроса к базе, в
-- мониторинг не попадает.
CREATE TABLE connectors (
    id UUID NOT NULL,
    tenant_id UUID NOT NULL,
    name VARCHAR(64) NOT NULL,
    kind VARCHAR(64) NOT NULL,
    enabled BOOLEAN DEFAULT true NOT NULL,
    -- Курсор источника (§4, «Pull возвращает порцию и новый курсор»).
    -- Пустой курсор означает полный обход, поэтому NULL и '' — не одно и то же.
    cursor TEXT,
    cursor_updated_at TIMESTAMP WITH TIME ZONE,
    -- Метрика §9: последнее успешное обновление.
    last_success_at TIMESTAMP WITH TIME ZONE,
    last_attempt_at TIMESTAMP WITH TIME ZONE,
    last_error_code VARCHAR(64),
    last_error_message TEXT,
    consecutive_failures INTEGER DEFAULT '0' NOT NULL,
    -- Порог «отстал», задаётся на источник: у Slack и у файловой папки
    -- правдоподобная свежесть разная, и одна константа на всех заставила бы
    -- либо молчать об отказе Slack, либо поднимать тревогу на каждой папке.
    stale_after_seconds INTEGER NOT NULL DEFAULT '3600',
    last_success_items INTEGER,
    settings JSONB DEFAULT '{}' NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL,
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT now() NOT NULL,
    PRIMARY KEY (id),
    CONSTRAINT uq_connectors_tenant_name UNIQUE (tenant_id, name),
    CONSTRAINT ck_connectors_failures CHECK (consecutive_failures >= 0),
    CONSTRAINT ck_connectors_stale_after CHECK (stale_after_seconds > 0),
    CONSTRAINT ck_connectors_items CHECK (last_success_items IS NULL OR last_success_items >= 0),
    CONSTRAINT ck_connectors_settings_object CHECK (jsonb_typeof(settings) = 'object'),
    CONSTRAINT ck_connectors_name_not_blank CHECK (btrim(name) <> ''),
    FOREIGN KEY(tenant_id) REFERENCES tenants (id)
);

COMMENT ON TABLE connectors IS
    'Коннекторы, их курсоры и свежесть (§4 — курсоры в Postgres; §9 — «у каждого коннектора есть метрика последнее успешное обновление и алерт на отставание»).';

COMMENT ON COLUMN connectors.cursor IS
    'Курсор источника: пустой — полный обход. Не NULL от «курсор пустой»: у источника курсор есть не всегда, и подменять одно другим значит потерять признак первого обхода.';

-- Свежесть как представление, а не как ещё одна колонка. Отставание зависит
-- от now(), а вычисляемое выражение в столбце запрещено; материализовать его
-- значит накапливать вторую копию времени, которая расходится с первой.
-- Порог алерта — из той же строки, поэтому настройка источника и метрика
-- не могут разъехаться.
CREATE VIEW connector_freshness AS
SELECT
    c.id,
    c.tenant_id,
    c.name,
    c.kind,
    c.enabled,
    c.last_success_at,
    c.last_attempt_at,
    c.consecutive_failures,
    c.stale_after_seconds,
    EXTRACT(EPOCH FROM (now() - c.last_success_at))::BIGINT AS lag_seconds,
    (c.last_success_at IS NULL
        OR EXTRACT(EPOCH FROM (now() - c.last_success_at)) > c.stale_after_seconds) AS is_stale
FROM connectors c;

COMMENT ON VIEW connector_freshness IS
    'Метрика свежести коннектора (§9): lag_seconds — отставание от последнего успешного обновления, is_stale — превышение собственного порога источника.';
