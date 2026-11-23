# 节点的选项和文件

## `tcpraid run` 的选项

| 参数 | 默认值 | 含义 |
|---|---|---|
| `-data DIR` | `/var/lib/tcpraid` | 数据目录；不存在时会创建(权限 0700) |
| `-api ADDR` | `127.0.0.1:9850` | 本机 API 和网页界面的监听地址(明文 HTTP) |
| `-name NAME` | 它已有的名字 | 节点的名字。给了就给节点改名。不给，节点保留原来的名字；第一次启动时用主机名。名字是 1 到 64 字节的可打印字符，两端不能有空格 |
| `-listen ADDR` | 关闭 | 供其他节点连接的监听地址(TLS)。没有它，节点可以去加入别的节点，但别的节点不能来加入它 |
| `-advertise HOST:PORT` | 无 | 其他节点连 `-listen` 时用的地址；会写进本节点生成的配对码。可以给多次 |
| `-listen-plain ADDR` | 关闭 | 不带 TLS 的对端监听地址，给 `off` 档的路径用 |
| `-cert FILE`、`-key FILE` | 无 | 一张证书和它的密钥(PEM)，给使用 `owncert` 档的对端；需要同时有 `-listen`。节点启动时会在日志里记下证书密钥的指纹 |
| `-transparent ADDR` | 关闭 | 接收由主机重定向到节点的连接的监听地址；它们去哪里由透明规则决定 |
| `-udp-listen ADDR` | 关闭 | 接收对端回执的监听地址(UDP)，对端要被告知把回执发到这里 |
| `-ui-password-file FILE` | 无 | 存放网页界面密码的文件 |

在节点里，传输层按默认设置运行；手动模式的那些调节选项在这里不适用。

日志的启动行会把实际生效的设置重复一遍：

```
node 0.1.0 name="nas-a" fingerprint=e5645f9b...7795ec86 data=/var/lib/tcpraid api=127.0.0.1:9850 listen=0.0.0.0:9851 listen-plain=off transparent=off udp-listen=off
```

### 网页界面的密码

它从哪里来，按下面的顺序：

1. `-ui-password-file FILE`：文件的内容，不含行尾。
2. 环境变量 `TCPRAID_UI_PASSWORD`(在用网页表单配置容器的地方很方便)。值为空等于没有设置。
3. 之前用过的密码：数据目录里保存着它的加盐哈希。
4. 以上都没有，而且是第一次启动：节点生成一个随机密码，并在日志里写一次。

```
web interface: no password was set, so one was made. It is shown this once: okt7wgr47g3lt6f2
web interface: to set your own, start the node with -ui-password-file FILE or the environment variable TCPRAID_UI_PASSWORD
```

用文件或环境变量给出的密码会取代已保存的那个，之后即使把文件或变量拿掉，它也继续有效。想要一个新的随机密码，就停掉节点，删掉 `ui.password`，再在不带这两者的情况下启动。

## 数据目录

(属主、大小和日期省略了。)

```
$ ls -la /var/lib/tcpraid
drwx------  .
-rw-------  api.addr
-rw-------  api.token
-rw-r--r--  identity.crt
-rw-------  identity.key
-rw-------  lock
-rw-------  state.json
-rw-------  ui.password
```

| 文件 | 内容 |
|---|---|
| `identity.key` | 节点的 Ed25519 私钥(PKCS#8，PEM)。第一次启动时创建，之后绝不替换；密钥文件读不了是一个错误 |
| `identity.crt` | 这把密钥的自签名证书 |
| `state.json` | 节点名字、各个对端及其配对密钥和路径、未使用的配对码(编号和一个哈希)、规则。由程序写入，不要手工改 |
| `api.token` | 本机 API 的访问令牌 |
| `ui.password` | 网页界面密码的加盐哈希(PBKDF2-HMAC-SHA256)；绝不是密码本身 |
| `api.addr` | 从本机访问 API 用的地址，每次启动时写入；各条命令会读它 |
| `lock` | 空文件；节点运行期间锁着它 |

- **一个数据目录上同一时间只能运行一个节点。** 在同一个目录上再执行一次 `tcpraid run` 会报错退出，什么都不会改变。节点运行期间不要删除 `lock` 文件：它不是残留物，没有它，第二个节点就可能在这个目录上启动起来。
- **这个目录就是节点本身。** 只要 `identity.key` 还在，节点的指纹就不变，所以更换程序或容器时要保留这个目录。还留着 `state.json`、却丢了两个身份文件的目录会被拒绝：节点不会给自己造一个新身份。
- **像备份私钥那样备份它**，绝不要用同一个目录的副本运行两个节点。
- `state.json` 有格式版本。新版程序能读旧版的文件，并在下一次有改动时按自己的格式写入；它看不懂的文件会被拒绝，并保持原样。

## 端口一览

| 端口(默认或惯用) | 由什么设置 | 用途 |
|---|---|---|
| 9850 TCP | `-api` | 网页界面和本机 API |
| 9851 TCP | `-listen` | 其他节点，TLS |
| 任意 TCP | `-listen-plain` | 其他节点，`off` 档 |
| 任意 TCP | 规则的监听地址 | 应用 |
| 任意 TCP | `-transparent` | 被重定向的连接 |
| 任意 UDP | `-udp-listen` | 对端的回执 |

只有 9850 是程序内置的默认值；9851 和给规则用的 50000-50010 这一段，是容器镜像和 `deploy/` 里的模板所采用的。

## 限制

| | |
|---|---|
| 一个节点为自己的规则承载的会话数 | 64 |
| 一个节点作为出口的会话数 | 64 |
| 规则数 | 256，每条最多覆盖 1024 个端口 |
| 通往一个对端的路径数 | 16，每条 1 到 64 条子连接 |
| 和一个对端之间同时进行的握手数 | 32 |
| 未使用的配对码的有效期 | 1 秒到 30 天，默认 1 小时 |
| 最坏情况下流数据占用的内存 | 约 512 MB |
