# vvae-notifier

[VVAE](https://vvae.app) 推送通知的自部署轮询程序。它在你自己的机器上轮询 V2EX 提醒，加密后经 VVAE 推送到你的 iPhone。

部署、更新、代理、排查与加密说明见官网指南：

- 中文：<https://vvae.app/docs/push-notifications>
- English: <https://vvae.app/en/docs/push-notifications>

## 快速开始

在 VVAE App 的推送设置里复制 Docker 命令，到你的机器上运行即可。

镜像：`ghcr.io/vastlogic-dev/vvae-notifier`（amd64、arm64、arm/v7）。

## 从源码构建

需要 Go 1.24 或更新版本：

```sh
go build -o vvae-notifier .
VVAE_KEY='vvaepush1.…' ./vvae-notifier
```
