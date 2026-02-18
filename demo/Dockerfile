FROM alpine:3.19
RUN apk add --no-cache bash ttyd coreutils
COPY cw /usr/local/bin/cw
COPY bashrc /home/demo/.bashrc
COPY showcase.sh /home/demo/showcase.sh
ENV HOME=/home/demo
RUN adduser -D -u 1000 demo && \
    mkdir -p /home/demo/.codewire && \
    chown -R demo:demo /home/demo && \
    chmod +x /home/demo/showcase.sh
USER demo
WORKDIR /home/demo
EXPOSE 7681
CMD ["ttyd", "-R", "-p", "7681", "-t", "fontSize=14", "-t", "theme={'background':'#0d0d0d','foreground':'#c8c8c8','cursor':'#7eb8da'}", "./showcase.sh"]
