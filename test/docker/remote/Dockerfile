# Удалённый хост туннеля: sshd + python3 (серверная часть sshuttle — питоновская).
FROM alpine:3.20

RUN apk add --no-cache openssh-server python3 && \
    ssh-keygen -A && \
    printf '%s\n' \
      'PermitRootLogin prohibit-password' \
      'PasswordAuthentication no' \
      'AuthorizedKeysFile /root/.ssh/authorized_keys' \
      'UseDNS no' \
      > /etc/ssh/sshd_config.d/e2e.conf

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 22
ENTRYPOINT ["/entrypoint.sh"]
