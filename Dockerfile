# syntax=docker/dockerfile:1
# 在构建机的原生平台上交叉编译，不用 QEMU；最终镜像只有一个静态二进制和 CA 证书。

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG VERSION=dev
WORKDIR /src
COPY go.mod main.go ./
COPY internal ./internal
RUN if [ "$TARGETARCH" = arm ]; then export GOARM="${TARGETVARIANT#v}"; fi; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/vvae-notifier . \
 && mkdir /out/data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/vvae-notifier /vvae-notifier
# 空的 /data 属于运行用户：命名卷第一次挂载时继承这个属主
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
VOLUME ["/data"]
ENV DATA_DIR=/data
ENTRYPOINT ["/vvae-notifier"]
