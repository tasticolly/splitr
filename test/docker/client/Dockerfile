# Клиент стенда: сборка splitr из репозитория + sshuttle + стаб pfctl.
FROM golang:1.26-bookworm

RUN apt-get update && apt-get install -y --no-install-recommends \
        sshuttle \
        openssh-client \
        iptables \
        iproute2 \
        procps \
        curl \
        python3 \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o /usr/local/bin/splitr ./cmd/splitr

# pf в Linux нет — подменяем ровно тот бинарь, который зовёт демон.
COPY test/docker/client/pfctl-stub.sh /sbin/pfctl
COPY test/docker/client/pf.conf /etc/pf.conf
COPY test/docker/client/config.yaml /etc/splitr/config.yaml
COPY test/docker/client/entrypoint.sh /entrypoint.sh
RUN chmod +x /sbin/pfctl /entrypoint.sh

ENV PFSTUB_DIR=/var/lib/pfstub \
    SPLITR_E2E=1 \
    SPLITR_CONFIG=/etc/splitr/config.yaml

ENTRYPOINT ["/entrypoint.sh"]
CMD ["sleep", "infinity"]
