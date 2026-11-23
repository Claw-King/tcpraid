# 教程：PostgreSQL 流复制

PostgreSQL 的备库通过普通的 PostgreSQL 连接从主库获取数据。`pg_basebackup` 用一条连接复制整个集群，加 `-X stream` 时再开第二条连接传预写日志，之后备库用一条连接持续接收日志。这篇教程让这些连接经过 tcpraid。它最有用的场合，是经长距离线路给大数据库做基础备份。

**试过什么。** 下面这些命令是在容器里用 PostgreSQL 17 跑的。主库在一个节点后面，在另一个节点上建规则、让 `pg_basebackup` 经规则连过去(137 MB)，用它启动备库，在主库上插入的行在备库上能查到。然后把主库所在站点的节点停掉五秒再启动，备库自己重新连上并追平了。**没试过的：**大数据库、长距离线路、PostgreSQL 客户端和服务器之间的 TLS、回环地址上 `trust` 以外的认证方式。

## 环境

| | 站点 A | 站点 B |
|---|---|---|
| 有 | 主库，在 192.0.2.10 的 5432 端口 | 备库 |
| tcpraid 节点 | `nas-a` | `nas-b` |

两个节点已经配对。主库接受来自 `nas-a` 地址的复制连接，因为在主库看来，连接来自它自己站点上的节点。和配置任何备库时一样，这需要一个带 `REPLICATION` 属性的角色，以及 `pg_hba.conf` 里对应的一行 `replication`。

```
site-a$ psql -c "create role repl replication login"
# pg_hba.conf on the primary; 192.0.2.5 is nas-a
host replication repl 192.0.2.5/32 METHOD
```

`METHOD` 是你给备库用的认证方式。为这篇教程做的那次运行里，所有东西都在一台机器上，对回环地址用了 `trust`，那只适合测试。

## 第 1 步：在 nas-b 上建规则

在 `nas-b` 的网页界面里打开 规则 >“新建规则”。监听地址和端口，如果备库和节点在同一台主机上就填 `127.0.0.1:15432`，否则填一个备库连得到的地址。对端选 `nas-a`，目的地填 `192.0.2.10:5432`。或者：

```
nas-b$ tcpraid rules add pg -peer nas-a -listen 127.0.0.1:15432 -dest 192.0.2.10:5432
rule pg added: listening
```

如果 `nas-b` 跑在容器里，容器里的 `127.0.0.1` 是容器自己的。这时改为监听 `0.0.0.0`，端口用容器映射出来的，例如 50000，并让备库连主机的地址和这个端口。另外，如果 `nas-a` 跑在主库所在主机的容器里，主库看到的连接来自这个容器在主机上的地址，而不是 192.0.2.5，所以 `pg_hba.conf` 里那一行要写这个地址。

## 第 2 步：经规则做基础备份

在备库所在的主机上，让 `pg_basebackup` 连规则，而不是连主库：

```
site-b$ pg_basebackup -h 127.0.0.1 -p 15432 -U repl -D /var/lib/postgresql/standby -R -X stream
```

`-R` 会写好复制用的配置(`standby.signal` 和一行 `primary_conninfo`)，所以备库之后会一直使用规则的地址：

```
primary_conninfo = 'user=repl ... host=127.0.0.1 port=15432 ...'
```

## 第 3 步：启动备库并检查

在新的数据目录上启动 PostgreSQL。在主库上：

```
site-a$ psql -c "select client_addr, state, sync_state from pg_stat_replication"
 client_addr |   state   | sync_state
-------------+-----------+------------
 192.0.2.5   | streaming | async
```

客户端地址是 `nas-a` 的地址。在节点上，备库的连接是一个会话，复制运行多久它就存在多久：

```
nas-b$ tcpraid rules
NAME  TAKES            PEER   DESTINATION      SESSIONS  STATE
pg    127.0.0.1:15432  nas-a  192.0.2.10:5432  1         listening
```

## 可以期待什么

- 一个不繁忙的数据库，日志流很小，tcpraid 对它没有任何增益。如果线路属于并行连接有帮助的那种，收益在基础备份上，以及长时间中断之后的追赶上。
- 如果线路断了，两个节点会把连接保持最长 15 分钟，然后接着传。如果有节点重启，备库的连接会被重置，PostgreSQL 会重新连接，和任何连接断掉之后一样。
- 在两个节点之间，数据流在 TLS 1.3 里面。在每个站点上 PostgreSQL 和本地节点之间，它是什么样取决于 PostgreSQL 自己在这条连接上怎么做。

## 出处

- PostgreSQL 文档：`pg_basebackup`("The backup is made over a regular PostgreSQL connection that uses the replication protocol"；`-X stream` "will open a second connection to the server")，
  `https://www.postgresql.org/docs/current/app-pgbasebackup.html`；日志传送备库，`https://www.postgresql.org/docs/current/warm-standby.html`
- PostgreSQL 17.12 的 `pg_basebackup --help`："-R, --write-recovery-conf  write configuration for replication"。
