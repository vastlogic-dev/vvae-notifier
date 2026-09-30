# vvae-notifier

[VVAE](https://vvae.app) 推送通知的自部署轮询程序。它用你自己的网络轮询 V2EX 提醒，
发现新提醒后加密，交给 VVAE 中继推送到你的 iPhone。

- **只用你自己的 IP 请求 V2EX。** 中继从不请求 V2EX。
- **端到端加密。** 密钥在 App 里生成，只有 App 和本程序有。中继只转发密文，看不到提醒内容、你的 V2EX 令牌或提醒源地址。
- **部署一次就不用再管。** 令牌续期、类型过滤、换轮询源，都由 App 加密下发，本程序自动应用。

## 部署

先在 VVAE App 的推送设置里复制部署串（以 `vvaepush1.` 开头）。

### Docker

```sh
docker run -d --name vvae-notifier --restart=always \
  -v vvae-notifier:/data \
  -e VVAE_KEY='vvaepush1.…' \
  ghcr.io/vastlogic-dev/vvae-notifier:latest
```

有多个 V2EX 账号时，每个账号一个部署串，放在同一个 `VVAE_KEY` 里用逗号隔开，一个容器全部轮询。
App 推送设置里「复制 Docker 命令」给出的就是包含所有账号的完整命令，它会先删掉同名的旧容器再启动。
命令里的 `--hostname "$(hostname)"` 让容器沿用宿主机的名字，App 里能看出轮询程序跑在哪台机器上；
同一个部署串在几处运行时，每条提醒会重复推送，App 会显示「N 个轮询程序在运行」并列出各自的机器。

或用 [docker-compose.yml](docker-compose.yml)：把部署串写进同目录的 `.env`（`VVAE_KEY=vvaepush1.…`），再 `docker compose up -d`。

镜像约 7MB，支持 amd64、arm64、arm/v7，以非 root 用户（uid 65532）运行。

`/data` 存去重状态和加密后的配置，每个部署串一个 `state-<标识>.json`（标识是推送 key 哈希的前 8 位，日志每行开头也带它）。删掉会重新记基线（不会重复推送历史提醒）。
挂载宿主机目录（如 NAS 上的 `-v /volume1/docker/vvae:/data`）时，先把目录属主改成 65532：

```sh
sudo chown 65532:65532 /volume1/docker/vvae
```

### 直接运行二进制

需要 Go 1.24 或更新版本，没有第三方依赖：

```sh
go build -o vvae-notifier .
VVAE_KEY='vvaepush1.…' ./vvae-notifier
```

状态默认存在 `./data`，可用 `DATA_DIR` 修改。

## 环境变量

| 变量 | 必填 | 说明 |
|---|---|---|
| `VVAE_KEY` | 是 | App 给的部署串；多个账号时用逗号隔开 |
| `DATA_DIR` | 否 | 状态目录，镜像里默认 `/data` |
| `HTTPS_PROXY` | 否 | 访问 V2EX 和中继要走代理时设置，支持 `http://`、`https://`、`socks5://`（域名由代理解析），可带 `用户名:密码@`；`NO_PROXY` 可排除地址 |

代理跑在宿主机上时，容器里的 `127.0.0.1` 是容器自己，不是宿主机：Docker Desktop（Mac、Windows）写 `host.docker.internal`，
Linux 写宿主机的局域网 IP 或加 `--network host`。例如 `-e HTTPS_PROXY=socks5://host.docker.internal:1080`。

## 加密与隐私

- 部署串里有三样东西：中继地址、推送 key、加密密钥。推送 key 标识你的推送通道，也是访问中继的凭据；加密密钥只在 App 和本程序里。部署串相当于密码，不要分享。
- 提醒内容、配置（含 V2EX 令牌、提醒源地址）和机器信息（含主机名）经过中继时都是 AES-256-GCM 密文，每条用 12 字节随机 nonce。
- 中继能看到的只有：
  - 推送 key，连接中继的 IP 地址；
  - 推送的时间、条数和密文大小；
  - 心跳里的明文字段：本程序版本号、已应用的配置版本、运行状态（如正常、令牌失效）、最近一次轮询成功的时间、每次启动随机生成的实例 id。
- `/data` 里的配置同样以密文保存，V2EX 令牌不以明文落盘。

## 它会发出哪些请求

- V2EX：
  - 轮询 `/api/v2/notifications`（默认每 30 秒）或提醒 Feed（默认每 60 秒）。
  - 用 PAT 时，每天查一次 `/api/v2/token` 的过期时间。
  - 请求都带 `If-None-Match`，UA 为 `vvae-notifier/<版本>`。
- VVAE 中继：拉配置、发加密推送、每 5 分钟一次心跳。

## 开发

```sh
go vet ./...
go test ./...
docker build --build-arg VERSION=dev -t vvae-notifier .
```
