FROM golang:1.26.5-alpine AS build

ARG CONCOURSE_VERSION=7.12.1

WORKDIR /src
RUN apk add --no-cache jq
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
    -o /out/homelab-controller ./cmd/homelab-controller
RUN wget -qO- https://api.github.com/meta \
    | jq -r '.ssh_keys[] | "github.com \(.)"' \
    > /out/ssh_known_hosts \
    && test -s /out/ssh_known_hosts
RUN wget -qO /tmp/fly.tgz \
    "https://github.com/concourse/concourse/releases/download/v${CONCOURSE_VERSION}/fly-${CONCOURSE_VERSION}-linux-amd64.tgz" \
    && tar -xzf /tmp/fly.tgz -C /out fly \
    && chmod 0755 /out/fly

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/homelab-controller /homelab-controller
COPY --from=build /out/ssh_known_hosts /etc/ssh/ssh_known_hosts
COPY --from=build /out/fly /usr/local/bin/fly
EXPOSE 8080
ENTRYPOINT ["/homelab-controller"]
